package usage

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultServiceTier is used when a request does not specify service_tier.
const DefaultServiceTier = "default"

const defaultQueueSize = 512

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider string
	// ExecutorType stores the concrete executor type that handled the request.
	ExecutorType string
	Model        string
	Alias        string
	APIKey       string
	AuthID       string
	AuthIndex    string
	AuthType     string
	Source       string
	// RequestID links all upstream attempts for one client request.
	RequestID string
	// AttemptNo is the 1-based upstream attempt number within the request.
	AttemptNo int
	// RetryReason explains why this attempt followed a previous failure.
	RetryReason string
	// FinalSuccess is filled when the request's final outcome is known.
	FinalSuccess *bool
	// ReasoningEffort stores the translated upstream thinking level for request event logs.
	ReasoningEffort string
	// ServiceTier stores the client-requested service tier for request event logs.
	ServiceTier string
	// MessageCount stores a safe count of inbound message/input items.
	MessageCount int
	// ToolCount stores a safe count of inbound tool definitions or tool-call items.
	ToolCount   int
	RequestedAt time.Time
	Latency     time.Duration
	TTFT        time.Duration
	Failed      bool
	// ProviderStatusCode stores the upstream HTTP status for failed requests.
	ProviderStatusCode int
	// ErrorCode stores a short provider error code only; raw messages and bodies are never stored here.
	ErrorCode string
	Fail      Failure
	Detail    Detail
	// ResponseHeaders stores a snapshot of upstream response headers for usage sinks.
	ResponseHeaders http.Header
}

// Failure holds HTTP failure metadata for an upstream request attempt.
type Failure struct {
	StatusCode int
	ErrorCode  string
	Body       string
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// RequestAttempt stores request-scoped retry metadata for usage sinks.
type RequestAttempt struct {
	RequestID   string
	AttemptNo   int
	RetryReason string
}

// RequestShape stores safe inbound request shape counters.
type RequestShape struct {
	MessageCount int
	ToolCount    int
}

// ToolShape stores safe redacted inbound tool-shape telemetry.
type ToolShape struct {
	ToolTypes         string
	ToolNameHashes    string
	DeclaredToolCount int
	InteractionCount  int
	MCPToolCount      int
	BuiltinToolCount  int
}

// FailureDiagnostic stores safe request-shape hints for narrowing failed attempts
// without logging raw request bodies.
type FailureDiagnostic struct {
	Channel             string
	CompatName          string
	CompatKind          string
	CompatKindSource    string
	CompatMapping       string
	UpstreamRequestID   string
	PayloadBytes        int
	PayloadFields       string
	MessageRoles        string
	MessageRoleSequence string
	MessageContentKinds string
	ContentPartTypes    string
	InputItemTypes      string
	Temperature         string
	TopP                string
	ToolChoiceType      string
	ThinkingType        string
	ResponseFormatType  string
	ParallelToolCalls   string
	AddedFields         string
	RemovedFields       string
	ModifiedFields      string
	ToolDefinitionCount int
	ToolCallCount       int
	AssistantToolCalls  int
	ToolResultMessages  int
	ReasoningMessages   int
	MaxTokens           int
	MaxCompletionTokens int
	MaxOutputTokens     int
	ThinkingBudget      int
	MaxContentParts     int
	StopCount           int
}

// RequestFinal stores the final outcome for one client request.
type RequestFinal struct {
	RequestID    string
	FinalSuccess bool
	AttemptCount int
	CompletedAt  time.Time
}

type requestedModelAliasContextKey struct{}
type reasoningEffortContextKey struct{}
type serviceTierContextKey struct{}
type requestShapeContextKey struct{}
type toolShapeContextKey struct{}
type failureDiagnosticContextKey struct{}
type requestAttemptContextKey struct{}
type routingGroupContextKey struct{}

// WithRequestedModelAlias stores the client-requested model name for usage sinks.
func WithRequestedModelAlias(ctx context.Context, alias string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ctx
	}
	return context.WithValue(ctx, requestedModelAliasContextKey{}, alias)
}

