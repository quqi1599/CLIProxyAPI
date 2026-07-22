package payload

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxTransformMetadataIDBytes = 128
	maxTransformMetadataIDs     = 32
	maxTransformReportObservers = 8
	maxTransformReportStages    = 64
)

// TransformStageReport describes one semantic payload transformation. It must
// contain metadata only; Stage, AppliedPolicies, and Downgrades must be
// controlled identifiers, never request-derived values.
type TransformStageReport struct {
	Stage           string                   `json:"stage"`
	InputBytes      int64                    `json:"input_bytes"`
	OutputBytes     int64                    `json:"output_bytes"`
	AddedBytes      int64                    `json:"added_bytes"`
	RemovedBytes    int64                    `json:"removed_bytes"`
	SyntheticBytes  int64                    `json:"synthetic_bytes"`
	Duration        time.Duration            `json:"duration"`
	AppliedPolicies []string                 `json:"applied_policies,omitempty"`
	Downgrades      []string                 `json:"downgrades,omitempty"`
	ReusedInput     bool                     `json:"reused_input"`
	Amplification   AmplificationObservation `json:"amplification"`
}

// TransformReport summarizes payload transformation cost without retaining
// prompt, reasoning, tool output, image, credential, or other body content.
type TransformReport struct {
	Instrumented   bool  `json:"instrumented"`
	Finalized      bool  `json:"finalized"`
	WireInputBytes int64 `json:"wire_input_bytes"`
	// InputBytes is the decoded request size retained for compatibility.
	InputBytes         int64                    `json:"input_bytes"`
	OutputBytes        int64                    `json:"output_bytes"`
	AddedBytes         int64                    `json:"added_bytes"`
	RemovedBytes       int64                    `json:"removed_bytes"`
	SyntheticBytes     int64                    `json:"synthetic_bytes"`
	Duration           time.Duration            `json:"duration"`
	Stages             []TransformStageReport   `json:"stages,omitempty"`
	FinalAmplification AmplificationObservation `json:"final_amplification"`
}

type transformReportContextKey struct{}

type transformReportAccumulator struct {
	mu        sync.Mutex
	report    TransformReport
	observers []TransformReportObserver
	finalized bool
	active    int
	observed  bool
}

// TransformReportObserver receives one metadata-only report snapshot after the
// final retained execution scope is released.
type TransformReportObserver func(TransformReport)

// TransformMetrics is the low-cardinality process aggregate exported through
// diagnostics. It contains counters only and never request payload data.
type TransformMetrics struct {
	Reports               uint64                           `json:"reports"`
	InstrumentedReports   uint64                           `json:"instrumented_reports"`
	FinalizedReports      uint64                           `json:"finalized_reports"`
	UninstrumentedReports uint64                           `json:"uninstrumented_reports"`
	UnfinalizedReports    uint64                           `json:"unfinalized_reports"`
	Stages                uint64                           `json:"stages"`
	ExceededReports       uint64                           `json:"exceeded_reports"`
	ExceededStages        uint64                           `json:"exceeded_stages"`
	WireInputBytes        uint64                           `json:"wire_input_bytes"`
	InputBytes            uint64                           `json:"input_bytes"`
	OutputBytes           uint64                           `json:"output_bytes"`
	SyntheticBytes        uint64                           `json:"synthetic_bytes"`
	TransformNanoseconds  uint64                           `json:"transform_nanoseconds"`
	ReportDistribution    TransformDistribution            `json:"report_distribution"`
	StageCatalog          map[string]TransformDistribution `json:"stage_catalog"`
	PolicyCatalog         map[string]TransformDistribution `json:"policy_catalog"`
}

var transformMetrics struct {
	reports               atomic.Uint64
	instrumentedReports   atomic.Uint64
	finalizedReports      atomic.Uint64
	uninstrumentedReports atomic.Uint64
	unfinalizedReports    atomic.Uint64
	stages                atomic.Uint64
	exceededReports       atomic.Uint64
	exceededStages        atomic.Uint64
	wireInputBytes        atomic.Uint64
	inputBytes            atomic.Uint64
	outputBytes           atomic.Uint64
	syntheticBytes        atomic.Uint64
	transformNanoseconds  atomic.Uint64
}

