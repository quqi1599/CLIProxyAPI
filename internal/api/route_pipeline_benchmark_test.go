package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/routemetrics"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// BenchmarkRoutePipelineMatrix exercises real ingress, audit encryption/SQLite,
// selection, provider preparation and response translation against loopback HTTP.
// Run with -benchtime=2x -cpu=1,2; no external credentials or upstreams are used.
func BenchmarkRoutePipelineMatrix(b *testing.B) {
	old := log.GetLevel()
	log.SetLevel(log.ErrorLevel)
	b.Cleanup(func() { log.SetLevel(old) })
	gin.SetMode(gin.TestMode)
	for _, size := range []int{8 << 10, 1 << 20, 16 << 20, 64 << 20} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, provider := range []string{"codex", "claude", "deepseek", "minimax"} {
				b.Run(fmt.Sprintf("%s/%s/%dKiB", provider, strings.TrimPrefix(path, "/v1/"), size>>10), func(b *testing.B) { benchmarkRoutePipeline(b, size, path, provider) })
			}
		}
	}
}

func benchmarkRoutePipeline(b *testing.B, size int, path, provider string) {
	model := map[string]string{"codex": "gpt-5.5", "claude": "claude-sonnet-4-6", "deepseek": "deepseek-v4.1-flash", "minimax": "MiniMax-M3"}[provider]
	var received atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		received.Add(1)
		time.Sleep(2 * time.Millisecond)
		if provider == "codex" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_matrix","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		} else if provider == "claude" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_matrix\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else if strings.HasSuffix(r.URL.Path, "/responses") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_matrix","object":"response","status":"completed","model":"deepseek-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"chatcmpl_matrix","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, model)
		}
	}))
	b.Cleanup(upstream.Close)
	dir := b.TempDir()
	policy := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policy, []byte("version: matrix\nrules:\n  - id: matrix\n    category: synthetic\n    severity: low\n    action: observe\n    keywords: [\"benchmark fixture\"]\n"), 0600); err != nil {
		b.Fatal(err)
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"matrix-local-key"}, RequestGuards: config.RequestGuardsConfig{GlobalAdmission: config.GlobalAdmissionConfig{Enabled: true, BodyCapacityBytes: 512 << 20, Capacity: 128, TransformCapacity: 16}}}, AuthDir: dir, DisableClaudeCloakMode: true, CommercialMode: true, RemoteManagement: config.RemoteManagement{DisableControlPanel: true}, ContentAudit: config.ContentAuditConfig{Enabled: true, AuditOnly: true, PolicyFile: policy, DatabasePath: filepath.Join(dir, "audit.db"), EvidenceKey: strings.Repeat("matrix", 8), MaxBodyBytes: 256 << 20}}
	manager := auth.NewManager(nil, &auth.SpreadSelector{}, nil)
	var exec auth.ProviderExecutor
	switch provider {
	case "codex":
		exec = runtimeexecutor.NewCodexExecutor(cfg)
	case "claude":
		exec = runtimeexecutor.NewClaudeExecutor(cfg)
	default:
		cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: provider, Kind: provider, BaseURL: upstream.URL, Models: []config.OpenAICompatibilityModel{{Name: model, Alias: model}}}}
		exec = runtimeexecutor.NewOpenAICompatExecutor(provider, cfg)
	}
	manager.RegisterExecutor(exec)
	id := "matrix-" + provider
	a := &auth.Auth{ID: id, Provider: exec.Identifier(), Attributes: map[string]string{"api_key": "matrix-upstream-key", "base_url": upstream.URL, "compat_name": provider, "compat_kind": provider}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		b.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, exec.Identifier(), []*registry.ModelInfo{{ID: model}})
	b.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	s := NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(dir, "config.yaml"))
	b.Cleanup(func() { _ = s.Stop(context.Background()) })
	text := "benchmark fixture " + strings.Repeat("abcdefghijklmno ", size/16) + " benchmark fixture"
	payload := map[string]any{"model": model, "stream": false, "max_tokens": 32}
	if path == "/v1/responses" {
		payload["input"] = []any{map[string]any{"role": "user", "content": text}}
		delete(payload, "max_tokens")
		payload["max_output_tokens"] = 32
	} else {
		payload["messages"] = []any{map[string]any{"role": "user", "content": text}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		b.Fatal(err)
	}
	before := routemetrics.Default.Snapshot()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer matrix-local-key")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			if w.Code != 200 {
				b.Errorf("status=%d body=%.400s", w.Code, w.Body.String())
			}
		}
	})
	b.StopTimer()
	after := routemetrics.Default.Snapshot()
	timings := map[string]uint64{}
	statuses := map[int]int64{}
	for _, part := range []struct {
		s    routemetrics.Snapshot
		sign int64
	}{{before, -1}, {after, 1}} {
		for _, row := range part.s.Series {
			if row.Route != path || row.Model != model {
				continue
			}
			for status, n := range row.Requests {
				statuses[status] += part.sign * int64(n)
			}
			for stage, d := range row.Timings {
				timings[stage] = uint64(int64(timings[stage]) + part.sign*int64(d.Nanoseconds))
			}
		}
	}
	for stage, n := range timings {
		b.ReportMetric(float64(n)/float64(b.N), stage+"-ns/op")
	}
	b.ReportMetric(float64(statuses[499]), "499-count")
	b.ReportMetric(float64(statuses[503]), "503-count")
	if received.Load() != int64(b.N) || timings["audit_write"] == 0 || timings["transform"] == 0 || timings["selection"] == 0 {
		b.Fatalf("incomplete coverage: received=%d n=%d timings=%v", received.Load(), b.N, timings)
	}
}