// RequestedModelAliasFromContext returns the client-requested model name stored in ctx.
func RequestedModelAliasFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(requestedModelAliasContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithReasoningEffort stores the client-requested reasoning effort for usage sinks.
func WithReasoningEffort(ctx context.Context, effort string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ctx
	}
	return context.WithValue(ctx, reasoningEffortContextKey{}, effort)
}

// ReasoningEffortFromContext returns the client-requested reasoning effort stored in ctx.
func ReasoningEffortFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(reasoningEffortContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithServiceTier stores the client-requested service tier for usage sinks.
func WithServiceTier(ctx context.Context, tier string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	tier = strings.TrimSpace(tier)
	if tier == "" {
		tier = DefaultServiceTier
	}
	return context.WithValue(ctx, serviceTierContextKey{}, tier)
}

// ServiceTierFromContext returns the client-requested service tier stored in ctx.
func ServiceTierFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultServiceTier
	}
	raw := ctx.Value(serviceTierContextKey{})
	switch value := raw.(type) {
	case string:
		tier := strings.TrimSpace(value)
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	case []byte:
		tier := strings.TrimSpace(string(value))
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	default:
		return DefaultServiceTier
	}
}

// WithRequestShape stores safe inbound request shape counters for usage sinks.
func WithRequestShape(ctx context.Context, shape RequestShape) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if shape.MessageCount < 0 {
		shape.MessageCount = 0
	}
	if shape.ToolCount < 0 {
		shape.ToolCount = 0
	}
	if shape.MessageCount == 0 && shape.ToolCount == 0 {
		return ctx
	}
	return context.WithValue(ctx, requestShapeContextKey{}, shape)
}

// RequestShapeFromContext returns safe inbound request shape counters stored in ctx.
func RequestShapeFromContext(ctx context.Context) RequestShape {
	if ctx == nil {
		return RequestShape{}
	}
	raw := ctx.Value(requestShapeContextKey{})
	switch value := raw.(type) {
	case RequestShape:
		return normalizeRequestShape(value)
	case *RequestShape:
		if value == nil {
			return RequestShape{}
		}
		return normalizeRequestShape(*value)
	default:
		return RequestShape{}
	}
}

func normalizeRequestShape(shape RequestShape) RequestShape {
	if shape.MessageCount < 0 {
		shape.MessageCount = 0
	}
	if shape.ToolCount < 0 {
		shape.ToolCount = 0
	}
	return shape
}

// WithToolShape stores safe inbound tool-shape telemetry for usage sinks.
func WithToolShape(ctx context.Context, shape ToolShape) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	shape = normalizeToolShape(shape)
	if !shape.HasData() {
		return ctx
	}
	return context.WithValue(ctx, toolShapeContextKey{}, shape)
}

// ToolShapeFromContext returns safe inbound tool-shape telemetry stored in ctx.
func ToolShapeFromContext(ctx context.Context) ToolShape {
	if ctx == nil {
		return ToolShape{}
	}
	raw := ctx.Value(toolShapeContextKey{})
	switch value := raw.(type) {
	case ToolShape:
		return normalizeToolShape(value)
	case *ToolShape:
		if value == nil {
			return ToolShape{}
		}
		return normalizeToolShape(*value)
	default:
		return ToolShape{}
	}
}

// HasData reports whether the shape carries any telemetry.
func (s ToolShape) HasData() bool {
	return s.ToolTypes != "" ||
		s.ToolNameHashes != "" ||
		s.DeclaredToolCount > 0 ||
		s.InteractionCount > 0 ||
		s.MCPToolCount > 0 ||
		s.BuiltinToolCount > 0
}

func normalizeToolShape(shape ToolShape) ToolShape {
	shape.ToolTypes = strings.TrimSpace(shape.ToolTypes)
	shape.ToolNameHashes = strings.TrimSpace(shape.ToolNameHashes)
	if shape.DeclaredToolCount < 0 {
		shape.DeclaredToolCount = 0
	}
	if shape.InteractionCount < 0 {
		shape.InteractionCount = 0
	}
	if shape.MCPToolCount < 0 {
		shape.MCPToolCount = 0
	}
	if shape.BuiltinToolCount < 0 {
		shape.BuiltinToolCount = 0
	}
	return shape
}

