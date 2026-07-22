package handlers

import (
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	defaultJSONPayloadBodyBytes      int64 = 64 << 20
	defaultMultipartPayloadBodyBytes int64 = 128 << 20
	defaultWebsocketPayloadBodyBytes int64 = 64 << 20

	// EmergencyPayloadBodyBytes is the non-configurable safety ceiling used
	// while a public route is in observe mode.
	EmergencyPayloadBodyBytes int64 = 256 << 20

	payloadBodyLimitGinKey         = "cliproxy_payload_body_limit_policy"
	payloadBodyLimitObserve        = "observe"
	payloadBodyLimitEnforce        = "enforce"
	payloadBodyMetricMaxKeys       = 32
	unknownPayloadBodyBytes  int64 = -1
)

type payloadBodyKind string

const (
	payloadBodyKindJSON      payloadBodyKind = "json"
	payloadBodyKindMultipart payloadBodyKind = "multipart"
	payloadBodyKindWebsocket payloadBodyKind = "websocket"
	payloadBodyKindOther     payloadBodyKind = "other"
)

var payloadBodyMetricKinds = [...]payloadBodyKind{
	payloadBodyKindJSON,
	payloadBodyKindMultipart,
	payloadBodyKindWebsocket,
	payloadBodyKindOther,
}

type payloadBodyLimitPolicy struct {
	mode           string
	jsonBytes      int64
	multipartBytes int64
	websocketBytes int64
}

type payloadBodyLimitDecision struct {
	policy           *payloadBodyLimitPolicy
	kind             payloadBodyKind
	softWireBytes    int64
	softDecodedBytes int64
	maxWireBytes     int64
	maxDecodedBytes  int64
}

// PayloadBodyLimitMetric is a fixed-cardinality aggregate for one endpoint
// and body kind. Byte fields are maxima, not payload samples.
type PayloadBodyLimitMetric struct {
	Requests             uint64 `json:"requests"`
	WouldReject          uint64 `json:"would_reject"`
	Rejected             uint64 `json:"rejected"`
	EmergencyRejected    uint64 `json:"emergency_rejected"`
	MaxWireBytes         int64  `json:"max_wire_bytes"`
	MaxDecodedBytes      int64  `json:"max_decoded_bytes"`
	PhysicalCeilingBytes int64  `json:"physical_ceiling_bytes"`
}

// PayloadBodyLimitSizeBuckets is a cumulative, fixed-boundary histogram. A
// p99.9 estimate can be found by locating the first le_* bucket whose count is
// at least ceil(samples*0.999). Overflow contains samples above 64 MiB, while
// Unknown counts observations whose decoded size could not be established.
type PayloadBodyLimitSizeBuckets struct {
	Samples               uint64 `json:"samples"`
	Unknown               uint64 `json:"unknown"`
	LessThanOrEqual64KiB  uint64 `json:"le_64_kib"`
	LessThanOrEqual256KiB uint64 `json:"le_256_kib"`
	LessThanOrEqual1MiB   uint64 `json:"le_1_mib"`
	LessThanOrEqual4MiB   uint64 `json:"le_4_mib"`
	LessThanOrEqual16MiB  uint64 `json:"le_16_mib"`
	LessThanOrEqual64MiB  uint64 `json:"le_64_mib"`
	Overflow              uint64 `json:"overflow"`
}

// PayloadBodyLimitTotals adds process-wide size histograms to the common
// counters. Per-endpoint metrics intentionally remain counters and maxima.
type PayloadBodyLimitTotals struct {
	PayloadBodyLimitMetric
	WireSizeBuckets    PayloadBodyLimitSizeBuckets `json:"wire_size_buckets"`
	DecodedSizeBuckets PayloadBodyLimitSizeBuckets `json:"decoded_size_buckets"`
}

