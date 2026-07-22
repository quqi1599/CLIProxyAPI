package compat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

// PolicyExecutionReport contains payload-free accounting for one policy run.
type PolicyExecutionReport struct {
	ID             string                                   `json:"id"`
	Phase          Phase                                    `json:"phase"`
	InputBytes     int64                                    `json:"input_bytes"`
	OutputBytes    int64                                    `json:"output_bytes"`
	SyntheticBytes int64                                    `json:"synthetic_bytes"`
	PatchedCount   int64                                    `json:"patched_count"`
	Duration       time.Duration                            `json:"duration"`
	Downgrades     []string                                 `json:"downgrades,omitempty"`
	ReusedInput    bool                                     `json:"reused_input"`
	Amplification  internalpayload.AmplificationObservation `json:"amplification"`
}

// PhaseExecutionReport groups policy accounting for one fixed pipeline phase.
type PhaseExecutionReport struct {
	Phase          Phase                   `json:"phase"`
	InputBytes     int64                   `json:"input_bytes"`
	OutputBytes    int64                   `json:"output_bytes"`
	SyntheticBytes int64                   `json:"synthetic_bytes"`
	PatchedCount   int64                   `json:"patched_count"`
	Duration       time.Duration           `json:"duration"`
	Downgrades     []string                `json:"downgrades,omitempty"`
	ReusedInput    bool                    `json:"reused_input"`
	Policies       []PolicyExecutionReport `json:"policies"`
}

// PipelineReport is a payload-free execution report.
type PipelineReport struct {
	InputBytes     int64                  `json:"input_bytes"`
	OutputBytes    int64                  `json:"output_bytes"`
	SyntheticBytes int64                  `json:"synthetic_bytes"`
	PatchedCount   int64                  `json:"patched_count"`
	ReusedInput    bool                   `json:"reused_input"`
	Phases         []PhaseExecutionReport `json:"phases"`
}

// PipelineResult is the compatibility output and its metadata-only report.
type PipelineResult struct {
	Payload []byte         `json:"-"`
	Report  PipelineReport `json:"report"`
}

// Pipeline applies one immutable registry in deterministic policy order.
type Pipeline struct {
	registry *Registry
}

// NewPipeline constructs an execution facade suitable for request planners.
func NewPipeline(registry *Registry) *Pipeline {
	return &Pipeline{registry: registry}
}

// Apply runs every matching policy. It clones the caller input once before the
// first policy so policies never need per-stage defensive copies.
func (pipeline *Pipeline) Apply(ctx context.Context, match MatchContext, input []byte) (PipelineResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := PipelineReport{
		InputBytes:  int64(len(input)),
		OutputBytes: int64(len(input)),
		ReusedInput: true,
		Phases:      make([]PhaseExecutionReport, 0, len(Phases())),
	}
	result := PipelineResult{Payload: input, Report: report}
	if pipeline == nil || pipeline.registry == nil {
		return result, nil
	}

	normalizeMatchContext(&match)
	current := input
	owned := false
	for index := range pipeline.registry.policies {
		policy := &pipeline.registry.policies[index]
		if !policyMatchesExecution(policy.Match, match) {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Payload = nil
			result.Report = report
			return result, cancelledFailure(err)
		}
		if !owned {
			current = internalpayload.CloneBytes(input)
			owned = true
			report.ReusedInput = false
		}

		phaseIndex := len(report.Phases) - 1
		if phaseIndex < 0 || report.Phases[phaseIndex].Phase != policy.Phase {
			if phaseIndex >= 0 {
				recordPayloadPhase(ctx, report.Phases[phaseIndex])
			}
			report.Phases = append(report.Phases, PhaseExecutionReport{
				Phase:       policy.Phase,
				InputBytes:  int64(len(current)),
				OutputBytes: int64(len(current)),
				ReusedInput: true,
				Policies:    make([]PolicyExecutionReport, 0, 1),
			})
			phaseIndex++
		}

		policyInput := current
		started := time.Now()
		transformed, err := policy.Apply(ctx, policyInput)
		duration := time.Since(started)
		if err != nil {
			policyReport := newPolicyExecutionReport(*policy, policyInput, policyInput, 0, 0, duration, nil)
			appendPolicyReport(&report, phaseIndex, policyReport)
			recordPayloadPhase(ctx, report.Phases[phaseIndex])
			result.Payload = nil
			result.Report = report
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, cancelledFailure(err)
			}
			return result, transformFailure(policy.ID, "compat_policy_failed", "compatibility transform failed")
		}

		downgrades, ok := declaredDowngrades(*policy, transformed.Downgrades)
		if transformed.SyntheticBytes < 0 || transformed.PatchedCount < 0 || !ok {
			policyReport := newPolicyExecutionReport(*policy, policyInput, policyInput, 0, 0, duration, nil)
			appendPolicyReport(&report, phaseIndex, policyReport)
			recordPayloadPhase(ctx, report.Phases[phaseIndex])
			result.Payload = nil
			result.Report = report
			return result, transformFailure(policy.ID, "compat_metadata_invalid", "compatibility transform produced invalid metadata")
		}
		if transformed.SyntheticBytes > math.MaxInt64-report.SyntheticBytes {
			recordPayloadPhase(ctx, report.Phases[phaseIndex])
			result.Payload = nil
			result.Report = report
			return result, transformFailure(policy.ID, "compat_synthetic_overflow", "compatibility transform synthetic byte count overflowed")
		}
		if transformed.PatchedCount > math.MaxInt64-report.PatchedCount {
			recordPayloadPhase(ctx, report.Phases[phaseIndex])
			result.Payload = nil
			result.Report = report
			return result, transformFailure(policy.ID, "compat_patched_count_overflow", "compatibility transform patched count overflowed")
		}

		policyReport := newPolicyExecutionReport(
			*policy,
			policyInput,
			transformed.Payload,
			transformed.SyntheticBytes,
			transformed.PatchedCount,
			duration,
			downgrades,
		)
		appendPolicyReport(&report, phaseIndex, policyReport)
		report.SyntheticBytes += transformed.SyntheticBytes
		report.PatchedCount += transformed.PatchedCount
		current = transformed.Payload
		report.OutputBytes = int64(len(current))
		amplificationMode, amplificationModeConfigured := internalpayload.AmplificationModeFromContext(ctx)
		if policyReport.Amplification.Exceeded && (!amplificationModeConfigured || amplificationMode == internalpayload.AmplificationModeEnforce) {
			recordPayloadPhase(ctx, report.Phases[phaseIndex])
			result.Payload = nil
			result.Report = report
			return result, transformFailure(policy.ID, "compat_expansion_exceeded", "compatibility transform exceeded its declared output bound")
		}
	}

	if len(report.Phases) > 0 {
		recordPayloadPhase(ctx, report.Phases[len(report.Phases)-1])
	}
	result.Payload = current
	result.Report = report
	return result, nil
}

