package cliproxy

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
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
