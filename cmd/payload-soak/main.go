// Command payload-soak runs the mixed payload profile required by the release soak gate.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultSoakDuration      = 12 * time.Hour
	minimumReleaseDuration   = 12 * time.Hour
	defaultSoakConcurrency   = 8
	defaultRequestTimeout    = 2 * time.Minute
	defaultHealthTimeout     = 5 * time.Second
	defaultRecoveryTimeout   = 2 * time.Minute
	defaultRecoveryInterval  = 5 * time.Second
	resourceTrendWarmup      = 15 * time.Minute
	minimumMemoryHeadroom    = 32 << 20
	minimumCountHeadroom     = 2
	maxSoakResponseBodyBytes = 16 << 20
	maxHealthResponseBytes   = 64 << 10
	thinkingHistoryPolicyID  = "thinking_history.synthetic_budget"
)

type configuration struct {
	baseURL          string
	endpoint         string
	model            string
	duration         time.Duration
	concurrency      int
	requestTimeout   time.Duration
	healthTimeout    time.Duration
	recoveryTimeout  time.Duration
	recoveryInterval time.Duration
	scenarioTimeout  time.Duration
	websocketIdle    time.Duration
	apiKey           string
	managementKey    string
	expectedCommit   string
	releaseGate      bool
	chaos            bool
	responsesWS      bool
}

type payloadProfile struct {
	name string
	body []byte
}