// WithFailureDiagnostic stores safe failure-only request-shape hints for usage sinks.
func WithFailureDiagnostic(ctx context.Context, diag FailureDiagnostic) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	diag = normalizeFailureDiagnostic(diag)
	if !diag.HasData() {
		return ctx
	}
	return context.WithValue(ctx, failureDiagnosticContextKey{}, diag)
}

// FailureDiagnosticFromContext returns safe failure-only request-shape hints stored in ctx.
func FailureDiagnosticFromContext(ctx context.Context) FailureDiagnostic {
	if ctx == nil {
		return FailureDiagnostic{}
	}
	raw := ctx.Value(failureDiagnosticContextKey{})
	switch value := raw.(type) {
	case FailureDiagnostic:
		return normalizeFailureDiagnostic(value)
	case *FailureDiagnostic:
		if value == nil {
			return FailureDiagnostic{}
		}
		return normalizeFailureDiagnostic(*value)
	default:
		return FailureDiagnostic{}
	}
}

// HasData reports whether the diagnostic carries any telemetry.
func (d FailureDiagnostic) HasData() bool {
	return d.Channel != "" ||
		d.CompatName != "" ||
		d.CompatKind != "" ||
		d.CompatKindSource != "" ||
		d.CompatMapping != "" ||
		d.UpstreamRequestID != "" ||
		d.PayloadBytes > 0 ||
		d.PayloadFields != "" ||
		d.MessageRoles != "" ||
		d.MessageRoleSequence != "" ||
		d.MessageContentKinds != "" ||
		d.ContentPartTypes != "" ||
		d.InputItemTypes != "" ||
		d.Temperature != "" ||
		d.TopP != "" ||
		d.ToolChoiceType != "" ||
		d.ThinkingType != "" ||
		d.ResponseFormatType != "" ||
		d.ParallelToolCalls != "" ||
		d.AddedFields != "" ||
		d.RemovedFields != "" ||
		d.ModifiedFields != "" ||
		d.ToolDefinitionCount > 0 ||
		d.ToolCallCount > 0 ||
		d.AssistantToolCalls > 0 ||
		d.ToolResultMessages > 0 ||
		d.ReasoningMessages > 0 ||
		d.MaxTokens > 0 ||
		d.MaxCompletionTokens > 0 ||
		d.MaxOutputTokens > 0 ||
		d.ThinkingBudget > 0 ||
		d.MaxContentParts > 0 ||
		d.StopCount > 0
}

func normalizeFailureDiagnostic(diag FailureDiagnostic) FailureDiagnostic {
	diag.Channel = strings.TrimSpace(diag.Channel)
	diag.CompatName = strings.TrimSpace(diag.CompatName)
	diag.CompatKind = strings.TrimSpace(diag.CompatKind)
	diag.CompatKindSource = strings.TrimSpace(diag.CompatKindSource)
	diag.CompatMapping = strings.TrimSpace(diag.CompatMapping)
	diag.UpstreamRequestID = strings.TrimSpace(diag.UpstreamRequestID)
	diag.PayloadFields = strings.TrimSpace(diag.PayloadFields)
	diag.MessageRoles = strings.TrimSpace(diag.MessageRoles)
	diag.MessageRoleSequence = strings.TrimSpace(diag.MessageRoleSequence)
	diag.MessageContentKinds = strings.TrimSpace(diag.MessageContentKinds)
	diag.ContentPartTypes = strings.TrimSpace(diag.ContentPartTypes)
	diag.InputItemTypes = strings.TrimSpace(diag.InputItemTypes)
	diag.Temperature = strings.TrimSpace(diag.Temperature)
	diag.TopP = strings.TrimSpace(diag.TopP)
	diag.ToolChoiceType = strings.TrimSpace(diag.ToolChoiceType)
	diag.ThinkingType = strings.TrimSpace(diag.ThinkingType)
	diag.ResponseFormatType = strings.TrimSpace(diag.ResponseFormatType)
	diag.ParallelToolCalls = strings.TrimSpace(diag.ParallelToolCalls)
	diag.AddedFields = strings.TrimSpace(diag.AddedFields)
	diag.RemovedFields = strings.TrimSpace(diag.RemovedFields)
	diag.ModifiedFields = strings.TrimSpace(diag.ModifiedFields)
	if diag.AssistantToolCalls < 0 {
		diag.AssistantToolCalls = 0
	}
	if diag.ToolResultMessages < 0 {
		diag.ToolResultMessages = 0
	}
	if diag.ReasoningMessages < 0 {
		diag.ReasoningMessages = 0
	}
	if diag.PayloadBytes < 0 {
		diag.PayloadBytes = 0
	}
	if diag.ToolDefinitionCount < 0 {
		diag.ToolDefinitionCount = 0
	}
	if diag.ToolCallCount < 0 {
		diag.ToolCallCount = 0
	}
	if diag.MaxTokens < 0 {
		diag.MaxTokens = 0
	}
	if diag.MaxCompletionTokens < 0 {
		diag.MaxCompletionTokens = 0
	}
	if diag.MaxOutputTokens < 0 {
		diag.MaxOutputTokens = 0
	}
	if diag.ThinkingBudget < 0 {
		diag.ThinkingBudget = 0
	}
	if diag.MaxContentParts < 0 {
		diag.MaxContentParts = 0
	}
	if diag.StopCount < 0 {
		diag.StopCount = 0
	}
	return diag
}

