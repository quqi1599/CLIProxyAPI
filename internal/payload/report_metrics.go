package payload

import (
	"math"
	"strings"
	"sync"
	"time"
)

const (
	transformMetricOther = "other"

	transformStageCompat         = "compat"
	transformStageNormalize      = "normalize"
	transformStageRequestPlan    = "request_plan"
	transformStageRequestPrepare = "request_prepare"
	transformStageRequest        = "request_transform"

	transformPolicyThinkingBudget      = "thinking_history.synthetic_budget"
	transformPolicyThinkingPlaceholder = "thinking_history.placeholder"
	transformPolicyCodexReplay         = "codex.reasoning_replay"
	transformPolicyAntigravityReplay   = "antigravity.reasoning_replay"
	transformPolicyMinimaxImage        = "openai_compat.minimax_m3_image_inline"
	transformPolicyOpenAICompatKimi    = "openai_compat.kimi.model_quirks"
	transformPolicyOpenAICompatMiniMax = "openai_compat.minimax.request_quirks"
	transformPolicyOpenAICompatQwen38  = "openai_compat.qwen38.thinking"
)

var (
	transformStageCatalog = [...]string{
		transformStageCompat,
		transformStageNormalize,
		transformStageRequestPlan,
		transformStageRequestPrepare,
		transformStageRequest,
		transformMetricOther,
	}
	transformPolicyCatalog = [...]string{
		DefaultAmplificationPolicyID,
		transformPolicyThinkingBudget,
		transformPolicyThinkingPlaceholder,
		transformPolicyCodexReplay,
		transformPolicyAntigravityReplay,
		transformPolicyMinimaxImage,
		transformPolicyOpenAICompatKimi,
		transformPolicyOpenAICompatMiniMax,
		transformPolicyOpenAICompatQwen38,
		transformMetricOther,
	}
)

// TransformResultCounters separates transforms that stayed within their
// declared output allowance from transforms that exceeded it.
type TransformResultCounters struct {
	WithinLimit uint64 `json:"within_limit"`
	Exceeded    uint64 `json:"exceeded"`
}

// TransformSizeBuckets is a cumulative histogram using the PRD size
// boundaries. Overflow contains values above 64 MiB.
type TransformSizeBuckets struct {
	Samples               uint64 `json:"samples"`
	LessThanOrEqual64KiB  uint64 `json:"le_64_kib"`
	LessThanOrEqual256KiB uint64 `json:"le_256_kib"`
	LessThanOrEqual1MiB   uint64 `json:"le_1_mib"`
	LessThanOrEqual4MiB   uint64 `json:"le_4_mib"`
	LessThanOrEqual16MiB  uint64 `json:"le_16_mib"`
	LessThanOrEqual64MiB  uint64 `json:"le_64_mib"`
	Overflow              uint64 `json:"overflow"`
}

// TransformDurationBuckets is a cumulative fixed-boundary histogram.
type TransformDurationBuckets struct {
	Samples                  uint64 `json:"samples"`
	LessThanOrEqual100Micros uint64 `json:"le_100_us"`
	LessThanOrEqual1Millis   uint64 `json:"le_1_ms"`
	LessThanOrEqual10Millis  uint64 `json:"le_10_ms"`
	LessThanOrEqual100Millis uint64 `json:"le_100_ms"`
	LessThanOrEqualOneSecond uint64 `json:"le_1_s"`
	GreaterThanOneSecond     uint64 `json:"gt_1_s"`
}

// TransformRatioBuckets is a cumulative fixed-boundary amplification
// histogram. Overflow contains ratios above 4x, including non-zero output
// produced from empty input.
type TransformRatioBuckets struct {
	Samples              uint64 `json:"samples"`
	LessThanOrEqualOne   uint64 `json:"le_1x"`
	LessThanOrEqualOne25 uint64 `json:"le_1_25x"`
	LessThanOrEqualOne50 uint64 `json:"le_1_5x"`
	LessThanOrEqualTwo   uint64 `json:"le_2x"`
	LessThanOrEqualFour  uint64 `json:"le_4x"`
	Overflow             uint64 `json:"overflow"`
}

// TransformDistribution contains fixed, metadata-only histograms for one
// controlled stage family or policy ID.
type TransformDistribution struct {
	Results             TransformResultCounters  `json:"results"`
	InputSizeBuckets    TransformSizeBuckets     `json:"input_size_buckets"`
	OutputSizeBuckets   TransformSizeBuckets     `json:"output_size_buckets"`
	DurationBuckets     TransformDurationBuckets `json:"duration_buckets"`
	AmplificationRatios TransformRatioBuckets    `json:"amplification_ratio_buckets"`
}

var transformDistributionMetrics = struct {
	mu       sync.Mutex
	reports  TransformDistribution
	stages   map[string]TransformDistribution
	policies map[string]TransformDistribution
}{
	stages:   newTransformDistributionCatalog(transformStageCatalog[:]),
	policies: newTransformDistributionCatalog(transformPolicyCatalog[:]),
}

func newTransformDistributionCatalog(ids []string) map[string]TransformDistribution {
	catalog := make(map[string]TransformDistribution, len(ids))
	for _, id := range ids {
		catalog[id] = TransformDistribution{}
	}
	return catalog
}