// PayloadBodyLimitSnapshot exposes the effective policy and bounded metrics
// so the health endpoint can publish them without exposing request data.
type PayloadBodyLimitSnapshot struct {
	Configured            bool                              `json:"configured"`
	Mode                  string                            `json:"mode,omitempty"`
	JSONBytes             int64                             `json:"json_bytes,omitempty"`
	MultipartBytes        int64                             `json:"multipart_bytes,omitempty"`
	WebsocketBytes        int64                             `json:"websocket_bytes,omitempty"`
	EmergencyCeilingBytes int64                             `json:"emergency_ceiling_bytes"`
	Total                 PayloadBodyLimitTotals            `json:"total"`
	Kinds                 map[string]PayloadBodyLimitTotals `json:"kinds"`
	Endpoints             map[string]PayloadBodyLimitMetric `json:"endpoints,omitempty"`
}

type payloadBodyLimitMetrics struct {
	mu        sync.Mutex
	total     PayloadBodyLimitTotals
	kinds     map[payloadBodyKind]PayloadBodyLimitTotals
	endpoints map[string]PayloadBodyLimitMetric
}

var globalPayloadBodyLimitMetrics = payloadBodyLimitMetrics{
	kinds:     newPayloadBodyLimitKindMetrics(),
	endpoints: make(map[string]PayloadBodyLimitMetric),
}

func newPayloadBodyLimitKindMetrics() map[payloadBodyKind]PayloadBodyLimitTotals {
	kinds := make(map[payloadBodyKind]PayloadBodyLimitTotals, len(payloadBodyMetricKinds))
	for _, kind := range payloadBodyMetricKinds {
		kinds[kind] = PayloadBodyLimitTotals{}
	}
	return kinds
}

func payloadBodyLimitPolicyFromConfig(cfg *config.SDKConfig) *payloadBodyLimitPolicy {
	if cfg == nil {
		return nil
	}
	settings := cfg.RequestGuards.PayloadBodyLimit
	mode := strings.ToLower(strings.TrimSpace(settings.Mode))
	if mode == "" {
		// Programmatic SDK configurations retain the historical hard-limit
		// behavior until they opt into this policy. File loaders set observe.
		return nil
	}
	if mode != payloadBodyLimitEnforce {
		mode = payloadBodyLimitObserve
	}
	return &payloadBodyLimitPolicy{
		mode:           mode,
		jsonBytes:      normalizePayloadBodySoftLimit(settings.JSONBytes, defaultJSONPayloadBodyBytes),
		multipartBytes: normalizePayloadBodySoftLimit(settings.MultipartBytes, defaultMultipartPayloadBodyBytes),
		websocketBytes: normalizePayloadBodySoftLimit(settings.WebsocketBytes, defaultWebsocketPayloadBodyBytes),
	}
}

func normalizePayloadBodySoftLimit(value, fallback int64) int64 {
	if value <= 0 {
		value = fallback
	}
	if value > EmergencyPayloadBodyBytes {
		return EmergencyPayloadBodyBytes
	}
	return value
}

func (h *BaseAPIHandler) updatePayloadBodyLimitPolicy(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.payloadBodyLimit.Store(payloadBodyLimitPolicyFromConfig(cfg))
}

func injectPayloadBodyLimitPolicy(c *gin.Context, policy *payloadBodyLimitPolicy) {
	if c == nil || policy == nil {
		return
	}
	c.Set(payloadBodyLimitGinKey, policy)
}

func payloadBodyLimitPolicyFromContext(c *gin.Context) *payloadBodyLimitPolicy {
	if c == nil {
		return nil
	}
	value, exists := c.Get(payloadBodyLimitGinKey)
	if !exists {
		return nil
	}
	policy, _ := value.(*payloadBodyLimitPolicy)
	return policy
}

