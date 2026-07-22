package payload

import (
	"context"
	"fmt"
	"math"
	"net/http"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

const (
	// DefaultMaxExpansionBytes keeps small normalizers from failing on fixed
	// protocol overhead while the proportional allowance protects large bodies.
	DefaultMaxExpansionBytes      int64 = 256 * 1024
	DefaultMaxExpansionRatio            = 1.25
	DefaultAmplificationPolicyID        = "payload.default_max_256k_25pct"
	requestTransformExpansionCode       = "request_transform_expansion_exceeded"
)

// AmplificationOverride is a policy-owned allowance. MaxExpansionRatio is the
// maximum output/input ratio, so 1.50 allows input plus 50 percent. An override
// is valid only when PolicyID and at least one valid limit are present.
type AmplificationOverride struct {
	PolicyID          string  `json:"policy_id,omitempty"`
	MaxExpansionBytes int64   `json:"max_expansion_bytes,omitempty"`
	MaxExpansionRatio float64 `json:"max_expansion_ratio,omitempty"`
}

// AmplificationObservation is metadata-only and never enforces a rejection.
type AmplificationObservation struct {
	InputBytes         int64   `json:"input_bytes"`
	OutputBytes        int64   `json:"output_bytes"`
	ExpansionBytes     int64   `json:"expansion_bytes"`
	AllowedOutputBytes int64   `json:"allowed_output_bytes"`
	Ratio              float64 `json:"ratio"`
	Exceeded           bool    `json:"exceeded"`
	OverrideApplied    bool    `json:"override_applied"`
	PolicyID           string  `json:"policy_id,omitempty"`
}

// ObserveAmplification compares output size against the default allowance or
// an explicit policy override. It reports a breach but never rejects a request.
func ObserveAmplification(inputBytes, outputBytes int64, override AmplificationOverride) AmplificationObservation {
	inputBytes = nonNegativeBytes(inputBytes)
	outputBytes = nonNegativeBytes(outputBytes)
	allowedOutputBytes := defaultAllowedOutputBytes(inputBytes)
	overridePolicyID := normalizeTransformMetadataID(override.PolicyID)
	overrideApplied := overridePolicyID != "" && validAmplificationLimits(override)
	policyID := DefaultAmplificationPolicyID
	if overrideApplied {
		policyID = overridePolicyID
		allowedOutputBytes = overrideAllowedOutputBytes(inputBytes, override)
	}
	expansionBytes, _ := byteDelta(inputBytes, outputBytes)
	ratio := float64(0)
	if inputBytes > 0 {
		ratio = float64(outputBytes) / float64(inputBytes)
	}

	observation := AmplificationObservation{
		InputBytes:         inputBytes,
		OutputBytes:        outputBytes,
		ExpansionBytes:     expansionBytes,
		AllowedOutputBytes: allowedOutputBytes,
		Ratio:              ratio,
		Exceeded:           outputBytes > allowedOutputBytes,
		OverrideApplied:    overrideApplied,
	}
	observation.PolicyID = policyID
	return observation
}

// EnforceRequestTransform records one metadata-only transform stage. Output
// beyond input + max(256 KiB, input*25%) or a named override is rejected unless
// the request context explicitly selects observe mode. This is a post-transform
// guard: it cannot avoid the transform's first allocation, so callers must
// invoke it before retaining copies, retrying, or sending data.
func EnforceRequestTransform(ctx context.Context, stage string, inputBytes, outputBytes int64, override AmplificationOverride) error {
	return EnforceRequestTransformStage(ctx, TransformStageReport{
		Stage:       stage,
		InputBytes:  inputBytes,
		OutputBytes: outputBytes,
	}, override)
}

// EnforceRequestTransformStage is EnforceRequestTransform with complete
// metadata-only stage accounting for callers that already measure duration or
// synthetic bytes.
func EnforceRequestTransformStage(ctx context.Context, stage TransformStageReport, override AmplificationOverride) error {
	stage.Stage = normalizeTransformMetadataID(stage.Stage)
	if stage.Stage == "" {
		stage.Stage = "request_transform"
	}
	observation := ObserveAmplification(stage.InputBytes, stage.OutputBytes, override)
	stage.AppliedPolicies = appendTransformPolicyID(stage.AppliedPolicies, observation.PolicyID)
	stage.InputBytes = observation.InputBytes
	stage.OutputBytes = observation.OutputBytes
	RecordTransformStage(ctx, stage, override)
	if !observation.Exceeded || !amplificationModeEnforces(ctx) {
		return nil
	}

	cause := &requestTransformExpansionError{
		PolicyID:           observation.PolicyID,
		Stage:              stage.Stage,
		InputBytes:         observation.InputBytes,
		OutputBytes:        observation.OutputBytes,
		AllowedOutputBytes: observation.AllowedOutputBytes,
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.InvalidRequest,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusBadRequest,
		ProviderCode:  requestTransformExpansionCode,
		Cause:         cause,
		PublicMessage: "request transformation exceeded safe output bounds",
	}
}

type requestTransformExpansionError struct {
	PolicyID           string
	Stage              string
	InputBytes         int64
	OutputBytes        int64
	AllowedOutputBytes int64
}

func (e *requestTransformExpansionError) Error() string {
	if e == nil {
		return "request transform exceeded output bound"
	}
	return fmt.Sprintf(
		"request transform exceeded output bound: policy_id=%s stage=%s input_bytes=%d output_bytes=%d allowed_output_bytes=%d",
		e.PolicyID,
		e.Stage,
		e.InputBytes,
		e.OutputBytes,
		e.AllowedOutputBytes,
	)
}

func appendTransformPolicyID(values []string, policyID string) []string {
	policyID = normalizeTransformMetadataID(policyID)
	if policyID == "" {
		return values
	}
	for _, value := range values {
		if value == policyID {
			return values
		}
	}
	return append(values, policyID)
}

func validAmplificationLimits(override AmplificationOverride) bool {
	if override.MaxExpansionBytes > 0 {
		return true
	}
	return override.MaxExpansionRatio >= 1 &&
		!math.IsNaN(override.MaxExpansionRatio) &&
		!math.IsInf(override.MaxExpansionRatio, 0)
}

func defaultAllowedOutputBytes(inputBytes int64) int64 {
	percentageExpansion := ceilRatioBytes(inputBytes, DefaultMaxExpansionRatio-1)
	expansionAllowance := max(DefaultMaxExpansionBytes, percentageExpansion)
	return saturatingAddInt64(inputBytes, expansionAllowance)
}

func overrideAllowedOutputBytes(inputBytes int64, override AmplificationOverride) int64 {
	allowedOutputBytes := inputBytes
	if override.MaxExpansionBytes > 0 {
		allowedOutputBytes = saturatingAddInt64(inputBytes, override.MaxExpansionBytes)
	}
	if override.MaxExpansionRatio >= 1 {
		ratioAllowance := ceilRatioBytes(inputBytes, override.MaxExpansionRatio)
		if ratioAllowance > allowedOutputBytes {
			allowedOutputBytes = ratioAllowance
		}
	}
	return allowedOutputBytes
}

func ceilRatioBytes(inputBytes int64, ratio float64) int64 {
	if inputBytes <= 0 || ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0
	}
	value := math.Ceil(float64(inputBytes) * ratio)
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(value)
}

func saturatingAddInt64(left, right int64) int64 {
	if right > math.MaxInt64-left {
		return math.MaxInt64
	}
	return left + right
}
