package helps

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const DeepSeekAnthropicCacheDiagnosticEvent = "deepseek_anthropic_cache_diag_v1"

const (
	cacheDiagnosticColdMiss           = "cold_miss"
	cacheDiagnosticHit                = "cache_hit"
	cacheDiagnosticClientChanged      = "client_prefix_changed"
	cacheDiagnosticCPAChanged         = "cpa_prefix_changed"
	cacheDiagnosticRouteChanged       = "route_or_auth_changed"
	cacheDiagnosticStableMiss         = "stable_prefix_miss"
	cacheDiagnosticStableAnomaly      = "stable_prefix_anomaly"
	cacheDiagnosticUsageUnknown       = "usage_unknown"
	cacheDiagnosticDefaultMaxEntries  = 10000
	cacheDiagnosticMaxAnchorsPerEntry = 8
)

// CachePrefixDiagnosticsOptions configures the in-memory, request-body-free
// DeepSeek Anthropic cache diagnostic state.
type CachePrefixDiagnosticsOptions struct {
	Enabled             bool
	SampleRate          float64
	CompareWindow       time.Duration
	StableMissThreshold int
	MaxEntries          int
	Secret              []byte
	KeyID               string
	Now                 func() time.Time
}

// CachePrefixDiagnosticMeta carries raw identities only until Begin hashes them.
// Callers must not persist or log this structure.
type CachePrefixDiagnosticMeta struct {
	Model            string
	SourceFormat     string
	Stream           bool
	RouteKind        string
	BaseHost         string
	Endpoint         string
	ConsumerIdentity string
	AuthIdentity     string
	AffinityIdentity string
	AffinityHit      bool
}

// CachePrefixFingerprint contains only truncated HMAC values and counts.
type CachePrefixFingerprint struct {
	System                 string
	Tools                  string
	HistoryWithoutLastUser string
	LastUser               string
	FullCacheablePrefix    string
	WholePrompt            string
	MessageAnchors         []string
	MessageCount           int
}

type cacheControlDiagnostic struct {
	Total int
	TTL5m int
	TTL1h int
}

type cachePrefixHistory struct {
	ExpiresAt             time.Time
	RouteSignature        string
	AuthBucket            string
	Received              CachePrefixFingerprint
	Compatible            CachePrefixFingerprint
	Upstream              CachePrefixFingerprint
	TurnEndAnchors        []string
	ConsecutiveStableMiss int
	AnomalyEmitted        bool
}

// CachePrefixDiagnostics stores bounded, HMAC-only comparison state.
type CachePrefixDiagnostics struct {
	secret              []byte
	keyID               string
	sampleRate          float64
	compareWindow       time.Duration
	stableMissThreshold int
	maxEntries          int
	now                 func() time.Time

	mu      sync.Mutex
	history map[string]cachePrefixHistory
}

// CachePrefixDiagnosticSession represents one sampled request. It never keeps
// the original request body or response text.
type CachePrefixDiagnosticSession struct {
	diagnostics *CachePrefixDiagnostics
	meta        cachePrefixDiagnosticHashedMeta
	received    CachePrefixFingerprint
	compatible  CachePrefixFingerprint
	upstream    CachePrefixFingerprint
	cacheIn     cacheControlDiagnostic
	cacheOut    cacheControlDiagnostic

	firstChangedStage string
	firstChangedPart  string
	stream            cacheDiagnosticStreamState
	completed         bool
}

type cachePrefixDiagnosticHashedMeta struct {
	Model          string
	SourceFormat   string
	Stream         bool
	RouteKind      string
	BaseHost       string
	Endpoint       string
	ConsumerBucket string
	AuthBucket     string
	AffinityBucket string
	AffinityHit    bool
	RouteSignature string
}

type cacheDiagnosticUsage struct {
	InputTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	OutputTokens        int64
	CacheFieldsPresent  bool
	UsagePresent        bool
	Source              string
}

type cacheDiagnosticStreamState struct {
	started      bool
	stopped      bool
	valid        bool
	blocks       map[int]*cacheDiagnosticStreamBlock
	blockDigests map[int]string
	usage        cacheDiagnosticUsage
}

type cacheDiagnosticStreamBlock struct {
	blockType       string
	static          []byte
	textDigest      hash.Hash
	thinkingDigest  hash.Hash
	signatureDigest hash.Hash
	hasText         bool
	hasThinking     bool
	hasSignature    bool
	inputDigest     string
}

