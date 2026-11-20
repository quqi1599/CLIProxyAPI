package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiVideoChatExecutorWire(t *testing.T) {
	const video = "data:video/mp4;base64,AAAAIGZ0eXBtcDQy"
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"` + video + `"}},{"type":"text","text":"inspect the clip"}]}]}`)
	for _, model := range []string{"k3", "k3.1-pro"} {
		for _, kind := range []string{"compat", "dedicated", "claude"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", model, kind, stream), func(t *testing.T) {
					e, auth := kimiKVVExecutor(kind, &config.Config{})
					calls := 0
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						wire, err := io.ReadAll(r.Body)
						if err != nil || r.URL.Path != "/coding/v1/chat/completions" || gjson.GetBytes(wire, "messages.0.content.0.video_url.url").Str != video || gjson.GetBytes(wire, "messages.0.content.1.text").Str != "inspect the clip" || r.Header.Get("Authorization") != "Bearer synthetic" {
							t.Fatal("native video request, message order or credential lost")
						}
						return kimiKVVResponse(stream), nil
					}))
					req := cliproxyexecutor.Request{Model: model, Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
					var out strings.Builder
					if stream {
						result, err := e.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							out.Write(chunk.Payload)
						}
					} else {
						resp, err := e.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						out.Write(resp.Payload)
					}
					if calls != 1 || !strings.Contains(out.String(), "OK") {
						t.Fatal("no completion payload")
					}
				})
			}
		}
	}
}

func TestKimiVideoRequestStopsBeforeInference(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"http://media.example/clip.mp4"}}]}]}`)
	for _, kind := range []string{"compat", "dedicated", "claude"} {
		t.Run(kind, func(t *testing.T) {
			e, auth := kimiKVVExecutor(kind, &config.Config{})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", miniMaxReceiptRoundTripper(func(r *http.Request) (*http.Response, error) {
				t.Fatal("inference called with a rejected video")
				return nil, nil
			}))
			req := cliproxyexecutor.Request{Model: "k3", Payload: body}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
			_, err := e.Execute(ctx, auth, req, opts)
			if err == nil || !strings.Contains(err.Error(), "public HTTPS") {
				t.Fatalf("unexpected error %v", err)
			}
			opts.Stream = true
			_, err = e.ExecuteStream(ctx, auth, req, opts)
			if err == nil || !strings.Contains(err.Error(), "public HTTPS") {
				t.Fatalf("unexpected stream error %v", err)
			}
		})
	}
}