func newPolicyExecutionReport(policy Policy, input, output []byte, syntheticBytes, patchedCount int64, duration time.Duration, downgrades []string) PolicyExecutionReport {
	override := internalpayload.AmplificationOverride{
		PolicyID:          policy.ID,
		MaxExpansionBytes: policy.Cost.MaxExpansionBytes,
		MaxExpansionRatio: policy.Cost.MaxExpansionRatio,
	}
	return PolicyExecutionReport{
		ID:             policy.ID,
		Phase:          policy.Phase,
		InputBytes:     int64(len(input)),
		OutputBytes:    int64(len(output)),
		SyntheticBytes: syntheticBytes,
		PatchedCount:   patchedCount,
		Duration:       duration,
		Downgrades:     downgrades,
		ReusedInput:    sameBacking(input, output),
		Amplification:  internalpayload.ObserveAmplification(int64(len(input)), int64(len(output)), override),
	}
}

func appendPolicyReport(report *PipelineReport, phaseIndex int, policyReport PolicyExecutionReport) {
	phase := &report.Phases[phaseIndex]
	phase.Policies = append(phase.Policies, policyReport)
	phase.OutputBytes = policyReport.OutputBytes
	phase.SyntheticBytes += policyReport.SyntheticBytes
	phase.PatchedCount += policyReport.PatchedCount
	phase.Duration += policyReport.Duration
	phase.ReusedInput = phase.ReusedInput && policyReport.ReusedInput
	phase.Downgrades = append(phase.Downgrades, policyReport.Downgrades...)
}

func recordPayloadPhase(ctx context.Context, phase PhaseExecutionReport) {
	policyIDs := make([]string, len(phase.Policies))
	for index := range phase.Policies {
		policyIDs[index] = phase.Policies[index].ID
	}
	internalpayload.RecordTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:           "compat/" + string(phase.Phase),
		InputBytes:      phase.InputBytes,
		OutputBytes:     phase.OutputBytes,
		SyntheticBytes:  phase.SyntheticBytes,
		PatchedCount:    phase.PatchedCount,
		Duration:        phase.Duration,
		AppliedPolicies: policyIDs,
		Downgrades:      phase.Downgrades,
		ReusedInput:     phase.ReusedInput,
	}, internalpayload.AmplificationOverride{})
}

func declaredDowngrades(policy Policy, values []string) ([]string, bool) {
	if len(values) == 0 {
		return nil, true
	}
	if len(values) > len(policy.DowngradeIDs) {
		return nil, false
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(policy.DowngradeIDs, value) {
			return nil, false
		}
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result, true
}

func sameBacking(left, right []byte) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	return &left[0] == &right[0]
}

func cancelledFailure(err error) error {
	cause := context.Canceled
	if errors.Is(err, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.Cancelled,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusRequestTimeout,
		ProviderCode:  "request_cancelled",
		Cause:         cause,
		PublicMessage: "request cancelled",
	}
}

func transformFailure(policyID, code, publicMessage string) error {
	return &failurecontract.Failure{
		Kind:          failurecontract.InternalTransformError,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusInternalServerError,
		ProviderCode:  code,
		Cause:         fmt.Errorf("compat policy %s violated its execution contract", policyID),
		PublicMessage: publicMessage,
	}
}
