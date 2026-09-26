package middleware

import (
	"bytes"
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/routemetrics"
	"github.com/tidwall/gjson"
)

func RouteMetrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.URL.Path {
		case "/v1/chat/completions", "/v1/responses", "/v1/messages":
		default:
			c.Next()
			return
		}
		ctx := routemetrics.Start(c.Request.Context(), c.Request.URL.Path, nil)
		c.Request = c.Request.WithContext(ctx)
		c.Writer = &tokenMetricWriter{ResponseWriter: c.Writer, ctx: ctx}
		defer func() {
			status := c.Writer.Status()
			if c.Request.Context().Err() != nil {
				status = 499
			}
			routemetrics.Finish(ctx, status)
		}()
		c.Next()
	}
}

// Buffer at most one 64 KiB SSE line until the first semantic delta. Oversized
// lines are skipped, rather than retaining customer output in telemetry memory.
type tokenMetricWriter struct {
	gin.ResponseWriter
	ctx  context.Context
	line []byte
	skip bool
	done bool
}

func (w *tokenMetricWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && !w.done && w.Status() < 400 && strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.observe(p[:n])
	}
	return n, err
}
func (w *tokenMetricWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *tokenMetricWriter) observe(p []byte) {
	for len(p) > 0 && !w.done {
		i := bytes.IndexByte(p, '\n')
		end := len(p)
		if i >= 0 {
			end = i
		}
		if !w.skip {
			if len(w.line)+end > 64<<10 {
				w.line = nil
				w.skip = true
			} else {
				w.line = append(w.line, p[:end]...)
			}
		}
		if i < 0 {
			return
		}
		if !w.skip && semanticDelta(w.line) {
			w.done = true
			routemetrics.FirstToken(w.ctx)
		}
		w.line = w.line[:0]
		w.skip = false
		p = p[i+1:]
	}
	if w.done {
		w.line = nil
	}
}
func semanticDelta(line []byte) bool {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	line = bytes.TrimSpace(line[5:])
	if !gjson.ValidBytes(line) {
		return false
	}
	root := gjson.ParseBytes(line)
	for _, path := range []string{"choices.0.delta.content", "choices.0.delta.reasoning_content", "choices.0.delta.reasoning", "choices.0.delta.tool_calls.0.function.arguments", "choices.0.delta.tool_calls.0.function.name", "delta.text", "delta.thinking", "delta.partial_json"} {
		if root.Get(path).String() != "" {
			return true
		}
	}
	switch root.Get("type").String() {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
		return root.Get("delta").String() != ""
	}
	return false
}
