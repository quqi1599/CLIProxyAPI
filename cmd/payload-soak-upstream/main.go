// Command payload-soak-upstream serves deterministic OpenAI-compatible fault fixtures.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultListenAddress = "127.0.0.1:18317"
	defaultAPIKey        = "payload-soak-upstream"
	maxRequestBodyBytes  = 32 << 20
	upstreamMarker       = "payload-soak-upstream"
)

type upstreamConfig struct {
	apiKey         string
	slowDelay      time.Duration
	streamInterval time.Duration
}

func main() {
	listen := flag.String("listen", defaultListenAddress, "HTTP listen address")
	apiKey := flag.String("api-key", defaultAPIKey, "fake upstream bearer token")
	slowDelay := flag.Duration("slow-delay", 300*time.Millisecond, "delay before the slow scenario sends headers")
	streamInterval := flag.Duration("stream-interval", 100*time.Millisecond, "delay between deterministic stream events")
	flag.Parse()
	if strings.TrimSpace(*listen) == "" || strings.TrimSpace(*apiKey) == "" || *slowDelay <= 0 || *streamInterval <= 0 {
		fmt.Fprintln(os.Stderr, "listen, api-key, slow-delay, and stream-interval must be set to positive values")
		os.Exit(2)
	}
	handler := newFakeUpstreamHandler(upstreamConfig{
		apiKey:         strings.TrimSpace(*apiKey),
		slowDelay:      *slowDelay,
		streamInterval: *streamInterval,
	})
	fmt.Printf("payload soak upstream listening on http://%s\n", *listen)
	err := http.ListenAndServe(*listen, handler)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "payload soak upstream: %v\n", err)
		os.Exit(1)
	}
}

func newFakeUpstreamHandler(cfg upstreamConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok","service":"payload-soak-upstream"}`)
			return
		}
		if r.Method != http.MethodPost || !supportedUpstreamPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+cfg.apiKey {
			writeUpstreamError(w, http.StatusUnauthorized, "soak_authentication_failed", "invalid fake upstream credential")
			return
		}
		body, errRead := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
		_ = r.Body.Close()
		if errRead != nil {
			writeUpstreamError(w, http.StatusBadRequest, "soak_request_read_failed", "request body could not be read")
			return
		}
		if len(body) > maxRequestBodyBytes {
			writeUpstreamError(w, http.StatusRequestEntityTooLarge, "soak_request_too_large", "request body exceeds 32 MiB")
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if errDecode := json.Unmarshal(body, &request); errDecode != nil {
			writeUpstreamError(w, http.StatusBadRequest, "soak_invalid_json", "request body must be JSON")
			return
		}
		scenario := detectScenario(body)
		switch scenario {
		case "slow_upstream":
			select {
			case <-r.Context().Done():
				return
			case <-time.After(cfg.slowDelay):
			}
			writeChatResponse(w, scenario)
		case "http_half_close":
			writeHalfClosedResponse(w)
		case "http_reset":
			writeResetResponse(w)
		case "rate_limit_429":
			writeUpstreamError(w, http.StatusTooManyRequests, "soak_rate_limit", "deterministic rate limit")
		case "upstream_5xx":
			writeUpstreamError(w, http.StatusServiceUnavailable, "soak_upstream_5xx", "deterministic upstream failure")
		case "bad_gzip":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-a-gzip-stream"))
		case "malformed_sse":
			writeMalformedSSE(w)
		case "downstream_stream":
			writeChatStream(w, r, cfg.streamInterval, scenario, false)
		case "client_mid_stream_cancel":
			writeChatStream(w, r, cfg.streamInterval, scenario, true)
		default:
			if request.Stream {
				writeChatStream(w, r, cfg.streamInterval, scenario, false)
			} else {
				writeChatResponse(w, scenario)
			}
		}
	})
}

func supportedUpstreamPath(path string) bool {
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/responses")
}

func detectScenario(body []byte) string {
	for _, name := range []string{
		"slow_upstream",
		"http_half_close",
		"http_reset",
		"rate_limit_429",
		"upstream_5xx",
		"bad_gzip",
		"malformed_sse",
		"downstream_stream",
		"client_mid_stream_cancel",
	} {
		if bytes.Contains(body, []byte("soak-scenario:"+name)) {
			return name
		}
	}
	return "normal"
}

func writeChatResponse(w http.ResponseWriter, scenario string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-payload-soak",
		"object":  "chat.completion",
		"created": 0,
		"model":   "payload-soak-model",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]string{
				"role":    "assistant",
				"content": upstreamMarker + " scenario=" + scenario,
			},
		}},
		"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func writeChatStream(w http.ResponseWriter, r *http.Request, interval time.Duration, scenario string, waitForCancel bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-payload-soak",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "payload-soak-model",
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{"role": "assistant", "content": upstreamMarker + " scenario=" + scenario}, "finish_reason": nil}},
	})
	if flusher != nil {
		flusher.Flush()
	}
	if waitForCancel {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * interval):
		}
		return
	}
	select {
	case <-r.Context().Done():
		return
	case <-time.After(interval):
	}
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-payload-soak",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "payload-soak-model",
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSEData(w io.Writer, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeMalformedSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"error":{"message":"malformed SSE sentinel"`+"\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeUpstreamError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": "soak_error", "code": code, "message": message},
	})
}

func writeHalfClosedResponse(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeUpstreamError(w, http.StatusInternalServerError, "soak_hijack_unavailable", "HTTP hijacking is unavailable")
		return
	}
	conn, buffer, errHijack := hijacker.Hijack()
	if errHijack != nil {
		return
	}
	_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1024\r\nConnection: close\r\n\r\n{\"id\":\"partial")
	_ = buffer.Flush()
	if closeWriter, okClose := conn.(interface{ CloseWrite() error }); okClose {
		_ = closeWriter.CloseWrite()
	}
	_ = conn.Close()
}

func writeResetResponse(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeUpstreamError(w, http.StatusInternalServerError, "soak_hijack_unavailable", "HTTP hijacking is unavailable")
		return
	}
	conn, _, errHijack := hijacker.Hijack()
	if errHijack != nil {
		return
	}
	if linger, okLinger := conn.(interface{ SetLinger(int) error }); okLinger {
		_ = linger.SetLinger(0)
	}
	_ = conn.Close()
}
