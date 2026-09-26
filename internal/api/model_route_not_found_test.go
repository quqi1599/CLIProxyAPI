package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnconfiguredHaikuReturns404AcrossProtocols(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []string{"false", "true"} {
			body := `{"model":"claude-haiku-4-5-20251001","stream":` + stream + `,"max_tokens":16,"messages":[{"role":"user","content":"OK"}],"input":"OK"}`
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer test-key")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no provider route configured") {
				t.Fatalf("%s stream=%s status=%d body=%s", path, stream, w.Code, w.Body.String())
			}
		}
	}
}
