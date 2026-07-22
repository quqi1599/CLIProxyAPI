package cliproxy

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestBuilderUsesSpreadRoutingStrategy(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Routing.Strategy = "spread"
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if service == nil || service.coreManager == nil {
		t.Fatal("Build() returned nil service or core manager")
	}

	selector := reflect.ValueOf(service.coreManager).Elem().FieldByName("selector")
	if !selector.IsValid() || selector.IsNil() {
		t.Fatal("core manager selector is nil")
	}
	selectorType := selector.Elem().Type().String()
	if !strings.Contains(selectorType, "SpreadSelector") {
		t.Fatalf("selector type = %s, want SpreadSelector", selectorType)
	}
}

func TestBuilderKeepsExplicitTranslatorRegistry(t *testing.T) {
	t.Parallel()

	translationRegistry := sdktranslator.NewRegistry()
	service, err := NewBuilder().
		WithConfig(&config.Config{}).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithTranslatorRegistry(translationRegistry).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if service.translatorRegistry != translationRegistry {
		t.Fatal("builder replaced the explicit translator registry")
	}
}