// WithRoutingGroup stores the safe routing group selected for the upstream attempt.
func WithRoutingGroup(ctx context.Context, group string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	group = strings.TrimSpace(group)
	if group == "" {
		return ctx
	}
	return context.WithValue(ctx, routingGroupContextKey{}, group)
}

// RoutingGroupFromContext returns the selected upstream routing group stored in ctx.
func RoutingGroupFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(routingGroupContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithRequestAttempt stores request-scoped retry attempt metadata.
func WithRequestAttempt(ctx context.Context, attempt RequestAttempt) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	attempt.RequestID = strings.TrimSpace(attempt.RequestID)
	attempt.RetryReason = strings.TrimSpace(attempt.RetryReason)
	if attempt.RequestID == "" && attempt.AttemptNo <= 0 && attempt.RetryReason == "" {
		return ctx
	}
	return context.WithValue(ctx, requestAttemptContextKey{}, attempt)
}

// RequestAttemptFromContext returns request-scoped retry attempt metadata.
func RequestAttemptFromContext(ctx context.Context) RequestAttempt {
	if ctx == nil {
		return RequestAttempt{}
	}
	raw := ctx.Value(requestAttemptContextKey{})
	switch value := raw.(type) {
	case RequestAttempt:
		value.RequestID = strings.TrimSpace(value.RequestID)
		value.RetryReason = strings.TrimSpace(value.RetryReason)
		return value
	case *RequestAttempt:
		if value == nil {
			return RequestAttempt{}
		}
		out := *value
		out.RequestID = strings.TrimSpace(out.RequestID)
		out.RetryReason = strings.TrimSpace(out.RetryReason)
		return out
	default:
		return RequestAttempt{}
	}
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

// RequestFinalizer is implemented by plugins that can update request outcomes.
type RequestFinalizer interface {
	HandleRequestFinal(ctx context.Context, final RequestFinal)
}

type queueItem struct {
	ctx    context.Context
	record Record
}

// Manager maintains a queue of usage records and delivers them to registered plugins.
type Manager struct {
	once     sync.Once
	stopOnce sync.Once
	cancel   context.CancelFunc

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []queueItem
	closed bool
	maxLen int
	drops  atomic.Uint64

	pluginsMu sync.RWMutex
	plugins   []Plugin
	named     map[string]int
}

// NewManager constructs a manager with a buffered queue.
func NewManager(buffer int) *Manager {
	if buffer <= 0 {
		buffer = defaultQueueSize
	}
	m := &Manager{maxLen: buffer}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Start launches the background dispatcher. Calling Start multiple times is safe.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		var workerCtx context.Context
		workerCtx, m.cancel = context.WithCancel(ctx)
		go m.run(workerCtx)
	})
}

// Stop stops the dispatcher and drains the queue.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cond.Broadcast()
	})
}