// WithTransformReport attaches an empty request-scoped transform report. An
// existing report is preserved so nested execution paths share one summary.
// The legacy input size is the decoded request size; wire size remains unknown.
func WithTransformReport(ctx context.Context, inputBytes int64) context.Context {
	return WithTransformReportBytes(ctx, 0, inputBytes)
}

// WithTransformReportBytes attaches an empty request-scoped transform report
// with separate wire and decoded request sizes.
func WithTransformReportBytes(ctx context.Context, wireInputBytes, decodedInputBytes int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if transformReportAccumulatorFromContext(ctx) != nil {
		return ctx
	}
	wireInputBytes = nonNegativeBytes(wireInputBytes)
	decodedInputBytes = nonNegativeBytes(decodedInputBytes)
	return context.WithValue(ctx, transformReportContextKey{}, &transformReportAccumulator{
		report: TransformReport{
			WireInputBytes:     wireInputBytes,
			InputBytes:         decodedInputBytes,
			OutputBytes:        decodedInputBytes,
			FinalAmplification: ObserveAmplification(decodedInputBytes, decodedInputBytes, AmplificationOverride{}),
		},
	})
}

// AddTransformReportObserver registers a bounded request-scoped observer. It
// returns false when no report exists, the observer is nil, the report was
// already published, or the observer limit was reached.
func AddTransformReportObserver(ctx context.Context, observer TransformReportObserver) bool {
	accumulator := transformReportAccumulatorFromContext(ctx)
	if accumulator == nil || observer == nil {
		return false
	}

	accumulator.mu.Lock()
	defer accumulator.mu.Unlock()
	if accumulator.observed || len(accumulator.observers) >= maxTransformReportObservers {
		return false
	}
	accumulator.observers = append(accumulator.observers, observer)
	return true
}

// RecordTransformStage appends stage metadata to the request report. Added and
// removed bytes are derived from the input/output sizes to keep accounting
// consistent across callers.
func RecordTransformStage(ctx context.Context, stage TransformStageReport, override AmplificationOverride) {
	accumulator := transformReportAccumulatorFromContext(ctx)
	if accumulator == nil {
		return
	}
	stage.Stage = normalizeTransformMetadataID(stage.Stage)
	if stage.Stage == "" {
		return
	}
	stage.InputBytes = nonNegativeBytes(stage.InputBytes)
	stage.OutputBytes = nonNegativeBytes(stage.OutputBytes)
	stage.SyntheticBytes = nonNegativeBytes(stage.SyntheticBytes)
	if stage.Duration < 0 {
		stage.Duration = 0
	}
	stage.AddedBytes, stage.RemovedBytes = byteDelta(stage.InputBytes, stage.OutputBytes)
	stage.AppliedPolicies = normalizeTransformMetadataIDs(stage.AppliedPolicies)
	stage.Downgrades = normalizeTransformMetadataIDs(stage.Downgrades)
	stage.Amplification = ObserveAmplification(stage.InputBytes, stage.OutputBytes, override)

	accumulator.mu.Lock()
	defer accumulator.mu.Unlock()
	if accumulator.finalized || len(accumulator.report.Stages) >= maxTransformReportStages {
		return
	}
	accumulator.report.Stages = append(accumulator.report.Stages, stage)
	accumulator.report.Instrumented = true
	accumulator.report.OutputBytes = stage.OutputBytes
	accumulator.report.AddedBytes = saturatingAddInt64(accumulator.report.AddedBytes, stage.AddedBytes)
	accumulator.report.RemovedBytes = saturatingAddInt64(accumulator.report.RemovedBytes, stage.RemovedBytes)
	accumulator.report.SyntheticBytes = saturatingAddInt64(accumulator.report.SyntheticBytes, stage.SyntheticBytes)
	accumulator.report.Duration = time.Duration(saturatingAddInt64(int64(accumulator.report.Duration), int64(stage.Duration)))
	accumulator.report.FinalAmplification = ObserveAmplification(
		accumulator.report.InputBytes,
		stage.OutputBytes,
		AmplificationOverride{},
	)
}