// CachePrefixDiagnosticEvent is safe for structured logs. It contains no raw
// prompt, tool name, credential, session ID, or response content.
type CachePrefixDiagnosticEvent struct {
	Model                    string
	SourceFormat             string
	Stream                   bool
	RouteKind                string
	BaseHost                 string
	Endpoint                 string
	ConsumerKeyBucket        string
	UpstreamAuthBucket       string
	AffinityBucket           string
	AffinityHit              bool
	PrefixAnchorMatch        string
	MatchedAnchorBucket      string
	Received                 CachePrefixFingerprint
	ProviderCompatible       CachePrefixFingerprint
	UpstreamFinal            CachePrefixFingerprint
	TurnEndPrefixAnchor      string
	FirstChangedStage        string
	FirstChangedPart         string
	CacheControlIn           int
	CacheControlOut          int
	CacheControl5mIn         int
	CacheControl1hIn         int
	CacheControl5mOut        int
	CacheControl1hOut        int
	CacheControlIgnored      bool
	InputTokens              int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	OutputTokens             int64
	CacheReadRatio           float64
	UsageSource              string
	UsageCacheFieldsMissing  bool
	Classification           string
	Sampled                  bool
	DiagnosticVersion        int
	HMACKeyID                string
}

// NewCachePrefixDiagnostics returns nil when diagnostics are disabled or the
// independent HMAC secret is absent. Business traffic must continue unchanged.
func NewCachePrefixDiagnostics(opts CachePrefixDiagnosticsOptions) *CachePrefixDiagnostics {
	if !opts.Enabled || len(opts.Secret) == 0 {
		return nil
	}
	if opts.SampleRate < 0 {
		opts.SampleRate = 0
	}
	if opts.SampleRate > 1 {
		opts.SampleRate = 1
	}
	if opts.CompareWindow <= 0 {
		opts.CompareWindow = 10 * time.Minute
	}
	if opts.StableMissThreshold <= 0 {
		opts.StableMissThreshold = 3
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = cacheDiagnosticDefaultMaxEntries
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &CachePrefixDiagnostics{
		secret:              append([]byte(nil), opts.Secret...),
		keyID:               strings.TrimSpace(opts.KeyID),
		sampleRate:          opts.SampleRate,
		compareWindow:       opts.CompareWindow,
		stableMissThreshold: opts.StableMissThreshold,
		maxEntries:          opts.MaxEntries,
		now:                 opts.Now,
		history:             make(map[string]cachePrefixHistory),
	}
}

// Begin fingerprints three request stages and returns nil when the request is
// not sampled or cannot be represented as Anthropic Messages semantics.
func (d *CachePrefixDiagnostics) Begin(meta CachePrefixDiagnosticMeta, received, providerCompatible, upstreamFinal []byte) *CachePrefixDiagnosticSession {
	if d == nil || d.sampleRate <= 0 {
		return nil
	}
	// Decide sampling with a one-pass HMAC over the received bytes before the
	// more expensive semantic normalization. The sampling digest is never logged.
	if !d.sample(d.digestParts("sample", received)) {
		return nil
	}
	receivedFingerprint, ok := d.fingerprintMessages(received)
	if !ok {
		return nil
	}
	compatibleFingerprint := receivedFingerprint
	okCompatible := true
	if !cacheDiagnosticSemanticBytesEqual(received, providerCompatible) {
		compatibleFingerprint, okCompatible = d.fingerprintMessages(providerCompatible)
	}
	upstreamFingerprint := compatibleFingerprint
	okUpstream := true
	if !cacheDiagnosticSemanticBytesEqual(providerCompatible, upstreamFinal) {
		if cacheDiagnosticSemanticBytesEqual(received, upstreamFinal) {
			upstreamFingerprint = receivedFingerprint
		} else {
			upstreamFingerprint, okUpstream = d.fingerprintMessages(upstreamFinal)
		}
	}
	if !okCompatible || !okUpstream {
		return nil
	}

	hashed := cachePrefixDiagnosticHashedMeta{
		Model:          strings.TrimSpace(meta.Model),
		SourceFormat:   strings.TrimSpace(meta.SourceFormat),
		Stream:         meta.Stream,
		RouteKind:      strings.TrimSpace(meta.RouteKind),
		BaseHost:       strings.TrimSpace(meta.BaseHost),
		Endpoint:       strings.TrimSpace(meta.Endpoint),
		ConsumerBucket: d.digestString("consumer", meta.ConsumerIdentity),
		AuthBucket:     d.digestString("auth", meta.AuthIdentity),
		AffinityBucket: d.digestString("affinity", meta.AffinityIdentity),
		AffinityHit:    meta.AffinityHit,
	}
	hashed.RouteSignature = d.digestParts("route", []byte(hashed.RouteKind), []byte(hashed.BaseHost), []byte(hashed.Endpoint), []byte(hashed.Model))

	return &CachePrefixDiagnosticSession{
		diagnostics:       d,
		meta:              hashed,
		received:          receivedFingerprint,
		compatible:        compatibleFingerprint,
		upstream:          upstreamFingerprint,
		cacheIn:           countCacheControls(received),
		cacheOut:          countCacheControls(upstreamFinal),
		firstChangedStage: "none",
		firstChangedPart:  "none",
		stream: cacheDiagnosticStreamState{
			valid:        true,
			blocks:       make(map[int]*cacheDiagnosticStreamBlock),
			blockDigests: make(map[int]string),
		},
	}
}

func cacheDiagnosticSemanticBytesEqual(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	leftParts := gjson.GetManyBytes(left, "system", "tools", "messages")
	rightParts := gjson.GetManyBytes(right, "system", "tools", "messages")
	if len(leftParts) != len(rightParts) {
		return false
	}
	for index := range leftParts {
		if leftParts[index].Exists() != rightParts[index].Exists() || leftParts[index].Raw != rightParts[index].Raw {
			return false
		}
	}
	return true
}

func (d *CachePrefixDiagnostics) sample(digest string) bool {
	if d.sampleRate >= 1 {
		return true
	}
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) < 8 {
		return false
	}
	value := binary.BigEndian.Uint64(raw[:8])
	return float64(value)/float64(^uint64(0)) < d.sampleRate
}

