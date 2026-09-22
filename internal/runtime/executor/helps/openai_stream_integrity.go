package helps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/tidwall/gjson"
)

// OpenAIStreamIntegrity checks the verified Aliyun DeepSeek usage contract.
// It retains metadata only, never response content or tool arguments.
type OpenAIStreamIntegrity struct {
	finishSeen bool
	usageSeen  bool
	doneSeen   bool
}

// NewAliyunDeepSeekStreamIntegrity deliberately excludes unverified routes.
func NewAliyunDeepSeekStreamIntegrity(kind, model string) *OpenAIStreamIntegrity {
	if !strings.EqualFold(strings.TrimSpace(kind), "qwen") || !strings.EqualFold(strings.TrimSpace(model), "deepseek-v4.1-flash") {
		return nil
	}
	return &OpenAIStreamIntegrity{}
}

// Observe validates a terminal marker before a translator can emit completion.
func (s *OpenAIStreamIntegrity) Observe(line []byte) error {
	if s == nil || !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(data, []byte("[DONE]")) {
		s.doneSeen = true
		return s.Finish()
	}
	root := gjson.ParseBytes(data)
	if upstreamErr := root.Get("error"); upstreamErr.Exists() && upstreamErr.Type != gjson.Null {
		// HTTP 200 only establishes the transport. An error inside the stream
		// must never be translated or published as a successful usage record.
		digest := sha256.Sum256(data)
		return &failurecontract.Failure{
			Kind: failurecontract.UpstreamProtocolError, Scope: failurecontract.ScopeProvider,
			HTTPStatus: http.StatusBadGateway, OuterStatus: http.StatusOK,
			ProviderCode: "upstream_stream_error", SemanticCode: "upstream_stream_error", Retryable: false,
			PublicMessage: fmt.Sprintf("upstream returned an error event inside an HTTP 200 stream; usage is unavailable; event_bytes=%d event_sha256=%x; no automatic replay of partial output", len(data), digest),
		}
	}
	for _, choice := range root.Get("choices").Array() {
		if reason := choice.Get("finish_reason"); reason.Type == gjson.String && strings.TrimSpace(reason.String()) != "" {
			s.finishSeen = true
		}
	}
	prompt, completion := root.Get("usage.prompt_tokens"), root.Get("usage.completion_tokens")
	if prompt.Type == gjson.Number && prompt.Float() >= 0 && completion.Type == gjson.Number && completion.Float() >= 0 {
		s.usageSeen = true
	} else if value := root.Get("usage"); value.Exists() && value.Type != gjson.Null {
		// Reject invalid usage before a translator can treat an empty object as
		// the final usage event and emit a successful terminal response.
		s.usageSeen = false
		return s.Finish()
	}
	return nil
}

// Finish rejects a clean transport EOF without a complete application result.
// No token estimates or automatic replays are introduced here.
func (s *OpenAIStreamIntegrity) Finish() error {
	if s == nil || (s.finishSeen && s.usageSeen) {
		return nil
	}
	code := "upstream_stream_incomplete"
	if s.finishSeen {
		code = "upstream_stream_usage_missing"
	}
	return &failurecontract.Failure{
		Kind: failurecontract.UpstreamProtocolError, Scope: failurecontract.ScopeProvider,
		HTTPStatus: http.StatusBadGateway, OuterStatus: http.StatusOK,
		ProviderCode: code, SemanticCode: code, Retryable: false,
		PublicMessage: fmt.Sprintf("upstream stream incomplete: finish_reason_received=%t usage_received=%t done_received=%t; no automatic replay of partial output", s.finishSeen, s.usageSeen, s.doneSeen),
	}
}
