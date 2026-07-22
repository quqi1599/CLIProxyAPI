package translator

import (
	"context"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Registry manages translation functions across schemas.
type Registry struct {
	mu        sync.RWMutex
	requests  map[Format]map[Format]RequestTransform
	responses map[Format]map[Format]ResponseTransform
	hooks     PluginHooks
}

type registryContextKey struct{}

type builtinRegistration struct {
	from     Format
	to       Format
	request  RequestTransform
	response ResponseTransform
}

var (
	builtinRegistrationsMu sync.RWMutex
	builtinRegistrations   []builtinRegistration
)

// NewRegistry constructs an empty translator registry.
func NewRegistry() *Registry {
	return &Registry{
		requests:  make(map[Format]map[Format]RequestTransform),
		responses: make(map[Format]map[Format]ResponseTransform),
	}
}

// NewBuiltinRegistry constructs an isolated registry populated with the built-in
// transforms linked into the current binary. Plugin hooks are not copied.
func NewBuiltinRegistry() *Registry {
	registry := NewRegistry()
	builtinRegistrationsMu.RLock()
	registrations := append([]builtinRegistration(nil), builtinRegistrations...)
	builtinRegistrationsMu.RUnlock()
	for _, registration := range registrations {
		registry.Register(registration.from, registration.to, registration.request, registration.response)
	}
	return registry
}

// ContextWithRegistry binds a registry to one execution context. A nil registry
// keeps the existing context unchanged.
func ContextWithRegistry(ctx context.Context, registry *Registry) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if registry == nil {
		return ctx
	}
	return context.WithValue(ctx, registryContextKey{}, registry)
}

// RegistryFromContext returns the request-scoped registry or the default facade.
func RegistryFromContext(ctx context.Context) *Registry {
	if ctx != nil {
		if registry, ok := ctx.Value(registryContextKey{}).(*Registry); ok && registry != nil {
			return registry
		}
	}
	return Default()
}

// Register stores request/response transforms between two formats.
func (r *Registry) Register(from, to Format, request RequestTransform, response ResponseTransform) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.requests[from]; !ok {
		r.requests[from] = make(map[Format]RequestTransform)
	}
	if request != nil {
		r.requests[from][to] = request
	}

	if _, ok := r.responses[from]; !ok {
		r.responses[from] = make(map[Format]ResponseTransform)
	}
	r.responses[from][to] = response
}

// SetPluginHooks stores translator plugin hooks for this registry.
func (r *Registry) SetPluginHooks(hooks PluginHooks) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.hooks = hooks
}

// TranslateRequest converts a payload between schemas, returning the original payload
// if no translator is registered. When falling back to the original payload, the
// "model" field is still updated to match the resolved model name so that
// client-side prefixes (e.g. "copilot/gpt-5-mini") are not leaked upstream.
func (r *Registry) TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte {
	r.mu.RLock()
	var fn RequestTransform
	if byTarget, ok := r.requests[from]; ok {
		fn = byTarget[to]
	}
	hooks := r.hooks
	r.mu.RUnlock()

	body := rawJSON
	if fn != nil {
		body = fn(model, body, stream)
	} else {
		if model != "" && gjson.GetBytes(body, "model").String() != model {
			if updated, err := sjson.SetBytes(body, "model", model); err != nil {
				log.Warnf("translator: failed to normalize model in request fallback: %v", err)
			} else {
				body = updated
			}
		}
	}

	if hooks != nil {
		body = hooks.NormalizeRequest(context.Background(), from, to, model, body, stream)
		if fn == nil {
			if translated, ok := hooks.TranslateRequest(context.Background(), from, to, model, body, stream); ok {
				body = translated
			}
		}
	}
	return body
}

// HasRequestTransformer indicates whether a request translator exists.
func (r *Registry) HasRequestTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.requests[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn != nil {
			return true
		}
	}
	return false
}

// HasResponseTransformer indicates whether a response translator exists.
func (r *Registry) HasResponseTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[from]; ok {
		if fn, isOk := byTarget[to]; isOk && hasAnyResponseTransform(fn) {
			return true
		}
	}
	return false
}

// HasStreamResponseTransformer indicates whether a streaming response translator exists.
func (r *Registry) HasStreamResponseTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn.Stream != nil {
			return true
		}
	}
	return false
}

// HasNonStreamResponseTransformer indicates whether a non-streaming response translator exists.
func (r *Registry) HasNonStreamResponseTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn.NonStream != nil {
			return true
		}
	}
	return false
}