// RecordTransformStageSince records a stage and measures its elapsed time.
func RecordTransformStageSince(ctx context.Context, stage TransformStageReport, started time.Time, override AmplificationOverride) {
	if !started.IsZero() {
		stage.Duration = time.Since(started)
	}
	RecordTransformStage(ctx, stage, override)
}

// FinalizeTransformReport records the final output size and applies an optional
// policy-specific amplification allowance. It remains observe-only.
func FinalizeTransformReport(ctx context.Context, outputBytes int64, override AmplificationOverride) {
	accumulator := transformReportAccumulatorFromContext(ctx)
	if accumulator == nil {
		return
	}
	outputBytes = nonNegativeBytes(outputBytes)

	accumulator.mu.Lock()
	defer accumulator.mu.Unlock()
	if accumulator.finalized {
		return
	}
	accumulator.report.OutputBytes = outputBytes
	accumulator.report.Instrumented = true
	accumulator.report.Finalized = true
	accumulator.report.FinalAmplification = ObserveAmplification(
		accumulator.report.InputBytes,
		outputBytes,
		override,
	)
	accumulator.finalized = true
}

// TransformReportFromContext returns an immutable snapshot of the request
// report. Slice fields are copied so callers cannot mutate future snapshots.
func TransformReportFromContext(ctx context.Context) (TransformReport, bool) {
	accumulator := transformReportAccumulatorFromContext(ctx)
	if accumulator == nil {
		return TransformReport{}, false
	}

	accumulator.mu.Lock()
	defer accumulator.mu.Unlock()
	report := accumulator.report
	report.Stages = cloneTransformStages(report.Stages)
	return report, true
}

// HasTransformReport reports whether ctx already carries request-scoped
// transform accounting without cloning the current stage snapshot.
func HasTransformReport(ctx context.Context) bool {
	return transformReportAccumulatorFromContext(ctx) != nil
}

// RetainTransformReport returns a release function for one execution scope.
// The final scope release seals the report and publishes one process aggregate.
func RetainTransformReport(ctx context.Context) func() {
	accumulator := transformReportAccumulatorFromContext(ctx)
	if accumulator == nil {
		return func() {}
	}
	accumulator.mu.Lock()
	if accumulator.observed {
		accumulator.mu.Unlock()
		return func() {}
	}
	accumulator.active++
	accumulator.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			var (
				report    TransformReport
				observers []TransformReportObserver
			)
			publish := false
			accumulator.mu.Lock()
			if accumulator.active > 0 {
				accumulator.active--
			}
			if accumulator.active == 0 && !accumulator.observed {
				if !accumulator.finalized {
					accumulator.report.FinalAmplification = ObserveAmplification(
						accumulator.report.InputBytes,
						accumulator.report.OutputBytes,
						AmplificationOverride{},
					)
					accumulator.report.Finalized = accumulator.report.Instrumented
					accumulator.finalized = true
				}
				accumulator.observed = true
				report = accumulator.report
				report.Stages = cloneTransformStages(report.Stages)
				observers = append([]TransformReportObserver(nil), accumulator.observers...)
				accumulator.observers = nil
				publish = true
			}
			accumulator.mu.Unlock()
			if publish {
				observeTransformMetrics(report)
				notifyTransformReportObservers(observers, report)
			}
		})
	}
}