// Register appends a plugin to the delivery list.
func (m *Manager) Register(plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	m.pluginsMu.Lock()
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// RegisterNamed registers or replaces a plugin by name.
func (m *Manager) RegisterNamed(name string, plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}

	m.pluginsMu.Lock()
	if m.named == nil {
		m.named = make(map[string]int)
	}
	if index, exists := m.named[name]; exists && index >= 0 && index < len(m.plugins) {
		m.plugins[index] = plugin
		m.pluginsMu.Unlock()
		return
	}
	m.named[name] = len(m.plugins)
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// Publish enqueues a usage record for processing. If no plugin is registered
// the record will be discarded downstream.
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	// ensure worker is running even if Start was not called explicitly
	m.Start(context.Background())
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if m.maxLen > 0 && len(m.queue) >= m.maxLen {
		copy(m.queue, m.queue[1:])
		m.queue[len(m.queue)-1] = queueItem{ctx: ctx, record: record}
		m.drops.Add(1)
		m.mu.Unlock()
		m.cond.Signal()
		return
	}
	m.queue = append(m.queue, queueItem{ctx: ctx, record: record})
	m.mu.Unlock()
	m.cond.Signal()
}

// Dropped returns the number of usage records dropped because the queue was full.
func (m *Manager) Dropped() uint64 {
	if m == nil {
		return 0
	}
	return m.drops.Load()
}

func (m *Manager) run(ctx context.Context) {
	for {
		m.mu.Lock()
		for !m.closed && len(m.queue) == 0 {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.closed {
			m.mu.Unlock()
			return
		}
		item := m.queue[0]
		m.queue[0] = queueItem{}
		m.queue = m.queue[1:]
		m.mu.Unlock()
		m.dispatch(item)
	}
}

func (m *Manager) dispatch(item queueItem) {
	m.pluginsMu.RLock()
	plugins := make([]Plugin, len(m.plugins))
	copy(plugins, m.plugins)
	m.pluginsMu.RUnlock()
	if len(plugins) == 0 {
		return
	}
	for _, plugin := range plugins {
		if plugin == nil {
			continue
		}
		safeInvoke(plugin, item.ctx, item.record)
	}
}

// PublishRequestFinal notifies interested plugins about a completed request.
func (m *Manager) PublishRequestFinal(ctx context.Context, final RequestFinal) {
	if m == nil {
		return
	}
	final.RequestID = strings.TrimSpace(final.RequestID)
	if final.RequestID == "" {
		return
	}
	if final.CompletedAt.IsZero() {
		final.CompletedAt = time.Now()
	}

	m.pluginsMu.RLock()
	plugins := make([]Plugin, len(m.plugins))
	copy(plugins, m.plugins)
	m.pluginsMu.RUnlock()
	for _, plugin := range plugins {
		finalizer, ok := plugin.(RequestFinalizer)
		if !ok || finalizer == nil {
			continue
		}
		safeInvokeFinalizer(finalizer, ctx, final)
	}
}

func safeInvoke(plugin Plugin, ctx context.Context, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleUsage(ctx, record)
}

func safeInvokeFinalizer(plugin RequestFinalizer, ctx context.Context, final RequestFinal) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: finalizer plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleRequestFinal(ctx, final)
}

var defaultManager = NewManager(512)

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a plugin on the default manager.
func RegisterPlugin(plugin Plugin) { DefaultManager().Register(plugin) }

// RegisterNamedPlugin registers or replaces a named plugin on the default manager.
func RegisterNamedPlugin(name string, plugin Plugin) { DefaultManager().RegisterNamed(name, plugin) }

// PublishRecord publishes a record using the default manager.
func PublishRecord(ctx context.Context, record Record) { DefaultManager().Publish(ctx, record) }

// PublishRequestFinal notifies plugins that one client request has completed.
func PublishRequestFinal(ctx context.Context, final RequestFinal) {
	DefaultManager().PublishRequestFinal(ctx, final)
}

// StartDefault starts the default manager's dispatcher.
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// StopDefault stops the default manager's dispatcher.
func StopDefault() { DefaultManager().Stop() }
