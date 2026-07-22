package builtin

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestNewRegistryReturnsIsolatedBuiltins(t *testing.T) {
	first := NewRegistry()
	second := NewRegistry()
	if first == second || first == Registry() || second == Registry() {
		t.Fatal("built-in registries are not isolated instances")
	}
	if !first.HasRequestTransformer(sdktranslator.FormatOpenAI, sdktranslator.FormatClaude) ||
		!second.HasRequestTransformer(sdktranslator.FormatOpenAI, sdktranslator.FormatClaude) {
		t.Fatal("isolated registry is missing built-in OpenAI to Claude translation")
	}

	from := sdktranslator.Format("isolated-builtin-from")
	to := sdktranslator.Format("isolated-builtin-to")
	first.Register(from, to, func(_ string, body []byte, _ bool) []byte { return body }, sdktranslator.ResponseTransform{})
	if second.HasRequestTransformer(from, to) {
		t.Fatal("registration leaked into another isolated registry")
	}
	if Registry().HasRequestTransformer(from, to) {
		t.Fatal("registration leaked into the default facade")
	}
}
