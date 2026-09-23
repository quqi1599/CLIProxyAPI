package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestShortGPTAvailabilityClassificationAcrossSelectionPaths(t *testing.T) {
	t.Parallel()
	const model = "gpt-5.6-sol"
	for _, path := range []string{"round-robin", "spread", "scheduler-single", "scheduler-mixed"} {
		for _, tc := range []struct {
			name       string
			statuses   []int
			healthOnly bool
			staleQuota bool
			wantStatus int
		}{
			{"auth401", []int{401}, false, false, 503},
			{"auth403", []int{403}, false, false, 503},
			{"auth429", []int{429}, false, false, 429},
			{"health401", []int{401}, true, false, 503},
			{"health502", []int{502}, true, false, 503},
			{"health429", []int{429}, true, false, 429},
			{"stale_quota401", []int{401}, false, true, 503},
			{"mixed401_429", []int{401, 429}, false, false, 503},
			{"mixed502_429", []int{502, 429}, true, false, 503},
			{"disabled_no_recovery", []int{0}, false, false, 503},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				auths := make([]*Auth, 0, len(tc.statuses))
				for i, status := range tc.statuses {
					provider := "codex"
					if path == "scheduler-mixed" && i%2 == 1 {
						provider = "openai-compatibility"
					}
					auth := &Auth{ID: fmt.Sprintf("%s/%d", t.Name(), i), Provider: provider, Status: StatusActive,
						Attributes: map[string]string{AttributeAPIKey: "test-key", "base_url": "https://example.invalid"}}
					switch {
					case status == 0:
						auth.Status = StatusDisabled
					case tc.healthOnly:
						auth.Health = HealthState{Observed: true, BreakerState: HealthBreakerOpen,
							OpenUntil: time.Now().Add(2 * time.Minute), LastStatusCode: status}
					default:
						auth.Unavailable = true
						auth.NextRetryAfter = time.Now().Add(2 * time.Minute)
						auth.Quota.Exceeded = status == 429 || tc.staleQuota
						auth.LastError = &Error{HTTPStatus: status}
					}
					registerSchedulerModels(t, provider, model, auth.ID)
					auths = append(auths, auth)
				}
				var selected *Auth
				var err error
				switch path {
				case "round-robin":
					selected, err = (&RoundRobinSelector{}).Pick(context.Background(), "codex", model, cliproxyexecutor.Options{}, auths)
				case "spread":
					selected, err = (&SpreadSelector{}).Pick(context.Background(), "codex", model, cliproxyexecutor.Options{}, auths)
				case "scheduler-single":
					selected, err = newSchedulerForTest(&RoundRobinSelector{}, auths...).pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
				case "scheduler-mixed":
					selected, _, err = newSchedulerForTest(&RoundRobinSelector{}, auths...).pickMixed(context.Background(), []string{"codex", "openai-compatibility"}, model, cliproxyexecutor.Options{}, nil)
				}
				if selected != nil || statusCodeFromError(err) != tc.wantStatus {
					t.Fatalf("selected=%v err=%v status=%d, want no selection/%d", selected, err, statusCodeFromError(err), tc.wantStatus)
				}
				terminal := normalizeExhaustedGPTChannelError(err, []string{"codex"}, model)
				if tc.wantStatus == 503 && errorCodeFromError(terminal) != "auth_unavailable" {
					t.Fatalf("terminal=%v, want auth_unavailable", terminal)
				}
				wait := retryAfterFromError(terminal)
				if tc.name == "disabled_no_recovery" {
					if wait != nil {
						t.Fatalf("invented retry delay %v without a recovery time", wait)
					}
				} else if wait == nil || *wait <= 0 || *wait > 2*time.Minute {
					t.Fatalf("retryAfter=%v, want preserved future recovery", wait)
				}
			})
		}
	}
}

func TestNonGPTAvailabilityClassificationAcrossSelectionPaths(t *testing.T) {
	t.Parallel()
	const model = "claude-sonnet-4-6"
	for _, path := range []string{"round-robin", "spread", "scheduler-single", "scheduler-mixed"} {
		for _, status := range []int{403, 429} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				auth := &Auth{ID: t.Name(), Provider: "claude", Status: StatusError, Unavailable: true,
					NextRetryAfter: time.Now().Add(time.Minute), Quota: QuotaState{Exceeded: status == 429}, LastError: &Error{HTTPStatus: status}}
				registerSchedulerModels(t, "claude", model, auth.ID)
				var err error
				switch path {
				case "round-robin":
					_, err = (&RoundRobinSelector{}).Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, []*Auth{auth})
				case "spread":
					_, err = (&SpreadSelector{}).Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, []*Auth{auth})
				case "scheduler-single":
					_, err = newSchedulerForTest(&RoundRobinSelector{}, auth).pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
				case "scheduler-mixed":
					_, _, err = newSchedulerForTest(&RoundRobinSelector{}, auth).pickMixed(context.Background(), []string{"claude", "gemini"}, model, cliproxyexecutor.Options{}, nil)
				}
				want := 503
				if status == 429 {
					want = 429
				}
				if statusCodeFromError(err) != want || retryAfterFromError(err) == nil {
					t.Fatalf("err=%v status=%d, want %d with recovery", err, statusCodeFromError(err), want)
				}
			})
		}
	}
}