// CurrentTransformMetrics returns a process snapshot.
func CurrentTransformMetrics() TransformMetrics {
	reports, stages, policies := currentTransformDistributions()
	return TransformMetrics{
		Reports:               transformMetrics.reports.Load(),
		InstrumentedReports:   transformMetrics.instrumentedReports.Load(),
		FinalizedReports:      transformMetrics.finalizedReports.Load(),
		UninstrumentedReports: transformMetrics.uninstrumentedReports.Load(),
		UnfinalizedReports:    transformMetrics.unfinalizedReports.Load(),
		Stages:                transformMetrics.stages.Load(),
		ExceededReports:       transformMetrics.exceededReports.Load(),
		ExceededStages:        transformMetrics.exceededStages.Load(),
		WireInputBytes:        transformMetrics.wireInputBytes.Load(),
		InputBytes:            transformMetrics.inputBytes.Load(),
		OutputBytes:           transformMetrics.outputBytes.Load(),
		SyntheticBytes:        transformMetrics.syntheticBytes.Load(),
		TransformNanoseconds:  transformMetrics.transformNanoseconds.Load(),
		ReportDistribution:    reports,
		StageCatalog:          stages,
		PolicyCatalog:         policies,
	}
}

func observeTransformMetrics(report TransformReport) {
	transformMetrics.reports.Add(1)
	transformMetrics.stages.Add(uint64(len(report.Stages)))
	transformMetrics.wireInputBytes.Add(uint64(nonNegativeBytes(report.WireInputBytes)))
	transformMetrics.inputBytes.Add(uint64(nonNegativeBytes(report.InputBytes)))
	transformMetrics.syntheticBytes.Add(uint64(nonNegativeBytes(report.SyntheticBytes)))
	transformMetrics.transformNanoseconds.Add(uint64(max(int64(report.Duration), 0)))
	if report.Instrumented {
		transformMetrics.instrumentedReports.Add(1)
	} else {
		transformMetrics.uninstrumentedReports.Add(1)
	}
	if report.Finalized {
		transformMetrics.finalizedReports.Add(1)
		transformMetrics.outputBytes.Add(uint64(nonNegativeBytes(report.OutputBytes)))
		if report.FinalAmplification.Exceeded {
			transformMetrics.exceededReports.Add(1)
		}
	} else {
		transformMetrics.unfinalizedReports.Add(1)
	}
	for _, stage := range report.Stages {
		if stage.Amplification.Exceeded {
			transformMetrics.exceededStages.Add(1)
		}
	}
	observeTransformDistributions(report)
}

func notifyTransformReportObservers(observers []TransformReportObserver, report TransformReport) {
	for _, observer := range observers {
		func() {
			defer func() {
				_ = recover()
			}()
			observer(cloneTransformReport(report))
		}()
	}
}

func transformReportAccumulatorFromContext(ctx context.Context) *transformReportAccumulator {
	if ctx == nil {
		return nil
	}
	accumulator, _ := ctx.Value(transformReportContextKey{}).(*transformReportAccumulator)
	return accumulator
}

func cloneTransformStages(stages []TransformStageReport) []TransformStageReport {
	if len(stages) == 0 {
		return nil
	}
	out := make([]TransformStageReport, len(stages))
	copy(out, stages)
	for idx := range out {
		out[idx].AppliedPolicies = append([]string(nil), stages[idx].AppliedPolicies...)
		out[idx].Downgrades = append([]string(nil), stages[idx].Downgrades...)
	}
	return out
}

func cloneTransformReport(report TransformReport) TransformReport {
	report.Stages = cloneTransformStages(report.Stages)
	return report
}

func byteDelta(inputBytes, outputBytes int64) (addedBytes, removedBytes int64) {
	if outputBytes >= inputBytes {
		return outputBytes - inputBytes, 0
	}
	return 0, inputBytes - outputBytes
}

func nonNegativeBytes(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func normalizeTransformMetadataIDs(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	capacity := min(len(values), maxTransformMetadataIDs)
	out := make([]string, 0, capacity)
	for _, value := range values {
		if len(out) == maxTransformMetadataIDs {
			break
		}
		if value = normalizeTransformMetadataID(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func normalizeTransformMetadataID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > maxTransformMetadataIDBytes {
		return ""
	}
	for idx := range len(value) {
		char := value[idx]
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '.', '_', '-', ':', '/', '@', '+':
			continue
		default:
			return ""
		}
	}
	return strings.Clone(value)
}