type requestMessage struct {
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatPayload struct {
	Model           string           `json:"model"`
	Messages        []requestMessage `json:"messages"`
	Tools           []map[string]any `json:"tools"`
	Stream          bool             `json:"stream"`
	MaxTokens       int              `json:"max_tokens"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
}

type soakStats struct {
	attempted            atomic.Uint64
	succeeded            atomic.Uint64
	failed               atomic.Uint64
	canceled             atomic.Uint64
	bytesSent            atomic.Uint64
	responseTooLarge     atomic.Uint64
	readinessFailed      atomic.Uint64
	livenessFailed       atomic.Uint64
	healthSamples        atomic.Uint64
	healthDetailsFailed  atomic.Uint64
	latencyTotalNS       atomic.Uint64
	latencyMaxNS         atomic.Uint64
	headerLatencyTotalNS atomic.Uint64
	headerLatencyMaxNS   atomic.Uint64
	mu                   sync.Mutex
	byProfile            map[string]uint64
	succeededByProfile   map[string]uint64
	byStatus             map[int]uint64
	byError              map[string]uint64
	resources            resourceSummary
	resourceObservations []resourceObservation
	thinkingHistory      transformEvidence
	expectedCommit       string
	releaseGate          bool
	scenarioMatrixRuns   uint64
	scenarios            map[string]scenarioResult
}

type resourcePoint struct {
	Timestamp     string  `json:"timestamp"`
	RSSBytes      *uint64 `json:"rss_bytes"`
	HeapLiveBytes uint64  `json:"heap_live_bytes"`
	Goroutines    int     `json:"goroutines"`
	OpenFDs       *int    `json:"open_fds"`
	OpenSockets   *int    `json:"open_sockets"`
	ActiveWeight  int     `json:"active_weight"`
	QueueDepth    int     `json:"queue_depth"`
}

type resourceMaxima struct {
	RSSBytes      uint64 `json:"rss_bytes"`
	HeapLiveBytes uint64 `json:"heap_live_bytes"`
	Goroutines    int    `json:"goroutines"`
	OpenFDs       int    `json:"open_fds"`
	OpenSockets   int    `json:"open_sockets"`
	ActiveWeight  int    `json:"active_weight"`
	QueueDepth    int    `json:"queue_depth"`
}

type resourceSummary struct {
	Samples         uint64           `json:"samples"`
	First           *resourcePoint   `json:"first,omitempty"`
	Last            *resourcePoint   `json:"last,omitempty"`
	Maximum         resourceMaxima   `json:"maximum"`
	RestartDetected bool             `json:"restart_detected"`
	CommitMismatch  bool             `json:"commit_mismatch"`
	StartedAt       string           `json:"started_at,omitempty"`
	Commit          string           `json:"commit,omitempty"`
	ExpectedCommit  string           `json:"expected_commit,omitempty"`
	Trend           resourceTrend    `json:"trend"`
	Recovery        resourceRecovery `json:"recovery"`
}

type resourceObservation struct {
	at    time.Time
	point resourcePoint
}

type resourceTrend struct {
	Samples              int      `json:"samples"`
	Window               string   `json:"window,omitempty"`
	RSSBytesPerHour      *float64 `json:"rss_bytes_per_hour,omitempty"`
	HeapLiveBytesPerHour float64  `json:"heap_live_bytes_per_hour"`
	GoroutinesPerHour    float64  `json:"goroutines_per_hour"`
	OpenFDsPerHour       *float64 `json:"open_fds_per_hour,omitempty"`
	OpenSocketsPerHour   *float64 `json:"open_sockets_per_hour,omitempty"`
	SustainedGrowth      bool     `json:"sustained_growth"`
}

type resourceRecovery struct {
	Attempted           bool     `json:"attempted"`
	Recovered           bool     `json:"recovered"`
	Duration            string   `json:"duration,omitempty"`
	BaselineHeapBytes   uint64   `json:"baseline_heap_bytes"`
	AllowedHeapBytes    uint64   `json:"allowed_heap_bytes"`
	BaselineRSSBytes    *uint64  `json:"baseline_rss_bytes,omitempty"`
	AllowedRSSBytes     *uint64  `json:"allowed_rss_bytes,omitempty"`
	BaselineGoroutines  int      `json:"baseline_goroutines"`
	AllowedGoroutines   int      `json:"allowed_goroutines"`
	BaselineOpenFDs     *int     `json:"baseline_open_fds,omitempty"`
	AllowedOpenFDs      *int     `json:"allowed_open_fds,omitempty"`
	BaselineOpenSockets *int     `json:"baseline_open_sockets,omitempty"`
	AllowedOpenSockets  *int     `json:"allowed_open_sockets,omitempty"`
	Unmet               []string `json:"unmet,omitempty"`
}

type soakReport struct {
	Duration                    string                    `json:"duration"`
	CompletedConfiguredDuration bool                      `json:"completed_configured_duration"`
	ReleaseGate                 bool                      `json:"release_gate"`
	Attempted                   uint64                    `json:"attempted"`
	Succeeded                   uint64                    `json:"succeeded"`
	Failed                      uint64                    `json:"failed"`
	Canceled                    uint64                    `json:"canceled_at_end"`
	BytesSent                   uint64                    `json:"bytes_sent"`
	ResponseTooLarge            uint64                    `json:"response_too_large"`
	ReadinessFailures           uint64                    `json:"readiness_failures"`
	LivenessFailures            uint64                    `json:"liveness_failures"`
	HealthSamples               uint64                    `json:"health_samples"`
	HealthDetailsFailures       uint64                    `json:"health_details_failures"`
	AverageLatencyMillis        float64                   `json:"average_latency_ms"`
	MaximumLatencyMillis        float64                   `json:"maximum_latency_ms"`
	AverageHeadersLatencyMillis float64                   `json:"average_headers_latency_ms"`
	MaximumHeadersLatencyMillis float64                   `json:"maximum_headers_latency_ms"`
	ByProfile                   map[string]uint64         `json:"by_profile"`
	SucceededByProfile          map[string]uint64         `json:"succeeded_by_profile"`
	ByStatus                    map[int]uint64            `json:"by_status"`
	ByError                     map[string]uint64         `json:"by_error"`
	ScenarioMatrixRuns          uint64                    `json:"scenario_matrix_runs"`
	Scenarios                   map[string]scenarioResult `json:"scenarios"`
	Resources                   resourceSummary           `json:"resources"`
	ThinkingHistory             transformEvidence         `json:"thinking_history"`
}

type transformEvidence struct {
	BaselinePolicySamples uint64 `json:"baseline_policy_samples"`
	LatestPolicySamples   uint64 `json:"latest_policy_samples"`
	Observed              bool   `json:"observed"`
	initialized           bool
}

type healthTransformDistribution struct {
	DurationBuckets struct {
		Samples uint64 `json:"samples"`
	} `json:"duration_buckets"`
}

type healthDetailsSnapshot struct {
	Build struct {
		Commit string `json:"commit"`
	} `json:"build"`
	Process struct {
		RSSBytes      *uint64 `json:"rss_bytes"`
		HeapLiveBytes uint64  `json:"heap_live_bytes"`
		Goroutines    int     `json:"goroutines"`
		OpenFDs       *int    `json:"open_fds"`
		OpenSockets   *int    `json:"open_sockets"`
		StartedAt     string  `json:"started_at"`
	} `json:"process"`
	Admission struct {
		ActiveWeight int `json:"active_weight"`
		QueueDepth   int `json:"queue_depth"`
	} `json:"admission"`
	Transforms struct {
		PolicyCatalog map[string]healthTransformDistribution `json:"policy_catalog"`
	} `json:"transforms"`
}

func main() {
	cfg, err := parseConfiguration()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	profiles, err := buildPayloadProfiles(cfg.model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build payload profiles: %v\n", err)
		os.Exit(2)
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	report, runErr := runSoak(signalCtx, cfg, profiles)
	report.Duration = time.Since(started).Round(time.Millisecond).String()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(report); errEncode != nil {
		fmt.Fprintf(os.Stderr, "encode report: %v\n", errEncode)
		os.Exit(1)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

func parseConfiguration() (configuration, error) {
	cfg := configuration{}
	flag.StringVar(&cfg.baseURL, "base-url", "http://127.0.0.1:8317", "staging CLIProxyAPI base URL")
	flag.StringVar(&cfg.endpoint, "endpoint", "/v1/chat/completions", "API endpoint used by the load profile")
	flag.StringVar(&cfg.model, "model", "", "configured staging model (required)")
	flag.DurationVar(&cfg.duration, "duration", defaultSoakDuration, "soak duration")
	flag.IntVar(&cfg.concurrency, "concurrency", defaultSoakConcurrency, "number of concurrent workers")
	flag.DurationVar(&cfg.requestTimeout, "request-timeout", defaultRequestTimeout, "maximum duration of one workload request")
	flag.DurationVar(&cfg.healthTimeout, "health-timeout", defaultHealthTimeout, "maximum duration of one health probe")
	flag.DurationVar(&cfg.recoveryTimeout, "recovery-timeout", defaultRecoveryTimeout, "maximum wait for resources to return to baseline after load")
	flag.DurationVar(&cfg.recoveryInterval, "recovery-interval", defaultRecoveryInterval, "resource probe interval after load")
	flag.DurationVar(&cfg.scenarioTimeout, "scenario-timeout", 30*time.Second, "client deadline for one deterministic chaos scenario")
	flag.DurationVar(&cfg.websocketIdle, "websocket-idle", 2*time.Second, "idle period exercised by the Responses WebSocket scenario")
	flag.StringVar(&cfg.expectedCommit, "expected-commit", strings.TrimSpace(os.Getenv("CPA_SOAK_EXPECTED_COMMIT")), "full commit SHA expected from /healthz/details")
	flag.BoolVar(&cfg.releaseGate, "release-gate", true, "enforce release duration and commit requirements")
	flag.BoolVar(&cfg.chaos, "chaos", true, "run the deterministic upstream fault matrix before and after load")
	flag.BoolVar(&cfg.responsesWS, "responses-websocket", true, "run Responses WebSocket connect, idle, frame, and cancel scenarios")
	flag.Parse()
	cfg.baseURL = strings.TrimRight(strings.TrimSpace(cfg.baseURL), "/")
	cfg.endpoint = "/" + strings.TrimLeft(strings.TrimSpace(cfg.endpoint), "/")
	cfg.model = strings.TrimSpace(cfg.model)
	cfg.apiKey = strings.TrimSpace(os.Getenv("CPA_SOAK_API_KEY"))
	cfg.managementKey = strings.TrimSpace(os.Getenv("CPA_SOAK_MANAGEMENT_KEY"))
	cfg.expectedCommit = strings.TrimSpace(cfg.expectedCommit)
	if cfg.baseURL == "" || cfg.model == "" || cfg.apiKey == "" || cfg.managementKey == "" {
		return configuration{}, errors.New("base URL, -model, CPA_SOAK_API_KEY, and CPA_SOAK_MANAGEMENT_KEY are required")
	}
	parsed, err := url.Parse(cfg.baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return configuration{}, fmt.Errorf("invalid -base-url %q", cfg.baseURL)
	}
	if cfg.duration <= 0 || cfg.concurrency <= 0 || cfg.requestTimeout <= 0 || cfg.healthTimeout <= 0 || cfg.recoveryTimeout <= 0 || cfg.recoveryInterval <= 0 || cfg.scenarioTimeout <= 0 || cfg.websocketIdle <= 0 {
		return configuration{}, errors.New("duration, concurrency, and request timeouts must be positive")
	}
	if errRelease := validateReleaseConfiguration(cfg); errRelease != nil {
		return configuration{}, errRelease
	}
	return cfg, nil
}

func validateReleaseConfiguration(cfg configuration) error {
	if cfg.expectedCommit != "" && !isFullCommitSHA(cfg.expectedCommit) {
		return errors.New("-expected-commit must be a full 40-character hexadecimal commit SHA")
	}
	if !cfg.releaseGate {
		return nil
	}
	if cfg.expectedCommit == "" {
		return errors.New("-expected-commit is required for the release gate")
	}
	if cfg.duration < minimumReleaseDuration {
		return fmt.Errorf("release gate duration must be at least %s", minimumReleaseDuration)
	}
	if !cfg.chaos {
		return errors.New("release gate requires the deterministic chaos matrix")
	}
	if !cfg.responsesWS {
		return errors.New("release gate requires Responses WebSocket scenarios")
	}
	return nil
}

func isFullCommitSHA(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for _, character := range commit {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func verifyBuildCommit(expected, actual string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	actual = strings.TrimSpace(actual)
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("build commit mismatch: got %q, want %q", actual, expected)
	}
	return nil
}

func buildPayloadProfiles(model string) ([3]payloadProfile, error) {
	specs := []struct {
		name           string
		messageCount   int
		messageBytes   int
		toolBytes      int
		reasoningBytes int
		imageBytes     int
	}{
		{name: "small", messageCount: 4, messageBytes: 4 << 10, toolBytes: 4 << 10},
		{name: "medium", messageCount: 32, messageBytes: 8 << 10, toolBytes: 512 << 10, reasoningBytes: 256 << 10, imageBytes: 512 << 10},
		{name: "large", messageCount: 256, messageBytes: 16 << 10, toolBytes: 4 << 20, reasoningBytes: 2 << 20, imageBytes: 6 << 20},
	}
	var profiles [3]payloadProfile
	for index, spec := range specs {
		body, err := buildChatPayload(model, spec.messageCount, spec.messageBytes, spec.toolBytes, spec.reasoningBytes, spec.imageBytes)
		if err != nil {
			return profiles, fmt.Errorf("%s profile: %w", spec.name, err)
		}
		profiles[index] = payloadProfile{name: spec.name, body: body}
	}
	return profiles, nil
}

func buildChatPayload(model string, messageCount, messageBytes, toolBytes, reasoningBytes, imageBytes int) ([]byte, error) {
	messages := make([]requestMessage, 0, messageCount+4)
	messages = append(messages, requestMessage{Role: "system", Content: "payload soak fixture; do not retain"})
	text := strings.Repeat("t", messageBytes)
	for index := 0; index < messageCount; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, requestMessage{Role: role, Content: text})
	}
	if reasoningBytes > 0 {
		messages = append(messages, requestMessage{Role: "assistant", Content: "summary", ReasoningContent: strings.Repeat("r", reasoningBytes)})
	}
	messages = append(messages,
		requestMessage{
			Role:    "assistant",
			Content: "calling fixture tool",
			ToolCalls: []toolCall{{
				ID:   "call_payload_soak",
				Type: "function",
				Function: toolFunction{
					Name:      "payload_soak_fixture",
					Arguments: `{"size":"mixed"}`,
				},
			}},
		},
		requestMessage{Role: "tool", ToolCallID: "call_payload_soak", Content: strings.Repeat("o", toolBytes)},
	)
	if imageBytes > 0 {
		messages = append(messages, requestMessage{Role: "user", Content: []map[string]any{
			{"type": "text", "text": "inspect the generated fixture"},
			{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + strings.Repeat("A", imageBytes)}},
		}})
	}
	payload := chatPayload{
		Model:    model,
		Messages: messages,
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "payload_soak_fixture",
				"description": "Synthetic release-gate fixture",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"size": map[string]string{"type": "string"}},
				},
			},
		}},
		Stream:    false,
		MaxTokens: 1,
	}
	if reasoningBytes > 0 {
		payload.ReasoningEffort = "high"
	}
	return json.Marshal(payload)
}

func selectPayloadProfile(sequence uint64, profiles [3]payloadProfile) payloadProfile {
	switch sequence % 100 {
	case 99:
		return profiles[2]
	case 90, 91, 92, 93, 94, 95, 96, 97, 98:
		return profiles[1]
	default:
		return profiles[0]
	}
}

func runSoak(parentCtx context.Context, cfg configuration, profiles [3]payloadProfile) (soakReport, error) {
	if errRelease := validateReleaseConfiguration(cfg); errRelease != nil {
		return soakReport{}, errRelease
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = cfg.concurrency * 2
	transport.MaxIdleConnsPerHost = cfg.concurrency
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	stats := &soakStats{
		byProfile:          make(map[string]uint64, len(profiles)),
		succeededByProfile: make(map[string]uint64, len(profiles)),
		byStatus:           make(map[int]uint64),
		byError:            make(map[string]uint64),
		expectedCommit:     cfg.expectedCommit,
		releaseGate:        cfg.releaseGate,
		scenarios:          make(map[string]scenarioResult, len(requiredReleaseScenarios)),
		resources:          resourceSummary{ExpectedCommit: cfg.expectedCommit},
	}
	if err := sampleResourcePreflight(parentCtx, client, cfg, stats); err != nil {
		return stats.report(), err
	}
	if cfg.chaos {
		if errChaos := runChaosMatrixWithRecovery(parentCtx, client, transport, cfg, stats); errChaos != nil {
			return stats.report(), fmt.Errorf("pre-load chaos matrix: %w", errChaos)
		}
	}
	loadCtx, cancelLoad := context.WithTimeout(parentCtx, cfg.duration)
	defer cancelLoad()
	var sequence atomic.Uint64
	var workers sync.WaitGroup
	workers.Add(cfg.concurrency)
	for range cfg.concurrency {
		go func() {
			defer workers.Done()
			for loadCtx.Err() == nil {
				profile := selectPayloadProfile(sequence.Add(1)-1, profiles)
				runSoakRequest(loadCtx, client, cfg, profile, stats)
			}
		}()
	}
	healthDone := make(chan struct{})
	go func() {
		defer close(healthDone)
		sampleHealth(loadCtx, client, cfg, stats)
	}()
	workers.Wait()
	<-healthDone
	loadErr := loadCtx.Err()
	loadCompleted := errors.Is(loadErr, context.DeadlineExceeded) && parentCtx.Err() == nil
	var chaosErr error
	if loadCompleted && cfg.chaos {
		chaosErr = runChaosMatrix(parentCtx, client, cfg, stats)
	}
	if loadCompleted {
		transport.CloseIdleConnections()
		recoverResources(parentCtx, client, cfg, stats)
	}
	report := stats.report()
	report.CompletedConfiguredDuration = loadCompleted
	if chaosErr != nil {
		return report, errors.Join(fmt.Errorf("post-load chaos matrix: %w", chaosErr), validateSoakReport(loadErr, report, profiles))
	}
	return report, validateSoakReport(loadErr, report, profiles)
}

func validateSoakReport(ctxErr error, report soakReport, profiles [3]payloadProfile) error {
	if report.Attempted == 0 || report.Succeeded == 0 {
		return errors.New("soak completed without a successful request")
	}
	if report.Succeeded+report.Failed+report.Canceled != report.Attempted {
		return errors.New("soak request accounting is incomplete")
	}
	if !report.CompletedConfiguredDuration || !errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("soak stopped before its configured duration: %v", ctxErr)
	}
	if report.HealthSamples == 0 {
		return errors.New("soak completed without a health sample")
	}
	if report.HealthDetailsFailures > 0 || report.Resources.Samples == 0 {
		return errors.New("soak completed without continuous authenticated resource samples")
	}
	if report.Resources.CommitMismatch {
		return fmt.Errorf("soak observed a build commit other than %q", report.Resources.ExpectedCommit)
	}
	if report.Resources.RestartDetected {
		return errors.New("soak detected a process restart or revision change")
	}
	if report.Resources.Trend.SustainedGrowth {
		return errors.New("soak detected sustained resource growth after warm-up")
	}
	if !report.Resources.Recovery.Attempted || !report.Resources.Recovery.Recovered {
		if len(report.Resources.Recovery.Unmet) > 0 {
			return fmt.Errorf("soak resources did not recover: %s", strings.Join(report.Resources.Recovery.Unmet, "; "))
		}
		return errors.New("soak resources did not return to baseline")
	}
	if report.ReleaseGate {
		if errScenarios := validateReleaseScenarios(report); errScenarios != nil {
			return errScenarios
		}
		if !report.ThinkingHistory.Observed {
			return errors.New("soak did not exercise the thinking-history normalization policy")
		}
	}
	for _, profile := range profiles {
		if report.SucceededByProfile[profile.name] == 0 {
			return fmt.Errorf("soak completed without a successful %s request", profile.name)
		}
	}
	if report.Failed > 0 || report.ResponseTooLarge > 0 || report.ReadinessFailures > 0 || report.LivenessFailures > 0 {
		return errors.New("soak release gate failed; inspect the JSON report")
	}
	return nil
}

func runSoakRequest(ctx context.Context, client *http.Client, cfg configuration, profile payloadProfile, stats *soakStats) {
	started := time.Now()
	stats.attempted.Add(1)
	stats.bytesSent.Add(uint64(len(profile.body)))
	stats.mu.Lock()
	stats.byProfile[profile.name]++
	stats.mu.Unlock()
	requestCtx, cancelRequest := context.WithTimeout(ctx, cfg.requestTimeout)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.baseURL+cfg.endpoint, bytes.NewReader(profile.body))
	if err != nil {
		stats.recordError("request_build")
		return
	}
	request.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	headersLatency := time.Since(started)
	if err != nil {
		stats.observeLatency(headersLatency, headersLatency)
		stats.recordRequestFailure(ctx, requestCtx, "transport")
		return
	}
	read, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxSoakResponseBodyBytes+1))
	errClose := response.Body.Close()
	stats.observeLatency(headersLatency, time.Since(started))
	if err == nil {
		err = errClose
	}
	if err != nil {
		stats.recordRequestFailure(ctx, requestCtx, "response_read")
		return
	}
	if read > maxSoakResponseBodyBytes {
		stats.responseTooLarge.Add(1)
		stats.recordError("response_too_large")
		return
	}
	stats.mu.Lock()
	stats.byStatus[response.StatusCode]++
	stats.mu.Unlock()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		stats.recordError("non_2xx")
		return
	}
	stats.succeeded.Add(1)
	stats.mu.Lock()
	stats.succeededByProfile[profile.name]++
	stats.mu.Unlock()
}

func sampleHealth(ctx context.Context, client *http.Client, cfg configuration, stats *soakStats) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cycles := uint64(0)
	probe := func() bool {
		if ctx.Err() != nil {
			return false
		}
		livenessOK := healthOK(ctx, client, cfg.baseURL+"/livez", cfg.healthTimeout)
		if ctx.Err() != nil {
			return false
		}
		readinessOK := healthOK(ctx, client, cfg.baseURL+"/readyz", cfg.healthTimeout)
		if ctx.Err() != nil {
			return false
		}
		stats.healthSamples.Add(1)
		cycles++
		if cycles == 1 || cycles%30 == 0 {
			snapshot, detailsOK := readHealthDetails(ctx, client, cfg.baseURL+"/healthz/details?gc=1", cfg.managementKey, cfg.healthTimeout)
			if ctx.Err() != nil {
				return false
			}
			if !detailsOK {
				stats.healthDetailsFailed.Add(1)
			} else {
				stats.recordResourceSnapshot(snapshot, time.Now(), true)
			}
		}
		if !livenessOK {
			stats.livenessFailed.Add(1)
		}
		if !readinessOK {
			stats.readinessFailed.Add(1)
		}
		return true
	}
	if !probe() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !probe() {
				return
			}
		}
	}
}

func sampleResourcePreflight(ctx context.Context, client *http.Client, cfg configuration, stats *soakStats) error {
	if !healthOK(ctx, client, cfg.baseURL+"/livez", cfg.healthTimeout) {
		stats.livenessFailed.Add(1)
		return errors.New("preflight liveness probe failed")
	}
	if !healthOK(ctx, client, cfg.baseURL+"/readyz", cfg.healthTimeout) {
		stats.readinessFailed.Add(1)
		return errors.New("preflight readiness probe failed")
	}
	stats.healthSamples.Add(1)
	snapshot, ok := readHealthDetails(ctx, client, cfg.baseURL+"/healthz/details?gc=1", cfg.managementKey, cfg.healthTimeout)
	if !ok {
		stats.healthDetailsFailed.Add(1)
		return errors.New("preflight authenticated health details probe failed")
	}
	stats.recordResourceSnapshot(snapshot, time.Now(), true)
	if errCommit := verifyBuildCommit(cfg.expectedCommit, snapshot.Build.Commit); errCommit != nil {
		return fmt.Errorf("preflight %w", errCommit)
	}
	return nil
}

func recoverResources(parentCtx context.Context, client *http.Client, cfg configuration, stats *soakStats) {
	started := time.Now()
	baseline := stats.firstResourcePoint()
	limits := recoveryLimits(baseline)
	stats.setRecovery(limits)

	recoveryCtx, cancelRecovery := context.WithTimeout(parentCtx, cfg.recoveryTimeout)
	defer cancelRecovery()
	ticker := time.NewTicker(cfg.recoveryInterval)
	defer ticker.Stop()
	var unmet []string
	probe := func() bool {
		snapshot, ok := readHealthDetails(recoveryCtx, client, cfg.baseURL+"/healthz/details?gc=1", cfg.managementKey, cfg.healthTimeout)
		if !ok {
			if recoveryCtx.Err() != nil {
				return false
			}
			stats.healthDetailsFailed.Add(1)
			unmet = []string{"health details unavailable"}
			return false
		}
		stats.recordResourceSnapshot(snapshot, time.Now(), false)
		unmet = recoveryFailures(snapshot, limits)
		return len(unmet) == 0
	}
	for {
		if recoveryCtx.Err() != nil {
			stats.finishRecovery(false, time.Since(started), unmet)
			return
		}
		if probe() {
			stats.finishRecovery(true, time.Since(started), nil)
			return
		}
		select {
		case <-recoveryCtx.Done():
			stats.finishRecovery(false, time.Since(started), unmet)
			return
		case <-ticker.C:
		}
	}
}

func recoveryLimits(baseline *resourcePoint) resourceRecovery {
	recovery := resourceRecovery{Attempted: true}
	if baseline == nil {
		recovery.Unmet = []string{"baseline unavailable"}
		return recovery
	}
	recovery.BaselineHeapBytes = baseline.HeapLiveBytes
	recovery.AllowedHeapBytes = baseline.HeapLiveBytes + max(uint64(minimumMemoryHeadroom), (baseline.HeapLiveBytes+19)/20)
	recovery.BaselineGoroutines = baseline.Goroutines
	recovery.AllowedGoroutines = baseline.Goroutines + max(1, (baseline.Goroutines+19)/20)
	if baseline.RSSBytes != nil {
		baselineRSS := *baseline.RSSBytes
		allowedRSS := baselineRSS + max(uint64(minimumMemoryHeadroom), (baselineRSS+19)/20)
		recovery.BaselineRSSBytes = &baselineRSS
		recovery.AllowedRSSBytes = &allowedRSS
	}
	if baseline.OpenFDs != nil {
		baselineFDs := *baseline.OpenFDs
		allowedFDs := baselineFDs + max(minimumCountHeadroom, (baselineFDs+19)/20)
		recovery.BaselineOpenFDs = &baselineFDs
		recovery.AllowedOpenFDs = &allowedFDs
	}
	if baseline.OpenSockets != nil {
		baselineSockets := *baseline.OpenSockets
		allowedSockets := baselineSockets + max(minimumCountHeadroom, (baselineSockets+19)/20)
		recovery.BaselineOpenSockets = &baselineSockets
		recovery.AllowedOpenSockets = &allowedSockets
	}
	return recovery
}

func recoveryFailures(snapshot healthDetailsSnapshot, limits resourceRecovery) []string {
	if limits.AllowedGoroutines == 0 || limits.AllowedHeapBytes == 0 {
		return []string{"baseline unavailable"}
	}
	failures := make([]string, 0, 6)
	if snapshot.Admission.ActiveWeight != 0 || snapshot.Admission.QueueDepth != 0 {
		failures = append(failures, fmt.Sprintf("admission active_weight=%d queue_depth=%d", snapshot.Admission.ActiveWeight, snapshot.Admission.QueueDepth))
	}
	if snapshot.Process.Goroutines > limits.AllowedGoroutines {
		failures = append(failures, fmt.Sprintf("goroutines=%d limit=%d", snapshot.Process.Goroutines, limits.AllowedGoroutines))
	}
	if snapshot.Process.HeapLiveBytes > limits.AllowedHeapBytes {
		failures = append(failures, fmt.Sprintf("heap_live_bytes=%d limit=%d", snapshot.Process.HeapLiveBytes, limits.AllowedHeapBytes))
	}
	if limits.AllowedRSSBytes != nil {
		if snapshot.Process.RSSBytes == nil {
			failures = append(failures, "rss unavailable during recovery")
		} else if *snapshot.Process.RSSBytes > *limits.AllowedRSSBytes {
			failures = append(failures, fmt.Sprintf("rss_bytes=%d limit=%d", *snapshot.Process.RSSBytes, *limits.AllowedRSSBytes))
		}
	}
	if limits.AllowedOpenFDs != nil {
		if snapshot.Process.OpenFDs == nil {
			failures = append(failures, "open_fds unavailable during recovery")
		} else if *snapshot.Process.OpenFDs > *limits.AllowedOpenFDs {
			failures = append(failures, fmt.Sprintf("open_fds=%d limit=%d", *snapshot.Process.OpenFDs, *limits.AllowedOpenFDs))
		}
	}
	if limits.AllowedOpenSockets != nil {
		if snapshot.Process.OpenSockets == nil {
			failures = append(failures, "open_sockets unavailable during recovery")
		} else if *snapshot.Process.OpenSockets > *limits.AllowedOpenSockets {
			failures = append(failures, fmt.Sprintf("open_sockets=%d limit=%d", *snapshot.Process.OpenSockets, *limits.AllowedOpenSockets))
		}
	}
	return failures
}

func readHealthDetails(ctx context.Context, client *http.Client, endpoint, managementKey string, timeout time.Duration) (healthDetailsSnapshot, bool) {
	probeCtx, cancelProbe := context.WithTimeout(ctx, timeout)
	defer cancelProbe()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return healthDetailsSnapshot{}, false
	}
	request.Header.Set("X-Management-Key", managementKey)
	response, err := client.Do(request)
	if err != nil {
		return healthDetailsSnapshot{}, false
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return healthDetailsSnapshot{}, false
	}
	limited := io.LimitReader(response.Body, (1<<20)+1)
	raw, errRead := io.ReadAll(limited)
	if errRead != nil || len(raw) > 1<<20 {
		return healthDetailsSnapshot{}, false
	}
	var snapshot healthDetailsSnapshot
	if errDecode := json.Unmarshal(raw, &snapshot); errDecode != nil || strings.TrimSpace(snapshot.Process.StartedAt) == "" {
		return healthDetailsSnapshot{}, false
	}
	return snapshot, true
}

func (s *soakStats) recordResourceSnapshot(snapshot healthDetailsSnapshot, timestamp time.Time, includeInTrend bool) {
	point := resourcePoint{
		Timestamp:     timestamp.UTC().Format(time.RFC3339Nano),
		RSSBytes:      snapshot.Process.RSSBytes,
		HeapLiveBytes: snapshot.Process.HeapLiveBytes,
		Goroutines:    snapshot.Process.Goroutines,
		OpenFDs:       snapshot.Process.OpenFDs,
		OpenSockets:   snapshot.Process.OpenSockets,
		ActiveWeight:  snapshot.Admission.ActiveWeight,
		QueueDepth:    snapshot.Admission.QueueDepth,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	thinkingSamples := snapshot.Transforms.PolicyCatalog[thinkingHistoryPolicyID].DurationBuckets.Samples
	if !s.thinkingHistory.initialized {
		s.thinkingHistory.BaselinePolicySamples = thinkingSamples
		s.thinkingHistory.initialized = true
	}
	s.thinkingHistory.LatestPolicySamples = thinkingSamples
	s.thinkingHistory.Observed = thinkingSamples > s.thinkingHistory.BaselinePolicySamples
	resources := &s.resources
	resources.Samples++
	actualCommit := strings.TrimSpace(snapshot.Build.Commit)
	if s.expectedCommit != "" && !strings.EqualFold(actualCommit, s.expectedCommit) {
		resources.CommitMismatch = true
	}
	if includeInTrend {
		s.resourceObservations = append(s.resourceObservations, resourceObservation{at: timestamp, point: point})
	}
	if resources.First == nil {
		first := point
		resources.First = &first
		resources.StartedAt = snapshot.Process.StartedAt
		resources.Commit = actualCommit
	} else if resources.StartedAt != snapshot.Process.StartedAt || !strings.EqualFold(resources.Commit, actualCommit) {
		resources.RestartDetected = true
	}
	last := point
	resources.Last = &last
	if point.RSSBytes != nil {
		resources.Maximum.RSSBytes = max(resources.Maximum.RSSBytes, *point.RSSBytes)
	}
	resources.Maximum.HeapLiveBytes = max(resources.Maximum.HeapLiveBytes, point.HeapLiveBytes)
	resources.Maximum.Goroutines = max(resources.Maximum.Goroutines, point.Goroutines)
	if point.OpenFDs != nil {
		resources.Maximum.OpenFDs = max(resources.Maximum.OpenFDs, *point.OpenFDs)
	}
	if point.OpenSockets != nil {
		resources.Maximum.OpenSockets = max(resources.Maximum.OpenSockets, *point.OpenSockets)
	}
	resources.Maximum.ActiveWeight = max(resources.Maximum.ActiveWeight, point.ActiveWeight)
	resources.Maximum.QueueDepth = max(resources.Maximum.QueueDepth, point.QueueDepth)
}

func (s *soakStats) firstResourcePoint() *resourcePoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resources.First == nil {
		return nil
	}
	point := *s.resources.First
	return &point
}

func (s *soakStats) setRecovery(recovery resourceRecovery) {
	s.mu.Lock()
	s.resources.Recovery = recovery
	s.mu.Unlock()
}

func (s *soakStats) finishRecovery(recovered bool, duration time.Duration, unmet []string) {
	s.mu.Lock()
	s.resources.Recovery.Recovered = recovered
	s.resources.Recovery.Duration = duration.Round(time.Millisecond).String()
	s.resources.Recovery.Unmet = append([]string(nil), unmet...)
	s.mu.Unlock()
}

func healthOK(ctx context.Context, client *http.Client, endpoint string, timeout time.Duration) bool {
	probeCtx, cancelProbe := context.WithTimeout(ctx, timeout)
	defer cancelProbe()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer func() {
		_ = response.Body.Close()
	}()
	read, errRead := io.Copy(io.Discard, io.LimitReader(response.Body, maxHealthResponseBytes+1))
	return errRead == nil && read <= maxHealthResponseBytes && response.StatusCode == http.StatusOK
}

func (s *soakStats) recordRequestFailure(parentCtx, requestCtx context.Context, fallback string) {
	if parentCtx != nil && parentCtx.Err() != nil {
		s.canceled.Add(1)
		return
	}
	if requestCtx != nil && errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		s.recordError("request_timeout")
		return
	}
	s.recordError(fallback)
}

func (s *soakStats) recordError(kind string) {
	s.failed.Add(1)
	s.mu.Lock()
	s.byError[kind]++
	s.mu.Unlock()
}

func (s *soakStats) observeLatency(headers, endToEnd time.Duration) {
	recordLatency(&s.headerLatencyTotalNS, &s.headerLatencyMaxNS, headers)
	recordLatency(&s.latencyTotalNS, &s.latencyMaxNS, endToEnd)
}

func recordLatency(total, maximum *atomic.Uint64, duration time.Duration) {
	nanoseconds := uint64(max(duration, 0))
	total.Add(nanoseconds)
	for {
		current := maximum.Load()
		if nanoseconds <= current || maximum.CompareAndSwap(current, nanoseconds) {
			return
		}
	}
}

func (s *soakStats) report() soakReport {
	attempted := s.attempted.Load()
	average := float64(0)
	if attempted > 0 {
		average = float64(s.latencyTotalNS.Load()) / float64(attempted) / float64(time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resources := cloneResourceSummary(s.resources)
	resources.Trend = calculateResourceTrend(s.resourceObservations)
	return soakReport{
		Attempted:             attempted,
		Succeeded:             s.succeeded.Load(),
		Failed:                s.failed.Load(),
		Canceled:              s.canceled.Load(),
		BytesSent:             s.bytesSent.Load(),
		ResponseTooLarge:      s.responseTooLarge.Load(),
		ReadinessFailures:     s.readinessFailed.Load(),
		LivenessFailures:      s.livenessFailed.Load(),
		HealthSamples:         s.healthSamples.Load(),
		HealthDetailsFailures: s.healthDetailsFailed.Load(),
		AverageLatencyMillis:  average,
		MaximumLatencyMillis:  float64(s.latencyMaxNS.Load()) / float64(time.Millisecond),
		AverageHeadersLatencyMillis: func() float64 {
			if attempted == 0 {
				return 0
			}
			return float64(s.headerLatencyTotalNS.Load()) / float64(attempted) / float64(time.Millisecond)
		}(),
		MaximumHeadersLatencyMillis: float64(s.headerLatencyMaxNS.Load()) / float64(time.Millisecond),
		ByProfile:                   cloneStringCounts(s.byProfile),
		SucceededByProfile:          cloneStringCounts(s.succeededByProfile),
		ByStatus:                    cloneStatusCounts(s.byStatus),
		ByError:                     cloneStringCounts(s.byError),
		ReleaseGate:                 s.releaseGate,
		ScenarioMatrixRuns:          s.scenarioMatrixRuns,
		Scenarios:                   cloneScenarioResults(s.scenarios),
		Resources:                   resources,
		ThinkingHistory:             publicTransformEvidence(s.thinkingHistory),
	}
}

func publicTransformEvidence(evidence transformEvidence) transformEvidence {
	evidence.initialized = false
	return evidence
}

func calculateResourceTrend(observations []resourceObservation) resourceTrend {
	if len(observations) < 5 {
		return resourceTrend{Samples: len(observations)}
	}
	warmupEnd := observations[0].at.Add(resourceTrendWarmup)
	windowStart := 0
	for windowStart < len(observations) && observations[windowStart].at.Before(warmupEnd) {
		windowStart++
	}
	windowStart = min(windowStart, len(observations)/5)
	window := observations[windowStart:]
	if len(window) < 3 {
		return resourceTrend{Samples: len(window)}
	}
	trend := resourceTrend{
		Samples: len(window),
		Window:  window[len(window)-1].at.Sub(window[0].at).Round(time.Second).String(),
		HeapLiveBytesPerHour: linearResourceSlope(window, func(point resourcePoint) (float64, bool) {
			return float64(point.HeapLiveBytes), true
		}),
		GoroutinesPerHour: linearResourceSlope(window, func(point resourcePoint) (float64, bool) {
			return float64(point.Goroutines), true
		}),
	}
	rssSlope, hasRSS := linearOptionalResourceSlope(window, func(point resourcePoint) *uint64 { return point.RSSBytes })
	if hasRSS {
		trend.RSSBytesPerHour = &rssSlope
	}
	fdSlope, hasFDs := linearOptionalIntResourceSlope(window, func(point resourcePoint) *int { return point.OpenFDs })
	if hasFDs {
		trend.OpenFDsPerHour = &fdSlope
	}
	socketSlope, hasSockets := linearOptionalIntResourceSlope(window, func(point resourcePoint) *int { return point.OpenSockets })
	if hasSockets {
		trend.OpenSocketsPerHour = &socketSlope
	}

	first := window[0].point
	last := window[len(window)-1].point
	heapThreshold := max(uint64(32<<20), first.HeapLiveBytes/20)
	heapGrowth := last.HeapLiveBytes > first.HeapLiveBytes && last.HeapLiveBytes-first.HeapLiveBytes > heapThreshold && trend.HeapLiveBytesPerHour > 0
	goroutineThreshold := max(1, (first.Goroutines+19)/20)
	goroutineGrowth := last.Goroutines > first.Goroutines+goroutineThreshold && trend.GoroutinesPerHour > 0
	rssGrowth := false
	if hasRSS && first.RSSBytes != nil && last.RSSBytes != nil {
		rssThreshold := max(uint64(32<<20), *first.RSSBytes/20)
		rssGrowth = *last.RSSBytes > *first.RSSBytes && *last.RSSBytes-*first.RSSBytes > rssThreshold && rssSlope > 0
	}
	fdGrowth := optionalCountGrowth(first.OpenFDs, last.OpenFDs, fdSlope, hasFDs)
	socketGrowth := optionalCountGrowth(first.OpenSockets, last.OpenSockets, socketSlope, hasSockets)
	trend.SustainedGrowth = heapGrowth || rssGrowth || goroutineGrowth || fdGrowth || socketGrowth
	return trend
}

func optionalCountGrowth(first, last *int, slope float64, available bool) bool {
	if !available || first == nil || last == nil {
		return false
	}
	threshold := max(minimumCountHeadroom, (*first+19)/20)
	return *last > *first+threshold && slope > 0
}

func linearResourceSlope(observations []resourceObservation, value func(resourcePoint) (float64, bool)) float64 {
	if len(observations) < 2 {
		return 0
	}
	started := observations[0].at
	var count, sumX, sumY, sumXX, sumXY float64
	for _, observation := range observations {
		y, ok := value(observation.point)
		if !ok {
			continue
		}
		x := observation.at.Sub(started).Hours()
		count++
		sumX += x
		sumY += y
		sumXX += x * x
		sumXY += x * y
	}
	denominator := count*sumXX - sumX*sumX
	if count < 2 || denominator == 0 {
		return 0
	}
	return (count*sumXY - sumX*sumY) / denominator
}

func linearOptionalResourceSlope(observations []resourceObservation, value func(resourcePoint) *uint64) (float64, bool) {
	available := 0
	slope := linearResourceSlope(observations, func(point resourcePoint) (float64, bool) {
		current := value(point)
		if current == nil {
			return 0, false
		}
		available++
		return float64(*current), true
	})
	return slope, available >= 2
}

func linearOptionalIntResourceSlope(observations []resourceObservation, value func(resourcePoint) *int) (float64, bool) {
	available := 0
	slope := linearResourceSlope(observations, func(point resourcePoint) (float64, bool) {
		current := value(point)
		if current == nil {
			return 0, false
		}
		available++
		return float64(*current), true
	})
	return slope, available >= 2
}

func cloneResourceSummary(source resourceSummary) resourceSummary {
	cloned := source
	if source.First != nil {
		first := *source.First
		cloned.First = &first
	}
	if source.Last != nil {
		last := *source.Last
		cloned.Last = &last
	}
	return cloned
}

func cloneStringCounts(source map[string]uint64) map[string]uint64 {
	cloned := make(map[string]uint64, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneStatusCounts(source map[int]uint64) map[int]uint64 {
	cloned := make(map[int]uint64, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
