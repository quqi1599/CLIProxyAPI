package pluginhost

import (
	"strings"
	"testing"
)

func TestPluginCallErrorDoesNotExposeBody(t *testing.T) {
	const secret = "plugin-call-error-sentinel"
	err := pluginCallError("execute", 7, []byte(`{"error":"`+secret+`"}`))
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), `"sha256":`) {
		t.Fatalf("unsafe plugin call error: %v", err)
	}
}
