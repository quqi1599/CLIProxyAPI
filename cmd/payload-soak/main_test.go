package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testCommitSHA = "0123456789abcdef0123456789abcdef01234567"

func TestValidateReleaseConfiguration(t *testing.T) {
	valid := configuration{releaseGate: true, duration: minimumReleaseDuration, expectedCommit: testCommitSHA, chaos: true, responsesWS: true}
	if err := validateReleaseConfiguration(valid); err != nil {
		t.Fatalf("valid release configuration error = %v", err)
	}

	tests := []struct {
		name string
		cfg  configuration
		want string
	}{
		{name: "missing commit", cfg: configuration{releaseGate: true, duration: minimumReleaseDuration, chaos: true, responsesWS: true}, want: "expected-commit is required"},
		{name: "short duration", cfg: configuration{releaseGate: true, duration: minimumReleaseDuration - time.Second, expectedCommit: testCommitSHA, chaos: true, responsesWS: true}, want: "at least 12h"},
		{name: "short commit", cfg: configuration{releaseGate: true, duration: minimumReleaseDuration, expectedCommit: testCommitSHA[:12], chaos: true, responsesWS: true}, want: "full 40-character"},
		{name: "chaos disabled", cfg: configuration{releaseGate: true, duration: minimumReleaseDuration, expectedCommit: testCommitSHA, responsesWS: true}, want: "chaos matrix"},
		{name: "websocket disabled", cfg: configuration{releaseGate: true, duration: minimumReleaseDuration, expectedCommit: testCommitSHA, chaos: true}, want: "WebSocket"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateReleaseConfiguration(test.cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateReleaseConfiguration() error = %v, want %q", err, test.want)
			}
		})
	}

	if err := validateReleaseConfiguration(configuration{releaseGate: false, duration: time.Second}); err != nil {
		t.Fatalf("explicit non-release configuration error = %v", err)
	}
}

func TestVerifyBuildCommitRequiresExactSHA(t *testing.T) {
	if err := verifyBuildCommit(testCommitSHA, strings.ToUpper(testCommitSHA)); err != nil {
		t.Fatalf("case-insensitive exact commit error = %v", err)
	}
	if err := verifyBuildCommit(testCommitSHA, testCommitSHA[:12]); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("short observed commit error = %v", err)
	}
}

func TestSelectPayloadProfileUsesRequiredDistribution(t *testing.T) {
	profiles := [3]payloadProfile{{name: "small"}, {name: "medium"}, {name: "large"}}
	counts := map[string]int{}
	for sequence := uint64(0); sequence < 100; sequence++ {
		counts[selectPayloadProfile(sequence, profiles).name]++
	}
	if counts["small"] != 90 || counts["medium"] != 9 || counts["large"] != 1 {
		t.Fatalf("profile distribution = %#v, want 90/9/1", counts)
	}
}

func TestValidateSoakReportRejectsEarlyCancellationAndMissingProfile(t *testing.T) {
	profiles := [3]payloadProfile{{name: "small"}, {name: "medium"}, {name: "large"}}
	report := soakReport{
		Attempted:          3,
		Succeeded:          3,
		HealthSamples:      1,
		SucceededByProfile: map[string]uint64{"small": 1, "medium": 1, "large": 1},
		Resources:          validResourceSummary(),
	}
	if err := validateSoakReport(context.Canceled, report, profiles); err == nil || !strings.Contains(err.Error(), "configured duration") {
		t.Fatalf("early cancellation error = %v", err)
	}

	delete(report.SucceededByProfile, "large")
	report.CompletedConfiguredDuration = true
	if err := validateSoakReport(context.DeadlineExceeded, report, profiles); err == nil || !strings.Contains(err.Error(), "large") {
		t.Fatalf("missing large profile error = %v", err)
	}
}

