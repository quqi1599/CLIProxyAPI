package watcher

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexNativeResponsesHotReload(t *testing.T) {
	enabled, disabled := true, false
	queue := make(chan AuthUpdate, 8)
	w := &Watcher{authDir: t.TempDir(), lastAuthHashes: make(map[string]string)}
	w.SetAuthUpdateQueue(queue)
	defer w.stopDispatch()
	var authID string
	for i, value := range []*bool{nil, &enabled, &disabled, nil} {
		w.SetConfig(&config.Config{AuthDir: w.authDir, CodexKey: []config.CodexKey{{
			APIKey: "test-key", BaseURL: "https://example.test/v1", NativeResponses: value,
		}}})
		w.refreshAuthState(false)
		select {
		case update := <-queue:
			wantAction := AuthUpdateActionModify
			if i == 0 {
				wantAction = AuthUpdateActionAdd
				authID = update.ID
			}
			if update.Action != wantAction || update.ID != authID {
				t.Fatalf("reload %d changed identity/action: %s %s", i, update.Action, update.ID)
			}
			want := ""
			if value != nil {
				want = "false"
				if *value {
					want = "true"
				}
			}
			if got := update.Auth.Attributes["native_responses"]; got != want {
				t.Fatalf("reload %d: attribute=%q, want %q", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("reload %d: no auth update dispatched", i)
		}
	}
}