func payloadBodyKindFromRequest(c *gin.Context) payloadBodyKind {
	if c != nil && c.Request != nil {
		if strings.EqualFold(strings.TrimSpace(c.Request.Header.Get("Upgrade")), "websocket") {
			return payloadBodyKindWebsocket
		}
		contentType := strings.ToLower(strings.TrimSpace(c.Request.Header.Get("Content-Type")))
		if strings.HasPrefix(contentType, "multipart/form-data") {
			return payloadBodyKindMultipart
		}
	}
	return payloadBodyKindJSON
}

func payloadBodyLimitDecisionForContext(c *gin.Context, kind payloadBodyKind, fallbackWireBytes, fallbackDecodedBytes int64) payloadBodyLimitDecision {
	fallbackWireBytes = normalizeRequestBodyLimit(fallbackWireBytes)
	fallbackDecodedBytes = normalizeRequestBodyLimit(fallbackDecodedBytes)
	decision := payloadBodyLimitDecision{
		kind:             kind,
		softWireBytes:    fallbackWireBytes,
		softDecodedBytes: fallbackDecodedBytes,
		maxWireBytes:     fallbackWireBytes,
		maxDecodedBytes:  fallbackDecodedBytes,
	}
	policy := payloadBodyLimitPolicyFromContext(c)
	if policy == nil {
		return decision
	}
	softBytes := policy.jsonBytes
	switch kind {
	case payloadBodyKindMultipart:
		softBytes = policy.multipartBytes
	case payloadBodyKindWebsocket:
		softBytes = policy.websocketBytes
	}
	decision.policy = policy
	decision.softWireBytes = softBytes
	decision.softDecodedBytes = softBytes
	decision.maxWireBytes = softBytes
	decision.maxDecodedBytes = softBytes
	if policy.mode == payloadBodyLimitObserve {
		decision.maxWireBytes = EmergencyPayloadBodyBytes
		decision.maxDecodedBytes = EmergencyPayloadBodyBytes
	}
	return decision
}

func effectiveKnownContentLengthLimit(c *gin.Context, fallbackBytes int64) (int64, payloadBodyLimitDecision) {
	kind := payloadBodyKindFromRequest(c)
	decision := payloadBodyLimitDecisionForContext(c, kind, fallbackBytes, fallbackBytes)
	return decision.maxWireBytes, decision
}

func recordKnownContentLengthBodyLimit(c *gin.Context, decision payloadBodyLimitDecision, wireBytes int64) {
	decodedBytes := unknownPayloadBodyBytes
	if c != nil && c.Request != nil && identityContentEncoding(c.Request.Header.Get("Content-Encoding")) {
		decodedBytes = wireBytes
	}
	recordPayloadBodyLimit(c, decision, wireBytes, decodedBytes, true)
}

// WebsocketPayloadBodyLimits returns the physical frame ceiling and the soft
// policy limit. Without an injected public-route policy both values equal the
// caller-provided legacy limit.
func WebsocketPayloadBodyLimits(c *gin.Context, fallbackBytes int64) (physicalBytes, softBytes int64) {
	decision := payloadBodyLimitDecisionForContext(c, payloadBodyKindWebsocket, fallbackBytes, fallbackBytes)
	physicalBytes = decision.maxWireBytes
	fallbackBytes = normalizeRequestBodyLimit(fallbackBytes)
	if fallbackBytes < physicalBytes {
		physicalBytes = fallbackBytes
	}
	return physicalBytes, decision.softWireBytes
}

// RecordWebsocketPayloadBody records a downstream frame without retaining its
// contents. rejected should only be true when the physical frame limit fired.
func RecordWebsocketPayloadBody(c *gin.Context, wireBytes int64, rejected bool) {
	RecordWebsocketPayloadBodyWithLimit(c, wireBytes, defaultWebsocketPayloadBodyBytes, rejected)
}