// TranslateStream applies the registered streaming response translator.
func (r *Registry) TranslateStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	r.mu.RLock()
	var stream ResponseStreamTransform
	if byTarget, ok := r.responses[to]; ok {
		stream = byTarget[from].Stream
	}
	hooks := r.hooks
	r.mu.RUnlock()

	body := rawJSON
	if hooks != nil {
		body = hooks.NormalizeResponseBefore(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, true)
	}

	var outputs [][]byte
	usedNativeTransform := false
	if stream != nil {
		usedNativeTransform = true
		outputs = stream(ctx, model, originalRequestRawJSON, requestRawJSON, body, param)
	} else if hooks != nil {
		if translated, ok := hooks.TranslateResponse(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, true); ok {
			outputs = [][]byte{translated}
		}
	}
	if outputs == nil && !usedNativeTransform {
		outputs = [][]byte{body}
	}
	if hooks != nil {
		for i, output := range outputs {
			outputs[i] = hooks.NormalizeResponseAfter(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, output, true)
		}
	}
	return outputs
}

// TranslateNonStream applies the registered non-stream response translator.
func (r *Registry) TranslateNonStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	r.mu.RLock()
	var fn ResponseTransform
	if byTarget, ok := r.responses[to]; ok {
		fn = byTarget[from]
	}
	hooks := r.hooks
	r.mu.RUnlock()

	body := rawJSON
	if hooks != nil {
		body = hooks.NormalizeResponseBefore(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, false)
	}
	if fn.NonStream != nil {
		body = fn.NonStream(ctx, model, originalRequestRawJSON, requestRawJSON, body, param)
	} else if hooks != nil {
		if translated, ok := hooks.TranslateResponse(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, false); ok {
			body = translated
		}
	}
	if hooks != nil {
		body = hooks.NormalizeResponseAfter(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, false)
	}
	return body
}

// TranslateTokenCount applies the registered token count response translator.
func (r *Registry) TranslateTokenCount(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[to]; ok {
		if fn, isOk := byTarget[from]; isOk && fn.TokenCount != nil {
			return fn.TokenCount(ctx, count)
		}
	}
	return rawJSON
}

var defaultRegistry = NewRegistry()

// Default exposes the package-level registry for shared use.
func Default() *Registry {
	return defaultRegistry
}

// Register attaches transforms to the default registry.
func Register(from, to Format, request RequestTransform, response ResponseTransform) {
	defaultRegistry.Register(from, to, request, response)
}

// RegisterBuiltin attaches a built-in transform to the default facade and the
// catalog used by NewBuiltinRegistry. Dynamic callers should use Register.
func RegisterBuiltin(from, to Format, request RequestTransform, response ResponseTransform) {
	defaultRegistry.Register(from, to, request, response)
	builtinRegistrationsMu.Lock()
	builtinRegistrations = append(builtinRegistrations, builtinRegistration{
		from: from, to: to, request: request, response: response,
	})
	builtinRegistrationsMu.Unlock()
}

// SetPluginHooks stores plugin hooks on the default registry.
func SetPluginHooks(hooks PluginHooks) {
	defaultRegistry.SetPluginHooks(hooks)
}

// TranslateRequest is a helper on the default registry.
func TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte {
	return defaultRegistry.TranslateRequest(from, to, model, rawJSON, stream)
}

// HasRequestTransformer inspects the default registry.
func HasRequestTransformer(from, to Format) bool {
	return defaultRegistry.HasRequestTransformer(from, to)
}

// HasResponseTransformer inspects the default registry.
func HasResponseTransformer(from, to Format) bool {
	return defaultRegistry.HasResponseTransformer(from, to)
}

// HasStreamResponseTransformer inspects the default registry for a streaming response translator.
func HasStreamResponseTransformer(from, to Format) bool {
	return defaultRegistry.HasStreamResponseTransformer(from, to)
}

// HasNonStreamResponseTransformer inspects the default registry for a non-streaming response translator.
func HasNonStreamResponseTransformer(from, to Format) bool {
	return defaultRegistry.HasNonStreamResponseTransformer(from, to)
}

// TranslateStream uses the request-scoped registry or the default facade.
func TranslateStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	return RegistryFromContext(ctx).TranslateStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}

// TranslateNonStream uses the request-scoped registry or the default facade.
func TranslateNonStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	return RegistryFromContext(ctx).TranslateNonStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}

// TranslateTokenCount uses the request-scoped registry or the default facade.
func TranslateTokenCount(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte {
	return RegistryFromContext(ctx).TranslateTokenCount(ctx, from, to, count, rawJSON)
}

func hasAnyResponseTransform(fn ResponseTransform) bool {
	return fn.Stream != nil || fn.NonStream != nil || fn.TokenCount != nil
}