// CompleteNonStream closes a non-streaming response diagnostic.
func (s *CachePrefixDiagnosticSession) CompleteNonStream(data []byte) (CachePrefixDiagnosticEvent, bool) {
	if s == nil || s.completed || !gjson.ValidBytes(data) {
		return CachePrefixDiagnosticEvent{}, false
	}
	content := gjson.GetBytes(data, "content")
	var contentValue any
	if !content.Exists() || json.Unmarshal([]byte(content.Raw), &contentValue) != nil {
		return CachePrefixDiagnosticEvent{}, false
	}
	usage := cacheDiagnosticUsageFromNode(gjson.GetBytes(data, "usage"), "non_stream_final")
	messageDigest, okDigest := s.diagnostics.messageDigest(map[string]any{"role": "assistant", "content": contentValue})
	if !okDigest {
		return CachePrefixDiagnosticEvent{}, false
	}
	turnEnd := s.diagnostics.turnEndAnchor(s.upstream.WholePrompt, messageDigest)
	return s.finish(usage, turnEnd)
}

// ObserveStreamLine incrementally processes one upstream Anthropic SSE line.
// It emits exactly once, only after a complete message_stop event.
func (s *CachePrefixDiagnosticSession) ObserveStreamLine(line []byte) (CachePrefixDiagnosticEvent, bool) {
	if s == nil || s.completed {
		return CachePrefixDiagnosticEvent{}, false
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return CachePrefixDiagnosticEvent{}, false
	}
	root := gjson.ParseBytes(payload)
	eventType := root.Get("type").String()
	s.observeStreamUsage(root, eventType)
	s.observeStreamContent(root, eventType)
	if eventType != "message_stop" {
		return CachePrefixDiagnosticEvent{}, false
	}
	s.stream.stopped = true
	if !s.stream.started {
		return CachePrefixDiagnosticEvent{}, false
	}
	turnEnd := ""
	if s.stream.valid {
		messageDigest := s.diagnostics.messageDigestFromBlockDigests("assistant", nil, s.stream.blockDigests)
		turnEnd = s.diagnostics.turnEndAnchor(s.upstream.WholePrompt, messageDigest)
	}
	return s.finish(s.stream.usage, turnEnd)
}

func (s *CachePrefixDiagnosticSession) observeStreamUsage(root gjson.Result, eventType string) {
	var node gjson.Result
	source := ""
	switch eventType {
	case "message_start":
		node = root.Get("message.usage")
		source = "message_start"
	case "message_delta":
		node = root.Get("usage")
		source = "message_delta"
	default:
		node = root.Get("usage")
		source = eventType
	}
	if !node.Exists() || node.Type == gjson.Null {
		return
	}
	observed := cacheDiagnosticUsageFromNode(node, source)
	s.stream.usage.UsagePresent = s.stream.usage.UsagePresent || observed.UsagePresent
	s.stream.usage.CacheFieldsPresent = s.stream.usage.CacheFieldsPresent || observed.CacheFieldsPresent
	if observed.InputTokens != 0 || node.Get("input_tokens").Exists() {
		s.stream.usage.InputTokens = observed.InputTokens
	}
	if observed.CacheReadTokens != 0 || node.Get("cache_read_input_tokens").Exists() {
		s.stream.usage.CacheReadTokens = observed.CacheReadTokens
	}
	if observed.CacheCreationTokens != 0 || node.Get("cache_creation_input_tokens").Exists() {
		s.stream.usage.CacheCreationTokens = observed.CacheCreationTokens
	}
	if observed.OutputTokens != 0 || node.Get("output_tokens").Exists() {
		s.stream.usage.OutputTokens = observed.OutputTokens
	}
	if source != "" {
		if s.stream.usage.Source == "" {
			s.stream.usage.Source = source
		} else if !strings.Contains(s.stream.usage.Source, source) {
			s.stream.usage.Source += "+" + source
		}
	}
}

