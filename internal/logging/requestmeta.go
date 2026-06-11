package logging

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

type endpointKey struct{}
type endpointMethodKey struct{}
type endpointPathKey struct{}
type responseStatusKey struct{}
type responseHeadersKey struct{}

type responseStatusHolder struct {
	status atomic.Int32
}

type responseHeadersHolder struct {
	mu      sync.RWMutex
	headers http.Header
}

func WithEndpoint(ctx context.Context, endpoint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint = strings.TrimSpace(endpoint)
	method, path := SplitEndpoint(endpoint)
	return WithEndpointParts(context.WithValue(ctx, endpointKey{}, endpoint), method, path)
}

func WithEndpointParts(ctx context.Context, method string, path string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if method != "" {
		ctx = context.WithValue(ctx, endpointMethodKey{}, method)
	}
	if path != "" {
		ctx = context.WithValue(ctx, endpointPathKey{}, path)
	}
	if endpoint := JoinEndpoint(method, path); endpoint != "" {
		ctx = context.WithValue(ctx, endpointKey{}, endpoint)
	}
	return ctx
}

func GetEndpoint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if endpoint, ok := ctx.Value(endpointKey{}).(string); ok {
		return endpoint
	}
	return ""
}

func GetEndpointMethod(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if method, ok := ctx.Value(endpointMethodKey{}).(string); ok {
		return method
	}
	method, _ := SplitEndpoint(GetEndpoint(ctx))
	return method
}

func GetEndpointPath(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if path, ok := ctx.Value(endpointPathKey{}).(string); ok {
		return path
	}
	_, path := SplitEndpoint(GetEndpoint(ctx))
	return path
}

func SplitEndpoint(endpoint string) (string, string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", ""
	}
	fields := strings.Fields(endpoint)
	if len(fields) >= 2 && isHTTPMethod(fields[0]) {
		return strings.ToUpper(fields[0]), fields[1]
	}
	return "", endpoint
}

func JoinEndpoint(method string, path string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	switch {
	case method != "" && path != "":
		return method + " " + path
	case path != "":
		return path
	default:
		return method
	}
}

func isHTTPMethod(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func WithResponseStatusHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseStatusKey{}, &responseStatusHolder{})
}

func WithResponseHeadersHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseHeadersKey{}, &responseHeadersHolder{})
}

func SetResponseStatus(ctx context.Context, status int) {
	if ctx == nil || status <= 0 {
		return
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return
	}
	holder.status.Store(int32(status))
}

func SetResponseHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.headers = cloneHTTPHeader(headers)
}

func GetResponseStatus(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return 0
	}
	return int(holder.status.Load())
}

func GetResponseHeaders(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return nil
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return cloneHTTPHeader(holder.headers)
}

func cloneHTTPHeader(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