func TestValidateSoakReportRejectsHungOrIncompleteRequests(t *testing.T) {
	profiles := [3]payloadProfile{{name: "small"}, {name: "medium"}, {name: "large"}}
	report := soakReport{Attempted: 8, Canceled: 8, HealthSamples: 1, SucceededByProfile: map[string]uint64{}, Resources: validResourceSummary()}
	if err := validateSoakReport(context.DeadlineExceeded, report, profiles); err == nil || !strings.Contains(err.Error(), "successful request") {
		t.Fatalf("all-hung error = %v", err)
	}

	report = soakReport{
		Attempted:          4,
		Succeeded:          3,
		HealthSamples:      1,
		SucceededByProfile: map[string]uint64{"small": 1, "medium": 1, "large": 1},
		Resources:          validResourceSummary(),
	}
	if err := validateSoakReport(context.DeadlineExceeded, report, profiles); err == nil || !strings.Contains(err.Error(), "accounting") {
		t.Fatalf("incomplete accounting error = %v", err)
	}
}

func TestValidateSoakReportReportsCommitMismatch(t *testing.T) {
	profiles := [3]payloadProfile{{name: "small"}, {name: "medium"}, {name: "large"}}
	resources := validResourceSummary()
	resources.CommitMismatch = true
	resources.RestartDetected = true
	resources.ExpectedCommit = testCommitSHA
	report := soakReport{
		CompletedConfiguredDuration: true,
		Attempted:                   3,
		Succeeded:                   3,
		HealthSamples:               1,
		SucceededByProfile:          map[string]uint64{"small": 1, "medium": 1, "large": 1},
		Resources:                   resources,
	}
	if err := validateSoakReport(context.DeadlineExceeded, report, profiles); err == nil || !strings.Contains(err.Error(), testCommitSHA) {
		t.Fatalf("commit mismatch error = %v", err)
	}
}

func TestCalculateResourceTrendDetectsHeapOnlyGrowthBeforeMidpoint(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	observations := make([]resourceObservation, 20)
	for index := range observations {
		heap := uint64(100 << 20)
		rss := uint64(200 << 20)
		if index >= 4 {
			heap += uint64(min(index-4, 4)) * (16 << 20)
		}
		observations[index] = resourceObservation{
			at: started.Add(time.Duration(index) * time.Hour),
			point: resourcePoint{
				RSSBytes:      &rss,
				HeapLiveBytes: heap,
				Goroutines:    100,
			},
		}
	}
	trend := calculateResourceTrend(observations)
	if !trend.SustainedGrowth || trend.Samples != 19 || trend.HeapLiveBytesPerHour <= 0 || trend.RSSBytesPerHour == nil || *trend.RSSBytesPerHour != 0 {
		t.Fatalf("trend = %+v, want independent heap growth", trend)
	}
}

func TestCalculateResourceTrendIgnoresWarmupGrowth(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	observations := make([]resourceObservation, 20)
	for index := range observations {
		heap := uint64(100 << 20)
		if index < 3 {
			heap += uint64(index) * (32 << 20)
		} else {
			heap = 164 << 20
		}
		observations[index] = resourceObservation{
			at:    started.Add(time.Duration(index) * 5 * time.Minute),
			point: resourcePoint{HeapLiveBytes: heap, Goroutines: 100},
		}
	}
	if trend := calculateResourceTrend(observations); trend.SustainedGrowth {
		t.Fatalf("warm-up-only trend = %+v, want stable", trend)
	}
}

func TestCalculateResourceTrendDetectsDescriptorAndSocketGrowth(t *testing.T) {
	for _, metric := range []string{"fds", "sockets"} {
		t.Run(metric, func(t *testing.T) {
			started := time.Unix(1_700_000_000, 0)
			observations := make([]resourceObservation, 12)
			for index := range observations {
				fds, sockets := 20, 10
				if metric == "fds" {
					fds += index
				} else {
					sockets += index
				}
				observations[index] = resourceObservation{
					at: started.Add(time.Duration(index) * time.Hour),
					point: resourcePoint{
						HeapLiveBytes: 100 << 20,
						Goroutines:    100,
						OpenFDs:       &fds,
						OpenSockets:   &sockets,
					},
				}
			}
			trend := calculateResourceTrend(observations)
			if !trend.SustainedGrowth {
				t.Fatalf("trend = %+v, want %s growth", trend, metric)
			}
		})
	}
}

