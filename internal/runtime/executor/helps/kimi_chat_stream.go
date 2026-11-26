package helps

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/tidwall/gjson"
)

// KimiChatStream checks upstream completion before an SSE translator can
// synthesize its downstream terminal event. It retains no response data.
type KimiChatStream struct {
	finished bool
	done     bool
}

func (s *KimiChatStream) Observe(event []byte) error {
	if s == nil {
		return nil
	}
	for line := range bytes.FieldsFuncSeq(event, func(r rune) bool { return r == '\r' || r == '\n' }) {
		line = bytes.TrimSpace(line)
		if name, ok := bytes.CutPrefix(line, []byte("event:")); ok && bytes.Equal(bytes.TrimSpace(name), []byte("error")) {
			return kimiStreamError()
		}
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		switch {
		case bytes.Equal(data, []byte("[DONE]")):
			s.done = true
		case len(data) == 0 || bytes.Equal(data, []byte("ping")):
			// Keep-alives are not completion evidence.
		case !gjson.ValidBytes(data):
			return kimiStreamIncomplete(nil)
		default:
			root := gjson.ParseBytes(data)
			upstreamErr := root.Get("error")
			if (upstreamErr.Exists() && upstreamErr.Type != gjson.Null) || root.Get("type").String() == "error" || root.Get("object").String() == "error" {
				return kimiStreamError()
			}
			if reason := root.Get("choices.0.finish_reason"); reason.Type == gjson.String && strings.TrimSpace(reason.Str) != "" {
				s.finished = true
			}
		}
	}
	return nil
}

func (s *KimiChatStream) Done() bool {
	return s != nil && s.done
}

func (s *KimiChatStream) Finish() error {
	if s == nil || s.done || s.finished {
		return nil
	}
	return kimiStreamIncomplete(io.ErrUnexpectedEOF)
}

// KimiChatAcceptedFailure stops replay after the upstream has accepted the
// request, even if the broken stream never reached a downstream client.
func KimiChatAcceptedFailure(err error, upstreamStatus int) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = kimiStreamIncomplete(err)
	}
	failure := failurecontract.Classify(err)
	failure.Scope = failurecontract.ScopeRequest
	failure.OuterStatus = upstreamStatus
	failure.Retryable = false
	return failure
}

func kimiStreamIncomplete(cause error) *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind: failurecontract.UpstreamProtocolError, HTTPStatus: http.StatusBadGateway,
		SemanticCode: "upstream_stream_incomplete", Cause: cause,
		PublicMessage: "Kimi upstream stream ended before completion",
	}
}

func kimiStreamError() *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind: failurecontract.UpstreamProtocolError, HTTPStatus: http.StatusBadGateway,
		SemanticCode:  "upstream_stream_error",
		PublicMessage: "Kimi upstream returned an error inside the stream",
	}
}