func (s *CachePrefixDiagnosticSession) observeStreamContent(root gjson.Result, eventType string) {
	switch eventType {
	case "message_start":
		s.stream.started = true
	case "content_block_start":
		index := int(root.Get("index").Int())
		var block map[string]any
		if raw := root.Get("content_block"); !raw.Exists() || json.Unmarshal([]byte(raw.Raw), &block) != nil {
			s.stream.valid = false
			return
		}
		s.stream.blocks[index] = s.diagnostics.newStreamBlock(block)
	case "content_block_delta":
		index := int(root.Get("index").Int())
		block := s.stream.blocks[index]
		if block == nil {
			s.stream.valid = false
			return
		}
		delta := root.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			block.hasText = true
			_, _ = block.textDigest.Write([]byte(delta.Get("text").String()))
		case "thinking_delta":
			block.hasThinking = true
			_, _ = block.thinkingDigest.Write([]byte(delta.Get("thinking").String()))
		case "signature_delta":
			block.hasSignature = true
			_, _ = block.signatureDigest.Write([]byte(delta.Get("signature").String()))
		case "input_json_delta":
			// Reconstructing arbitrary partial JSON would retain the complete tool
			// input. Keep request/usage diagnostics but omit the turn-end anchor.
			s.stream.valid = false
		default:
			s.stream.valid = false
		}
	case "content_block_stop":
		index := int(root.Get("index").Int())
		block := s.stream.blocks[index]
		if block == nil {
			s.stream.valid = false
			return
		}
		s.stream.blockDigests[index] = s.diagnostics.finishStreamBlock(block)
		delete(s.stream.blocks, index)
	}
}

func (s *CachePrefixDiagnosticSession) finish(usage cacheDiagnosticUsage, turnEnd string) (CachePrefixDiagnosticEvent, bool) {
	if s == nil || s.completed {
		return CachePrefixDiagnosticEvent{}, false
	}
	s.completed = true
	classification, prefixMatch, matchedAnchor := s.diagnostics.classifyAndStore(s, usage, turnEnd)
	ratio := float64(0)
	inputTotal := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if inputTotal > 0 {
		ratio = float64(usage.CacheReadTokens) / float64(inputTotal)
	}
	return CachePrefixDiagnosticEvent{
		Model:                    s.meta.Model,
		SourceFormat:             s.meta.SourceFormat,
		Stream:                   s.meta.Stream,
		RouteKind:                s.meta.RouteKind,
		BaseHost:                 s.meta.BaseHost,
		Endpoint:                 s.meta.Endpoint,
		ConsumerKeyBucket:        s.meta.ConsumerBucket,
		UpstreamAuthBucket:       s.meta.AuthBucket,
		AffinityBucket:           s.meta.AffinityBucket,
		AffinityHit:              s.meta.AffinityHit,
		PrefixAnchorMatch:        prefixMatch,
		MatchedAnchorBucket:      matchedAnchor,
		Received:                 s.received,
		ProviderCompatible:       s.compatible,
		UpstreamFinal:            s.upstream,
		TurnEndPrefixAnchor:      turnEnd,
		FirstChangedStage:        s.firstChangedStage,
		FirstChangedPart:         s.firstChangedPart,
		CacheControlIn:           s.cacheIn.Total,
		CacheControlOut:          s.cacheOut.Total,
		CacheControl5mIn:         s.cacheIn.TTL5m,
		CacheControl1hIn:         s.cacheIn.TTL1h,
		CacheControl5mOut:        s.cacheOut.TTL5m,
		CacheControl1hOut:        s.cacheOut.TTL1h,
		CacheControlIgnored:      true,
		InputTokens:              usage.InputTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		CacheCreationInputTokens: usage.CacheCreationTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadRatio:           ratio,
		UsageSource:              usage.Source,
		UsageCacheFieldsMissing:  !usage.CacheFieldsPresent,
		Classification:           classification,
		Sampled:                  true,
		DiagnosticVersion:        1,
		HMACKeyID:                s.diagnostics.keyID,
	}, true
}