// RecordWebsocketPayloadBodyWithLimit records a frame together with the
// transport's effective physical ceiling. This keeps health diagnostics honest
// when a shared aggregate byte budget is lower than the configured body policy.
func RecordWebsocketPayloadBodyWithLimit(c *gin.Context, wireBytes, physicalLimitBytes int64, rejected bool) {
	if physicalLimitBytes <= 0 || physicalLimitBytes > EmergencyPayloadBodyBytes {
		physicalLimitBytes = EmergencyPayloadBodyBytes
	}
	decision := payloadBodyLimitDecisionForContext(c, payloadBodyKindWebsocket, physicalLimitBytes, physicalLimitBytes)
	decision.maxWireBytes = min(decision.maxWireBytes, physicalLimitBytes)
	decision.maxDecodedBytes = min(decision.maxDecodedBytes, physicalLimitBytes)
	recordPayloadBodyLimit(c, decision, wireBytes, wireBytes, rejected)
}

func recordPayloadBodyLimit(c *gin.Context, decision payloadBodyLimitDecision, wireBytes, decodedBytes int64, rejected bool) {
	if decision.policy == nil {
		return
	}
	if wireBytes < 0 {
		wireBytes = 0
	}
	decodedKnown := decodedBytes >= 0
	wouldReject := wireBytes > decision.softWireBytes || decodedKnown && decodedBytes > decision.softDecodedBytes
	emergencyRejected := rejected && decision.policy.mode == payloadBodyLimitObserve &&
		(wireBytes > EmergencyPayloadBodyBytes || decodedKnown && decodedBytes > EmergencyPayloadBodyBytes)
	kind := normalizePayloadBodyMetricKind(decision.kind)
	endpoint := payloadBodyLimitMetricKey(c, kind)
	physicalCeilingBytes := effectivePayloadBodyPhysicalCeiling(decision)

	globalPayloadBodyLimitMetrics.mu.Lock()
	defer globalPayloadBodyLimitMetrics.mu.Unlock()
	updatePayloadBodyLimitMetric(&globalPayloadBodyLimitMetrics.total.PayloadBodyLimitMetric, wireBytes, decodedBytes, physicalCeilingBytes, decodedKnown, wouldReject, rejected, emergencyRejected)
	updatePayloadBodyLimitSizeBuckets(&globalPayloadBodyLimitMetrics.total.WireSizeBuckets, wireBytes)
	updatePayloadBodyLimitSizeBuckets(&globalPayloadBodyLimitMetrics.total.DecodedSizeBuckets, decodedBytes)
	kindTotals := globalPayloadBodyLimitMetrics.kinds[kind]
	updatePayloadBodyLimitMetric(&kindTotals.PayloadBodyLimitMetric, wireBytes, decodedBytes, physicalCeilingBytes, decodedKnown, wouldReject, rejected, emergencyRejected)
	updatePayloadBodyLimitSizeBuckets(&kindTotals.WireSizeBuckets, wireBytes)
	updatePayloadBodyLimitSizeBuckets(&kindTotals.DecodedSizeBuckets, decodedBytes)
	globalPayloadBodyLimitMetrics.kinds[kind] = kindTotals
	if _, exists := globalPayloadBodyLimitMetrics.endpoints[endpoint]; !exists && len(globalPayloadBodyLimitMetrics.endpoints) >= payloadBodyMetricMaxKeys {
		endpoint = string(kind) + " other"
	}
	metric := globalPayloadBodyLimitMetrics.endpoints[endpoint]
	updatePayloadBodyLimitMetric(&metric, wireBytes, decodedBytes, physicalCeilingBytes, decodedKnown, wouldReject, rejected, emergencyRejected)
	globalPayloadBodyLimitMetrics.endpoints[endpoint] = metric
}

