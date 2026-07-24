package cliproxy

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestServiceRoutingSelectorStartupAndHotReloadMatch(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
	}{
		{name: "round robin", strategy: coreauth.RoutingStrategyRoundRobin},
		{name: "fill first", strategy: coreauth.RoutingStrategyFillFirst},
		{name: "sequential fill", strategy: coreauth.RoutingStrategySequentialFill},
		{name: "spread", strategy: coreauth.RoutingStrategySpread},
	}

	for _, tt := range tests {
		for _, affinity := range []bool{false, true} {
			t.Run(tt.name+"/affinity="+strconv.FormatBool(affinity), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Routing.Strategy = tt.strategy
				cfg.Routing.SessionAffinity = affinity
				cfg.Routing.SessionAffinityTTL = "45m"

				startupSelector := serviceRoutingSelector(
					cfg.Routing.Strategy,
					cfg.Routing.SessionAffinity,
					cfg.Routing.SessionAffinityTTL,
				)
				startupShape := serviceRoutingSelectorShape(t, reflect.ValueOf(startupSelector))
				if stoppable, ok := startupSelector.(interface{ Stop() }); ok {
					t.Cleanup(stoppable.Stop)
				}

				manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
				service := &Service{
					cfg:         &config.Config{},
					coreManager: manager,
				}
				t.Cleanup(func() {
					manager.StopAutoRefresh()
					_ = service.shutdownPprof(context.Background())
				})

				service.applyWatcherConfigUpdate(cfg)
				hotReloadShape := serviceManagerRoutingSelectorShape(t, manager)
				if hotReloadShape != startupShape {
					t.Fatalf("hot reload selector = %+v, startup selector = %+v", hotReloadShape, startupShape)
				}
				if affinity && hotReloadShape.ttl != 45*time.Minute {
					t.Fatalf("session affinity TTL = %v, want %v", hotReloadShape.ttl, 45*time.Minute)
				}
			})
		}
	}
}

type serviceRoutingSelectorShapeValue struct {
	selector string
	fallback string
	ttl      time.Duration
}

func serviceManagerRoutingSelectorShape(t *testing.T, manager *coreauth.Manager) serviceRoutingSelectorShapeValue {
	t.Helper()
	selector := reflect.ValueOf(manager).Elem().FieldByName("selector")
	if !selector.IsValid() || selector.IsNil() {
		t.Fatal("manager selector is nil")
	}
	return serviceRoutingSelectorShape(t, selector)
}

func serviceRoutingSelectorShape(t *testing.T, selector reflect.Value) serviceRoutingSelectorShapeValue {
	t.Helper()
	for selector.IsValid() && selector.Kind() == reflect.Interface {
		selector = selector.Elem()
	}
	if !selector.IsValid() || selector.Kind() != reflect.Pointer || selector.IsNil() {
		t.Fatalf("invalid selector value: %v", selector)
	}

	shape := serviceRoutingSelectorShapeValue{selector: selector.Type().String()}
	if selector.Type() != reflect.TypeOf((*coreauth.SessionAffinitySelector)(nil)) {
		return shape
	}

	fallback := selector.Elem().FieldByName("fallback")
	for fallback.IsValid() && fallback.Kind() == reflect.Interface {
		fallback = fallback.Elem()
	}
	if !fallback.IsValid() || fallback.IsNil() {
		t.Fatal("session affinity fallback is nil")
	}
	shape.fallback = fallback.Type().String()

	cache := selector.Elem().FieldByName("cache")
	if !cache.IsValid() || cache.IsNil() {
		t.Fatal("session affinity cache is nil")
	}
	shape.ttl = time.Duration(cache.Elem().FieldByName("ttl").Int())
	return shape
}