func currentTransformDistributions() (TransformDistribution, map[string]TransformDistribution, map[string]TransformDistribution) {
	transformDistributionMetrics.mu.Lock()
	defer transformDistributionMetrics.mu.Unlock()
	return transformDistributionMetrics.reports,
		cloneTransformDistributionCatalog(transformDistributionMetrics.stages),
		cloneTransformDistributionCatalog(transformDistributionMetrics.policies)
}

func cloneTransformDistributionCatalog(source map[string]TransformDistribution) map[string]TransformDistribution {
	clone := make(map[string]TransformDistribution, len(source))
	for id, distribution := range source {
		clone[id] = distribution
	}
	return clone
}

func observeTransformDistributions(report TransformReport) {
	transformDistributionMetrics.mu.Lock()
	defer transformDistributionMetrics.mu.Unlock()

	observeTransformDistribution(
		&transformDistributionMetrics.reports,
		report.InputBytes,
		report.OutputBytes,
		report.Duration,
		report.FinalAmplification,
	)
	for _, stage := range report.Stages {
		family := transformStageCatalogID(stage.Stage)
		distribution := transformDistributionMetrics.stages[family]
		observeTransformDistribution(&distribution, stage.InputBytes, stage.OutputBytes, stage.Duration, stage.Amplification)
		transformDistributionMetrics.stages[family] = distribution

		for _, policyID := range stage.AppliedPolicies {
			policyID = transformPolicyCatalogID(policyID)
			distribution = transformDistributionMetrics.policies[policyID]
			observeTransformDistribution(&distribution, stage.InputBytes, stage.OutputBytes, stage.Duration, stage.Amplification)
			transformDistributionMetrics.policies[policyID] = distribution
		}
	}
}

func transformStageCatalogID(stage string) string {
	for _, family := range transformStageCatalog[:len(transformStageCatalog)-1] {
		if stage == family || strings.HasPrefix(stage, family+".") || strings.HasPrefix(stage, family+"/") {
			return family
		}
	}
	return transformMetricOther
}

func transformPolicyCatalogID(policyID string) string {
	for _, known := range transformPolicyCatalog[:len(transformPolicyCatalog)-1] {
		if policyID == known {
			return known
		}
	}
	return transformMetricOther
}

func observeTransformDistribution(distribution *TransformDistribution, inputBytes, outputBytes int64, duration time.Duration, amplification AmplificationObservation) {
	if amplification.Exceeded {
		distribution.Results.Exceeded++
	} else {
		distribution.Results.WithinLimit++
	}
	observeTransformSizeBucket(&distribution.InputSizeBuckets, nonNegativeBytes(inputBytes))
	observeTransformSizeBucket(&distribution.OutputSizeBuckets, nonNegativeBytes(outputBytes))
	observeTransformDurationBucket(&distribution.DurationBuckets, max(duration, 0))
	observeTransformRatioBucket(&distribution.AmplificationRatios, inputBytes, outputBytes, amplification.Ratio)
}

func observeTransformSizeBucket(buckets *TransformSizeBuckets, sizeBytes int64) {
	buckets.Samples++
	if sizeBytes <= 64<<10 {
		buckets.LessThanOrEqual64KiB++
	}
	if sizeBytes <= 256<<10 {
		buckets.LessThanOrEqual256KiB++
	}
	if sizeBytes <= 1<<20 {
		buckets.LessThanOrEqual1MiB++
	}
	if sizeBytes <= 4<<20 {
		buckets.LessThanOrEqual4MiB++
	}
	if sizeBytes <= 16<<20 {
		buckets.LessThanOrEqual16MiB++
	}
	if sizeBytes <= 64<<20 {
		buckets.LessThanOrEqual64MiB++
		return
	}
	buckets.Overflow++
}

func observeTransformDurationBucket(buckets *TransformDurationBuckets, duration time.Duration) {
	buckets.Samples++
	if duration <= 100*time.Microsecond {
		buckets.LessThanOrEqual100Micros++
	}
	if duration <= time.Millisecond {
		buckets.LessThanOrEqual1Millis++
	}
	if duration <= 10*time.Millisecond {
		buckets.LessThanOrEqual10Millis++
	}
	if duration <= 100*time.Millisecond {
		buckets.LessThanOrEqual100Millis++
	}
	if duration <= time.Second {
		buckets.LessThanOrEqualOneSecond++
		return
	}
	buckets.GreaterThanOneSecond++
}

func observeTransformRatioBucket(buckets *TransformRatioBuckets, inputBytes, outputBytes int64, ratio float64) {
	buckets.Samples++
	if inputBytes <= 0 && outputBytes > 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		buckets.Overflow++
		return
	}
	if ratio <= 1 {
		buckets.LessThanOrEqualOne++
	}
	if ratio <= 1.25 {
		buckets.LessThanOrEqualOne25++
	}
	if ratio <= 1.5 {
		buckets.LessThanOrEqualOne50++
	}
	if ratio <= 2 {
		buckets.LessThanOrEqualTwo++
	}
	if ratio <= 4 {
		buckets.LessThanOrEqualFour++
		return
	}
	buckets.Overflow++
}
