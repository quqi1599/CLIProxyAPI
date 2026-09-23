package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketSelectionErrorPreservesTypedCodeAndRetryAfter(t *testing.T) {
	wait := 1500 * time.Millisecond
	localSelection := &failurecontract.Failure{
		Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeProvider,
		HTTPStatus: 503, OuterStatus: 503, SemanticCode: "auth_unavailable",
		StreamPhase: failurecontract.StreamPhaseBeforeOutput, RetryAfter: &wait,
		PublicMessage: "auth_unavailable: no auth available",
		Cause:         &coreauth.Error{Code: "auth_unavailable", HTTPStatus: 503},
	}
	for _, test := range []struct {
		name     string
		cause    error
		wantCode string
	}{
		{name: "typed local selection", cause: localSelection, wantCode: "auth_unavailable"},
		{name: "plain upstream text", cause: errors.New(localSelection.Error()), wantCode: "internal_server_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			serverErrors := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					serverErrors <- err
					return
				}
				defer func() { _ = conn.Close() }()
				addon := localSelection.Headers()
				addon.Set("Authorization", "must-not-be-forwarded")
				_, err = writeResponsesWebsocketError(conn, nil, &interfaces.ErrorMessage{
					StatusCode: 503, Error: test.cause, Addon: addon,
				}, false)
				serverErrors <- err
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, frame, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if err := <-serverErrors; err != nil {
				t.Fatalf("server error: %v", err)
			}
			if gjson.GetBytes(frame, "type").String() != "error" || gjson.GetBytes(frame, "status").Int() != 503 {
				t.Fatalf("invalid websocket error envelope: %s", frame)
			}
			if got := gjson.GetBytes(frame, "error.code").String(); got != test.wantCode {
				t.Fatalf("code=%q want %q; frame=%s", got, test.wantCode, frame)
			}
			if got := gjson.GetBytes(frame, "headers.Retry-After").String(); got != "2" {
				t.Fatalf("Retry-After=%q want 2; frame=%s", got, frame)
			}
			if gjson.GetBytes(frame, "headers.Authorization").Exists() {
				t.Fatal("unsafe addon header was forwarded")
			}
		})
	}
}