func updatePayloadBodyLimitMetric(metric *PayloadBodyLimitMetric, wireBytes, decodedBytes, physicalCeilingBytes int64, decodedKnown, wouldReject, rejected, emergencyRejected bool) {
	metric.Requests++
	if wouldReject {
		metric.WouldReject++
	}
	if rejected {
		metric.Rejected++
	}
	if emergencyRejected {
		metric.EmergencyRejected++
	}
	metric.MaxWireBytes = max(metric.MaxWireBytes, wireBytes)
	if decodedKnown {
		metric.MaxDecodedBytes = max(metric.MaxDecodedBytes, decodedBytes)
	}
	if physicalCeilingBytes > 0 && (metric.PhysicalCeilingBytes == 0 || physicalCeilingBytes < metric.PhysicalCeilingBytes) {
		metric.PhysicalCeilingBytes = physicalCeilingBytes
	}
}

func effectivePayloadBodyPhysicalCeiling(decision payloadBodyLimitDecision) int64 {
	ceiling := decision.maxWireBytes
	if decision.maxDecodedBytes > 0 && (ceiling <= 0 || decision.maxDecodedBytes < ceiling) {
		ceiling = decision.maxDecodedBytes
	}
	return ceiling
}

func updatePayloadBodyLimitSizeBuckets(buckets *PayloadBodyLimitSizeBuckets, sizeBytes int64) {
	if buckets == nil {
		return
	}
	if sizeBytes < 0 {
		buckets.Unknown++
		return
	}
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

func normalizePayloadBodyMetricKind(kind payloadBodyKind) payloadBodyKind {
	switch kind {
	case payloadBodyKindJSON, payloadBodyKindMultipart, payloadBodyKindWebsocket:
		return kind
	default:
		return payloadBodyKindOther
	}
}

func payloadBodyLimitMetricKey(c *gin.Context, kind payloadBodyKind) string {
	endpoint := "other"
	if c != nil {
		if fullPath := strings.TrimSpace(c.FullPath()); fullPath != "" {
			endpoint = fullPath
		}
	}
	return string(kind) + " " + endpoint
}

// PayloadBodyLimitSnapshot returns the effective policy and bounded process
// metrics. It is safe to call from health-detail handlers.
func (h *BaseAPIHandler) PayloadBodyLimitSnapshot() PayloadBodyLimitSnapshot {
	snapshot := PayloadBodyLimitSnapshot{EmergencyCeilingBytes: EmergencyPayloadBodyBytes}
	if h != nil {
		if policy := h.payloadBodyLimit.Load(); policy != nil {
			snapshot.Configured = true
			snapshot.Mode = policy.mode
			snapshot.JSONBytes = policy.jsonBytes
			snapshot.MultipartBytes = policy.multipartBytes
			snapshot.WebsocketBytes = policy.websocketBytes
		}
	}
	globalPayloadBodyLimitMetrics.mu.Lock()
	snapshot.Total = globalPayloadBodyLimitMetrics.total
	snapshot.Kinds = make(map[string]PayloadBodyLimitTotals, len(payloadBodyMetricKinds))
	for _, kind := range payloadBodyMetricKinds {
		snapshot.Kinds[string(kind)] = globalPayloadBodyLimitMetrics.kinds[kind]
	}
	if len(globalPayloadBodyLimitMetrics.endpoints) > 0 {
		snapshot.Endpoints = make(map[string]PayloadBodyLimitMetric, len(globalPayloadBodyLimitMetrics.endpoints))
		for key, metric := range globalPayloadBodyLimitMetrics.endpoints {
			snapshot.Endpoints[key] = metric
		}
	}
	globalPayloadBodyLimitMetrics.mu.Unlock()
	return snapshot
}

func resetPayloadBodyLimitMetricsForTest() {
	globalPayloadBodyLimitMetrics.mu.Lock()
	globalPayloadBodyLimitMetrics.total = PayloadBodyLimitTotals{}
	globalPayloadBodyLimitMetrics.kinds = newPayloadBodyLimitKindMetrics()
	globalPayloadBodyLimitMetrics.endpoints = make(map[string]PayloadBodyLimitMetric)
	globalPayloadBodyLimitMetrics.mu.Unlock()
}
