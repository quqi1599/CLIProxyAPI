package helps

import (
	"net/http"
	"strings"
	"testing"
)

func TestKimiCodeClientIdentityBoundaries(t *testing.T) {
	for _, endpoint := range []string{"https://api.kimi.com/coding/v1/responses", "https://api.kimi.ai/coding/v1/messages", "https://api.kimi.com.evil.example/coding/v1", "https://api.moonshot.cn/v1/chat/completions", "https://api.kimi.com/not-coding", "http://api.kimi.com/coding/v1"} {
		req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
		req.Header.Set("User-Agent", "existing")
		ApplyKimiCodeClientIdentity(req, "client/2")
		want := "existing"
		if endpoint == "https://api.kimi.com/coding/v1/responses" || endpoint == "https://api.kimi.ai/coding/v1/messages" {
			want = "client/2"
		}
		if got := req.Header.Get("User-Agent"); got != want {
			t.Fatalf("%s UA=%s want=%s", endpoint, got, want)
		}
	}
	for _, ua := range []string{"", "client\r\nInjected: value", strings.Repeat("a", 513)} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/responses", nil)
		ApplyKimiCodeClientIdentity(req, ua)
		if req.Header.Get("User-Agent") != "CLIProxyAPI" {
			t.Fatal("invalid or missing UA must use proxy identity")
		}
	}
}