func (d *CachePrefixDiagnostics) classifyAndStore(s *CachePrefixDiagnosticSession, usage cacheDiagnosticUsage, turnEnd string) (classification, prefixMatch, matchedAnchor string) {
	if d == nil {
		return cacheDiagnosticUsageUnknown, "none", ""
	}
	now := d.now()
	key := s.meta.AffinityBucket
	if key != "" {
		key = s.meta.ConsumerBucket + ":" + key + ":" + s.meta.Model
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)
	previous, hasPrevious := d.history[key]
	if key == "" {
		hasPrevious = false
	}

	if hasPrevious {
		s.meta.AffinityHit = true
		for _, anchor := range previous.TurnEndAnchors {
			if containsString(s.upstream.MessageAnchors, anchor) {
				prefixMatch = "prior_turn_end"
				matchedAnchor = anchor
				break
			}
		}
	}
	if prefixMatch == "" {
		prefixMatch = "none"
	}

	stable := hasPrevious && s.upstream.WholePrompt == previous.Upstream.WholePrompt
	cpaChanged := hasPrevious &&
		s.received.WholePrompt == previous.Received.WholePrompt &&
		s.upstream.WholePrompt != previous.Upstream.WholePrompt
	if cpaChanged {
		if part := firstCacheDiagnosticPartChange(previous.Compatible, s.compatible); part != "none" {
			s.firstChangedStage = "provider_compatible"
			s.firstChangedPart = part
		} else {
			s.firstChangedStage = "upstream_final"
			s.firstChangedPart = firstCacheDiagnosticPartChange(previous.Upstream, s.upstream)
		}
	}
	switch {
	case !usage.UsagePresent || !usage.CacheFieldsPresent:
		classification = cacheDiagnosticUsageUnknown
	case usage.CacheReadTokens > 0:
		classification = cacheDiagnosticHit
	case !hasPrevious:
		classification = cacheDiagnosticColdMiss
	case previous.RouteSignature != s.meta.RouteSignature || previous.AuthBucket != s.meta.AuthBucket:
		classification = cacheDiagnosticRouteChanged
	case cpaChanged:
		classification = cacheDiagnosticCPAChanged
	case stable || prefixMatch == "prior_turn_end":
		previous.ConsecutiveStableMiss++
		if previous.ConsecutiveStableMiss >= d.stableMissThreshold && !previous.AnomalyEmitted {
			classification = cacheDiagnosticStableAnomaly
			previous.AnomalyEmitted = true
		} else {
			classification = cacheDiagnosticStableMiss
		}
	default:
		classification = cacheDiagnosticClientChanged
	}

	if classification != cacheDiagnosticStableMiss && classification != cacheDiagnosticStableAnomaly {
		previous.ConsecutiveStableMiss = 0
		previous.AnomalyEmitted = false
	}
	anchors := append([]string(nil), previous.TurnEndAnchors...)
	if turnEnd != "" {
		anchors = append(anchors, turnEnd)
		if len(anchors) > cacheDiagnosticMaxAnchorsPerEntry {
			anchors = anchors[len(anchors)-cacheDiagnosticMaxAnchorsPerEntry:]
		}
	}
	if key != "" {
		storedReceived := s.received
		storedReceived.MessageAnchors = nil
		storedCompatible := s.compatible
		storedCompatible.MessageAnchors = nil
		storedUpstream := s.upstream
		storedUpstream.MessageAnchors = nil
		d.history[key] = cachePrefixHistory{
			ExpiresAt:             now.Add(d.compareWindow),
			RouteSignature:        s.meta.RouteSignature,
			AuthBucket:            s.meta.AuthBucket,
			Received:              storedReceived,
			Compatible:            storedCompatible,
			Upstream:              storedUpstream,
			TurnEndAnchors:        anchors,
			ConsecutiveStableMiss: previous.ConsecutiveStableMiss,
			AnomalyEmitted:        previous.AnomalyEmitted,
		}
	}
	return classification, prefixMatch, matchedAnchor
}

