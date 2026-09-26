package middleware

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/routemetrics"
)

func TestTokenMetricsIgnoreEnvelopesAndHandleSplitSSE(t *testing.T) {
	for _, content := range []string{`{"choices":[{"delta":{"content":"ok"}}]}`, `{"type":"response.output_text.delta","delta":"ok"}`, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`} {
		collector := routemetrics.NewCollector()
		ctx := routemetrics.Start(context.Background(), "/v1/responses", collector)
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		w := &tokenMetricWriter{ResponseWriter: ginCtx.Writer, ctx: ctx}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n:ping\n\n"))
		if w.done {
			t.Fatal("envelope counted as token")
		}
		_, _ = w.Write([]byte("data: " + strings.Repeat("x", 70<<10) + "\n"))
		if cap(w.line) > 64<<10 {
			t.Fatal("unbounded SSE buffer")
		}
		sse := "data: " + content + "\n\n"
		for _, b := range []byte(sse) {
			_, _ = w.Write([]byte{b})
		}
		routemetrics.Finish(ctx, 200)
		if collector.Snapshot().Series[0].Timings["first_token"].Count != 1 {
			t.Fatal("semantic delta missing")
		}
	}
}