func TestRecoveryFailuresCheckEveryAvailableResource(t *testing.T) {
	rss, fds, sockets := uint64(200<<20), 20, 10
	baseline := &resourcePoint{
		RSSBytes:      &rss,
		HeapLiveBytes: 100 << 20,
		Goroutines:    100,
		OpenFDs:       &fds,
		OpenSockets:   &sockets,
	}
	limits := recoveryLimits(baseline)
	snapshot := healthDetailsSnapshot{}
	snapshot.Process.RSSBytes = &rss
	snapshot.Process.HeapLiveBytes = baseline.HeapLiveBytes
	snapshot.Process.Goroutines = baseline.Goroutines
	snapshot.Process.OpenFDs = &fds
	snapshot.Process.OpenSockets = &sockets
	if failures := recoveryFailures(snapshot, limits); len(failures) != 0 {
		t.Fatalf("stable recovery failures = %v", failures)
	}

	snapshot.Process.HeapLiveBytes = limits.AllowedHeapBytes + 1
	snapshot.Process.Goroutines = limits.AllowedGoroutines + 1
	snapshot.Process.RSSBytes = nil
	tooManyFDs := *limits.AllowedOpenFDs + 1
	tooManySockets := *limits.AllowedOpenSockets + 1
	snapshot.Process.OpenFDs = &tooManyFDs
	snapshot.Process.OpenSockets = &tooManySockets
	snapshot.Admission.ActiveWeight = 1
	failures := strings.Join(recoveryFailures(snapshot, limits), " ")
	for _, expected := range []string{"admission", "goroutines", "heap_live_bytes", "rss unavailable", "open_fds", "open_sockets"} {
		if !strings.Contains(failures, expected) {
			t.Fatalf("recovery failures = %q, missing %q", failures, expected)
		}
	}
}

func TestRecoverResourcesRejectsHealthyOverLimitSnapshot(t *testing.T) {
	baseline := healthDetailsSnapshot{}
	baseline.Build.Commit = testCommitSHA
	baseline.Process.StartedAt = "2026-07-22T00:00:00Z"
	baseline.Process.HeapLiveBytes = 64 << 20
	baseline.Process.Goroutines = 10

	stats := &soakStats{}
	stats.recordResourceSnapshot(baseline, time.Now(), true)
	limits := recoveryLimits(stats.firstResourcePoint())
	overLimit := baseline
	overLimit.Process.HeapLiveBytes = limits.AllowedHeapBytes + 1
	overLimit.Admission.ActiveWeight = 1

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("gc") != "1" {
			http.Error(w, "fresh GC snapshot required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(overLimit)
	}))
	t.Cleanup(server.Close)

	cfg := configuration{
		baseURL:          server.URL,
		managementKey:    "test-management-key",
		healthTimeout:    200 * time.Millisecond,
		recoveryTimeout:  200 * time.Millisecond,
		recoveryInterval: 5 * time.Millisecond,
	}
	recoverResources(context.Background(), server.Client(), cfg, stats)

	resources := stats.report().Resources
	recovery := resources.Recovery
	if recovery.Recovered {
		t.Fatal("healthy over-limit snapshot incorrectly passed recovery")
	}
	unmet := strings.Join(recovery.Unmet, " ")
	if !strings.Contains(unmet, "admission") || !strings.Contains(unmet, "heap_live_bytes") {
		t.Fatalf("recovery unmet = %q, want admission and heap limits", unmet)
	}
	if samples := resources.Samples; samples < 2 {
		t.Fatalf("resource samples = %d, want recovery probes to be recorded", samples)
	}
	if resources.Last == nil || resources.Last.HeapLiveBytes <= limits.AllowedHeapBytes || resources.Last.ActiveWeight == 0 {
		t.Fatalf("last recovery sample = %+v, want healthy over-limit snapshot", resources.Last)
	}
	if failures := stats.healthDetailsFailed.Load(); failures != 0 {
		t.Fatalf("health details failures = %d, want recovery deadline excluded", failures)
	}
}

func TestRecordResourceSnapshotDetectsCommitMismatchAndDrift(t *testing.T) {
	stats := &soakStats{expectedCommit: testCommitSHA, resources: resourceSummary{ExpectedCommit: testCommitSHA}}
	snapshot := healthDetailsSnapshot{}
	snapshot.Build.Commit = testCommitSHA[:12]
	snapshot.Process.StartedAt = "2026-07-22T00:00:00Z"
	stats.recordResourceSnapshot(snapshot, time.Now(), true)
	if !stats.resources.CommitMismatch {
		t.Fatal("first unexpected commit did not mark mismatch")
	}

	snapshot.Build.Commit = testCommitSHA
	stats.recordResourceSnapshot(snapshot, time.Now().Add(time.Minute), true)
	if !stats.resources.RestartDetected {
		t.Fatal("commit drift did not mark restart")
	}
}