func (d *CachePrefixDiagnostics) pruneLocked(now time.Time) {
	for key, item := range d.history {
		if !item.ExpiresAt.After(now) {
			delete(d.history, key)
		}
	}
	for len(d.history) >= d.maxEntries {
		var oldestKey string
		var oldest time.Time
		for key, item := range d.history {
			if oldestKey == "" || item.ExpiresAt.Before(oldest) {
				oldestKey = key
				oldest = item.ExpiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(d.history, oldestKey)
	}
}

func (d *CachePrefixDiagnostics) fingerprintMessages(body []byte) (CachePrefixFingerprint, bool) {
	if d == nil || !gjson.ValidBytes(body) {
		return CachePrefixFingerprint{}, false
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return CachePrefixFingerprint{}, false
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return CachePrefixFingerprint{}, false
	}
	system := canonicalCacheDiagnosticValue(root["system"])
	tools := canonicalCacheDiagnosticValue(root["tools"])
	chain := d.digestParts("prefix_root")
	chain = d.chainDigest(chain, "system", system)
	systemAnchor := chain
	chain = d.chainDigest(chain, "tools", tools)
	toolsAnchor := chain
	anchors := []string{systemAnchor, toolsAnchor}

	lastUserIndex := -1
	if len(messages) > 0 {
		message, isMap := messages[len(messages)-1].(map[string]any)
		if isMap && strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "user") {
			lastUserIndex = len(messages) - 1
		}
	}
	historyAnchor := chain
	lastUserDigest := d.digestParts("last_user", canonicalCacheDiagnosticValue(nil))
	for i, message := range messages {
		messageDigest, okMessage := d.messageDigest(message)
		if !okMessage {
			return CachePrefixFingerprint{}, false
		}
		if i == lastUserIndex {
			historyAnchor = chain
			lastUserDigest = d.digestParts("last_user", []byte(messageDigest))
		}
		chain = d.chainDigest(chain, "message", []byte(messageDigest))
		anchors = append(anchors, chain)
	}
	if lastUserIndex < 0 {
		historyAnchor = chain
	}
	fullCacheable := chain
	if lastUserIndex >= 0 {
		fullCacheable = historyAnchor
	}
	return CachePrefixFingerprint{
		System:                 d.digestParts("system", system),
		Tools:                  d.digestParts("tools", tools),
		HistoryWithoutLastUser: historyAnchor,
		LastUser:               lastUserDigest,
		FullCacheablePrefix:    fullCacheable,
		WholePrompt:            chain,
		MessageAnchors:         anchors,
		MessageCount:           len(messages),
	}, true
}

func (d *CachePrefixDiagnostics) turnEndAnchor(promptAnchor, messageDigest string) string {
	if d == nil || promptAnchor == "" || messageDigest == "" {
		return ""
	}
	return d.chainDigest(promptAnchor, "message", []byte(messageDigest))
}

func (d *CachePrefixDiagnostics) messageDigest(message any) (string, bool) {
	messageMap, ok := message.(map[string]any)
	if !ok {
		return "", false
	}
	role := strings.ToLower(strings.TrimSpace(stringValue(messageMap["role"])))
	static := make(map[string]any, len(messageMap))
	for key, value := range messageMap {
		if key == "role" || key == "content" || key == "cache_control" {
			continue
		}
		static[key] = stripCacheDiagnosticControl(value)
	}
	staticBytes := canonicalCacheDiagnosticValue(static)
	content, exists := messageMap["content"]
	if !exists {
		return d.digestParts("message", []byte(role), staticBytes, []byte("missing")), true
	}
	switch typed := content.(type) {
	case string:
		valueDigest := d.digestValue("message_string", []byte(typed))
		return d.digestParts("message", []byte(role), staticBytes, []byte("string"), []byte(valueDigest)), true
	case []any:
		blockDigests := make(map[int]string, len(typed))
		for index, block := range typed {
			blockDigest, okBlock := d.contentBlockDigest(block)
			if !okBlock {
				return "", false
			}
			blockDigests[index] = blockDigest
		}
		return d.messageDigestFromBlockDigests(role, staticBytes, blockDigests), true
	default:
		return "", false
	}
}

func (d *CachePrefixDiagnostics) messageDigestFromBlockDigests(role string, static []byte, blockDigests map[int]string) string {
	if static == nil {
		static = canonicalCacheDiagnosticValue(map[string]any{})
	}
	indexes := make([]int, 0, len(blockDigests))
	for index := range blockDigests {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	parts := make([][]byte, 0, 3+len(indexes))
	parts = append(parts, []byte(strings.ToLower(strings.TrimSpace(role))), static, []byte("blocks"))
	for _, index := range indexes {
		parts = append(parts, []byte(blockDigests[index]))
	}
	return d.digestParts("message", parts...)
}

func (d *CachePrefixDiagnostics) contentBlockDigest(block any) (string, bool) {
	blockMap, ok := block.(map[string]any)
	if !ok {
		return "", false
	}
	blockType := strings.TrimSpace(stringValue(blockMap["type"]))
	static := make(map[string]any, len(blockMap))
	for key, value := range blockMap {
		switch key {
		case "type", "text", "thinking", "signature", "input", "cache_control":
			continue
		default:
			static[key] = stripCacheDiagnosticControl(value)
		}
	}
	return d.digestParts(
		"content_block",
		[]byte(blockType),
		canonicalCacheDiagnosticValue(static),
		[]byte(d.optionalStringDigest("block_text", blockMap, "text")),
		[]byte(d.optionalStringDigest("block_thinking", blockMap, "thinking")),
		[]byte(d.optionalStringDigest("block_signature", blockMap, "signature")),
		[]byte(d.optionalValueDigest("block_input", blockMap, "input")),
	), true
}

func (d *CachePrefixDiagnostics) newStreamBlock(block map[string]any) *cacheDiagnosticStreamBlock {
	streamBlock := &cacheDiagnosticStreamBlock{
		blockType:       strings.TrimSpace(stringValue(block["type"])),
		textDigest:      d.newValueDigest("block_text"),
		thinkingDigest:  d.newValueDigest("block_thinking"),
		signatureDigest: d.newValueDigest("block_signature"),
	}
	static := make(map[string]any, len(block))
	for key, value := range block {
		switch key {
		case "type", "text", "thinking", "signature", "input", "cache_control":
			continue
		default:
			static[key] = stripCacheDiagnosticControl(value)
		}
	}
	streamBlock.static = canonicalCacheDiagnosticValue(static)
	if value, exists := block["text"].(string); exists {
		streamBlock.hasText = true
		_, _ = streamBlock.textDigest.Write([]byte(value))
	}
	if value, exists := block["thinking"].(string); exists {
		streamBlock.hasThinking = true
		_, _ = streamBlock.thinkingDigest.Write([]byte(value))
	}
	if value, exists := block["signature"].(string); exists {
		streamBlock.hasSignature = true
		_, _ = streamBlock.signatureDigest.Write([]byte(value))
	}
	if value, exists := block["input"]; exists {
		streamBlock.inputDigest = d.digestValue("block_input", canonicalCacheDiagnosticValue(value))
	}
	return streamBlock
}

func (d *CachePrefixDiagnostics) finishStreamBlock(block *cacheDiagnosticStreamBlock) string {
	if d == nil || block == nil {
		return ""
	}
	return d.digestParts(
		"content_block",
		[]byte(block.blockType),
		block.static,
		[]byte(optionalFinishedDigest(block.hasText, block.textDigest)),
		[]byte(optionalFinishedDigest(block.hasThinking, block.thinkingDigest)),
		[]byte(optionalFinishedDigest(block.hasSignature, block.signatureDigest)),
		[]byte(block.inputDigest),
	)
}

func (d *CachePrefixDiagnostics) optionalStringDigest(label string, values map[string]any, key string) string {
	value, exists := values[key]
	if !exists {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return d.digestValue(label, []byte(text))
}

func (d *CachePrefixDiagnostics) optionalValueDigest(label string, values map[string]any, key string) string {
	value, exists := values[key]
	if !exists {
		return ""
	}
	return d.digestValue(label, canonicalCacheDiagnosticValue(value))
}

func (d *CachePrefixDiagnostics) newValueDigest(label string) hash.Hash {
	mac := hmac.New(sha256.New, d.secret)
	writeFramed(mac, []byte(label))
	return mac
}

func (d *CachePrefixDiagnostics) digestValue(label string, value []byte) string {
	mac := d.newValueDigest(label)
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func optionalFinishedDigest(exists bool, digest hash.Hash) string {
	if !exists || digest == nil {
		return ""
	}
	return hex.EncodeToString(digest.Sum(nil)[:16])
}

func (d *CachePrefixDiagnostics) chainDigest(previous, label string, value []byte) string {
	previousBytes, err := hex.DecodeString(previous)
	if err != nil {
		previousBytes = []byte(previous)
	}
	return d.digestParts("chain", previousBytes, []byte(label), value)
}

func (d *CachePrefixDiagnostics) digestString(label, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return d.digestParts(label, []byte(value))
}

func (d *CachePrefixDiagnostics) digestParts(label string, parts ...[]byte) string {
	mac := hmac.New(sha256.New, d.secret)
	writeFramed(mac, []byte(label))
	for _, part := range parts {
		writeFramed(mac, part)
	}
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

type framedWriter interface {
	Write([]byte) (int, error)
}

func writeFramed(writer framedWriter, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func canonicalCacheDiagnosticValue(value any) []byte {
	normalized := stripCacheDiagnosticControl(value)
	data, err := json.Marshal(normalized)
	if err != nil {
		return []byte("null")
	}
	return data
}

func stripCacheDiagnosticControl(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "cache_control" {
				continue
			}
			out[key] = stripCacheDiagnosticControl(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = stripCacheDiagnosticControl(typed[i])
		}
		return out
	default:
		return typed
	}
}

func countCacheControls(body []byte) cacheControlDiagnostic {
	if !gjson.ValidBytes(body) {
		return cacheControlDiagnostic{}
	}
	var result cacheControlDiagnostic
	var walk func(gjson.Result)
	walk = func(node gjson.Result) {
		if !node.IsArray() && !node.IsObject() {
			return
		}
		node.ForEach(func(key, child gjson.Result) bool {
			if node.IsObject() && key.String() == "cache_control" {
				result.Total++
				switch strings.ToLower(strings.TrimSpace(child.Get("ttl").String())) {
				case "5m":
					result.TTL5m++
				case "1h":
					result.TTL1h++
				}
				return true
			}
			walk(child)
			return true
		})
	}
	walk(gjson.ParseBytes(body))
	return result
}

func cacheDiagnosticUsageFromNode(node gjson.Result, source string) cacheDiagnosticUsage {
	if !node.Exists() || node.Type == gjson.Null || !node.IsObject() {
		return cacheDiagnosticUsage{}
	}
	return cacheDiagnosticUsage{
		InputTokens:         node.Get("input_tokens").Int(),
		CacheReadTokens:     node.Get("cache_read_input_tokens").Int(),
		CacheCreationTokens: node.Get("cache_creation_input_tokens").Int(),
		OutputTokens:        node.Get("output_tokens").Int(),
		CacheFieldsPresent:  node.Get("cache_read_input_tokens").Exists() || node.Get("cache_creation_input_tokens").Exists(),
		UsagePresent:        true,
		Source:              source,
	}
}

func firstCacheDiagnosticPartChange(before, after CachePrefixFingerprint) string {
	switch {
	case before.System != after.System:
		return "system"
	case before.Tools != after.Tools:
		return "tools"
	case before.HistoryWithoutLastUser != after.HistoryWithoutLastUser:
		return "history_without_last_user"
	case before.LastUser != after.LastUser:
		return "last_user"
	case before.FullCacheablePrefix != after.FullCacheablePrefix:
		return "full_cacheable_prefix"
	case before.WholePrompt != after.WholePrompt:
		return "whole_prompt"
	default:
		return "none"
	}
}

func containsString(values []string, want string) bool {
	if want == "" {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// Fields returns the low-sensitivity structured log representation.
func (e CachePrefixDiagnosticEvent) Fields() map[string]any {
	return map[string]any{
		"event":                             DeepSeekAnthropicCacheDiagnosticEvent,
		"model":                             e.Model,
		"source_format":                     e.SourceFormat,
		"stream":                            e.Stream,
		"route_kind":                        e.RouteKind,
		"base_host":                         e.BaseHost,
		"endpoint":                          e.Endpoint,
		"consumer_key_bucket":               e.ConsumerKeyBucket,
		"upstream_auth_bucket":              e.UpstreamAuthBucket,
		"affinity_bucket":                   e.AffinityBucket,
		"affinity_hit":                      e.AffinityHit,
		"prefix_anchor_match":               e.PrefixAnchorMatch,
		"matched_anchor_bucket":             e.MatchedAnchorBucket,
		"received":                          cachePrefixFingerprintFields(e.Received),
		"provider_compatible":               cachePrefixFingerprintFields(e.ProviderCompatible),
		"upstream_final":                    cachePrefixFingerprintFields(e.UpstreamFinal),
		"turn_end_prefix_anchor":            e.TurnEndPrefixAnchor,
		"first_changed_stage":               e.FirstChangedStage,
		"first_changed_part":                e.FirstChangedPart,
		"cache_control_in":                  e.CacheControlIn,
		"cache_control_out":                 e.CacheControlOut,
		"cache_control_5m_in":               e.CacheControl5mIn,
		"cache_control_1h_in":               e.CacheControl1hIn,
		"cache_control_5m_out":              e.CacheControl5mOut,
		"cache_control_1h_out":              e.CacheControl1hOut,
		"cache_control_ignored_by_provider": e.CacheControlIgnored,
		"input_tokens":                      e.InputTokens,
		"cache_read_input_tokens":           e.CacheReadInputTokens,
		"cache_creation_input_tokens":       e.CacheCreationInputTokens,
		"output_tokens":                     e.OutputTokens,
		"cache_read_ratio":                  e.CacheReadRatio,
		"usage_source":                      e.UsageSource,
		"usage_cache_fields_missing":        e.UsageCacheFieldsMissing,
		"classification":                    e.Classification,
		"sampled":                           e.Sampled,
		"diag_version":                      e.DiagnosticVersion,
		"hmac_key_id":                       e.HMACKeyID,
	}
}

func cachePrefixFingerprintFields(fingerprint CachePrefixFingerprint) map[string]any {
	return map[string]any{
		"system":                    fingerprint.System,
		"tools":                     fingerprint.Tools,
		"history_without_last_user": fingerprint.HistoryWithoutLastUser,
		"last_user":                 fingerprint.LastUser,
		"full_cacheable_prefix":     fingerprint.FullCacheablePrefix,
		"whole_prompt":              fingerprint.WholePrompt,
		"message_count":             fingerprint.MessageCount,
	}
}