func TestRecordResourceSnapshotTracksThinkingHistoryPolicy(t *testing.T) {
	stats := &soakStats{}
	snapshot := healthDetailsSnapshot{}
	snapshot.Process.StartedAt = "2026-07-22T00:00:00Z"
	baseline := healthTransformDistribution{}
	baseline.DurationBuckets.Samples = 7
	snapshot.Transforms.PolicyCatalog = map[string]healthTransformDistribution{thinkingHistoryPolicyID: baseline}
	stats.recordResourceSnapshot(snapshot, time.Now(), true)

	after := baseline
	after.DurationBuckets.Samples = 9
	snapshot.Transforms.PolicyCatalog[thinkingHistoryPolicyID] = after
	stats.recordResourceSnapshot(snapshot, time.Now().Add(time.Minute), true)

	evidence := stats.report().ThinkingHistory
	if evidence.BaselinePolicySamples != 7 || evidence.LatestPolicySamples != 9 || !evidence.Observed {
		t.Fatalf("thinking-history evidence = %+v", evidence)
	}
}

func TestValidateReleaseSoakRequiresThinkingHistoryPolicyEvidence(t *testing.T) {
	profiles := [3]payloadProfile{{name: "small"}, {name: "medium"}, {name: "large"}}
	report := soakReport{
		CompletedConfiguredDuration: true,
		ReleaseGate:                 true,
		Attempted:                   3,
		Succeeded:                   3,
		HealthSamples:               1,
		SucceededByProfile:          map[string]uint64{"small": 1, "medium": 1, "large": 1},
		Resources:                   validResourceSummary(),
		ScenarioMatrixRuns:          2,
		Scenarios:                   make(map[string]scenarioResult, len(requiredReleaseScenarios)),
	}
	for _, name := range requiredReleaseScenarios {
		report.Scenarios[name] = scenarioResult{Expected: expectedScenarioOutcome(name), Attempts: 2, Succeeded: 2}
	}
	if err := validateSoakReport(context.DeadlineExceeded, report, profiles); err == nil || !strings.Contains(err.Error(), "thinking-history") {
		t.Fatalf("missing thinking-history evidence error = %v", err)
	}
	report.ThinkingHistory = transformEvidence{BaselinePolicySamples: 4, LatestPolicySamples: 5, Observed: true}
	if err := validateSoakReport(context.DeadlineExceeded, report, profiles); err != nil {
		t.Fatalf("valid thinking-history evidence error = %v", err)
	}
}

func validResourceSummary() resourceSummary {
	return resourceSummary{
		Samples: 1,
		Recovery: resourceRecovery{
			Attempted: true,
			Recovered: true,
		},
	}
}

func TestBuildPayloadProfilesStayInsideIngressBudget(t *testing.T) {
	profiles, err := buildPayloadProfiles("fixture-model")
	if err != nil {
		t.Fatalf("buildPayloadProfiles() error = %v", err)
	}
	if len(profiles[0].body) >= len(profiles[1].body) || len(profiles[1].body) >= len(profiles[2].body) {
		t.Fatalf("profile sizes are not increasing: %d, %d, %d", len(profiles[0].body), len(profiles[1].body), len(profiles[2].body))
	}
	if len(profiles[2].body) >= 32<<20 {
		t.Fatalf("large profile size = %d, must remain below JSON ingress ceiling", len(profiles[2].body))
	}
	for index, profile := range profiles {
		if len(profile.body) == 0 {
			t.Fatalf("%s profile is empty", profile.name)
		}
		var payload chatPayload
		if errUnmarshal := json.Unmarshal(profile.body, &payload); errUnmarshal != nil {
			t.Fatalf("parse %s profile: %v", profile.name, errUnmarshal)
		}
		if index == 0 && payload.ReasoningEffort != "" {
			t.Fatalf("small profile reasoning_effort = %q, want empty", payload.ReasoningEffort)
		}
		if index > 0 && payload.ReasoningEffort != "high" {
			t.Fatalf("%s profile reasoning_effort = %q, want high", profile.name, payload.ReasoningEffort)
		}
	}
}
