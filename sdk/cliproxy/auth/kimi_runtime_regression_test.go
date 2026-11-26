package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestKimiRuntimeQuotaSurvivesConfigReload(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := kimiQuotaTestAuth("reload", "openai-compatible-kimi", "synthetic-key")
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	wait := 24 * time.Hour
	cause := &failurecontract.Failure{Kind: failurecontract.QuotaExceeded, Scope: failurecontract.ScopeCredential, HTTPStatus: 429, RetryAfter: &wait, Retryable: true, PublicMessage: "synthetic quota exhausted"}
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "kimi-k3.1-pro", Cause: cause})
	before, _ := m.GetByID(a.ID)
	if blocked, _, _ := isAuthBlockedForModel(before, "kimi-k3.1-pro", time.Now()); !blocked {
		t.Fatal("baseline quota not blocked")
	}
	fresh := kimiQuotaTestAuth(a.ID, a.Provider, "synthetic-key")
	if _, err := m.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	after, _ := m.GetByID(a.ID)
	if blocked, _, _ := isAuthBlockedForModel(after, "kimi-k3.1-pro", time.Now()); !blocked {
		t.Fatal("config reload erased credential cooldown")
	}
	if after.Quota != before.Quota || !after.NextRetryAfter.Equal(before.NextRetryAfter) {
		t.Fatal("quota recovery changed during reload")
	}
}

func TestKimiRuntimeModelStateCannotBypassQuota(t *testing.T) {
	now := time.Now()
	for _, retry := range []time.Time{{}, now.Add(-time.Minute), now.Add(time.Second)} {
		a := kimiQuotaTestAuth("model", "kimi", "synthetic-key")
		a.Unavailable, a.Quota.Exceeded = true, true
		a.NextRetryAfter = now.Add(24 * time.Hour)
		a.ModelStates = map[string]*ModelState{"kimi-k3.1-pro": {Unavailable: true, NextRetryAfter: retry}}
		blocked, _, next := isAuthBlockedForModel(a, "kimi-k3.1-pro", now)
		if !blocked || next.Before(a.NextRetryAfter) {
			t.Fatalf("model state bypassed credential cooldown: blocked=%v next=%v", blocked, next)
		}
	}
}

func TestKimiRuntimeAliasSuccessCannotClearQuota(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := kimiQuotaTestAuth("alias-success", "openai-compatible-kimi", "synthetic-key")
	a.Attributes["compat_name"] = "kimi"
	a.ModelStates = map[string]*ModelState{"kimi-k3.1-pro": {Status: StatusActive}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "k3.1-pro", Cause: kimiInsufficientQuotaTestFailure()})
	ctx := coreusage.WithRequestedModelAlias(context.Background(), "kimi-k3.1-pro")
	m.MarkResult(ctx, Result{AuthID: a.ID, Provider: a.Provider, Model: "k3.1-pro", Success: true})
	after, _ := m.GetByID(a.ID)
	if blocked, _, _ := isAuthBlockedForModel(after, "kimi-k3.1-pro", time.Now()); !blocked {
		t.Fatal("late alias success cleared active account cooldown")
	}
}

func TestKimiRuntimeSpreadCountsPhysicalKeys(t *testing.T) {
	s := &SpreadSelector{channelAware: true}
	a := kimiQuotaTestAuth("a-openai", "openai-compatible-kimi", "key-a")
	a.Attributes["base_url"] = "https://api.kimi.com/coding/v1"
	alias := kimiQuotaTestAuth("a-claude", "claude", "key-a")
	alias.Attributes["base_url"] = "https://api.kimi.com/coding"
	b := kimiQuotaTestAuth("b-openai", "openai-compatible-kimi", "key-b")
	b.Attributes["base_url"] = a.Attributes["base_url"]
	counts := map[string]int{}
	for range 60 {
		picked, err := s.Pick(context.Background(), "group:kimi", "kimi-k3.1-pro", cliproxyexecutor.Options{}, []*Auth{a, alias, b})
		if err != nil {
			t.Fatal(err)
		}
		counts[kimiCredentialFingerprint(picked)]++
		s.MarkRouteDone("group:kimi", picked.ID, "kimi-k3.1-pro")
	}
	if counts[kimiCredentialFingerprint(a)] != 30 || counts[kimiCredentialFingerprint(b)] != 30 {
		t.Fatalf("protocol aliases multiplied physical key share: %v", counts)
	}
}

func TestKimiRuntimeChangedKeyAndDisableDoNotInheritQuota(t *testing.T) {
	for _, change := range []string{"rotate", "disable", "disable-cooling"} {
		t.Run(change, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			a := kimiQuotaTestAuth("lifecycle", "kimi", "old-key")
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "k3.1-pro", Cause: kimiInsufficientQuotaTestFailure()})
			fresh := kimiQuotaTestAuth(a.ID, a.Provider, "old-key")
			switch change {
			case "rotate":
				fresh.Attributes[AttributeAPIKey] = "new-key"
			case "disable":
				fresh.Disabled = true
				fresh.Status = StatusDisabled
			case "disable-cooling":
				fresh.Metadata = map[string]any{"disable_cooling": true}
			}
			after, err := m.Update(context.Background(), fresh)
			if err != nil {
				t.Fatal(err)
			}
			if after.Quota.Exceeded || after.Unavailable || !after.NextRetryAfter.IsZero() {
				t.Fatalf("%s inherited previous cooldown", change)
			}
		})
	}
}

type kimiUpdateBarrierStore struct {
	started, release chan struct{}
	once             sync.Once
}

func (s *kimiUpdateBarrierStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *kimiUpdateBarrierStore) Delete(context.Context, string) error  { return nil }
func (s *kimiUpdateBarrierStore) Save(_ context.Context, a *Auth) (string, error) {
	if a.Metadata["block_update"] == true {
		s.once.Do(func() { close(s.started); <-s.release })
	}
	return a.ID, nil
}

func TestKimiRuntimeQuotaArrivingDuringUpdateIsRetained(t *testing.T) {
	s := &kimiUpdateBarrierStore{started: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(s, nil, nil)
	a := kimiQuotaTestAuth("concurrent-update", "kimi", "same-key")
	a.Metadata = map[string]any{"block_update": false}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	next := a.Clone()
	next.Metadata["block_update"] = true
	done := make(chan error, 1)
	go func() { _, err := m.Update(context.Background(), next); done <- err }()
	select {
	case <-s.started:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not reach persistence barrier")
	}
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "k3.1-pro", Cause: kimiInsufficientQuotaTestFailure()})
	close(s.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _ := m.GetByID(a.ID)
	if blocked, _, _ := isAuthBlockedForModel(after, "k3.1-pro", time.Now()); !blocked {
		t.Fatal("concurrent quota failure lost during update")
	}
	if after.Failed != 1 {
		t.Fatal("concurrent result count lost")
	}
}

func TestKimiRuntimeSpreadSharesActiveLoadAndReleases(t *testing.T) {
	s := &SpreadSelector{channelAware: true}
	a := kimiQuotaTestAuth("a-openai", "kimi", "busy-key")
	alias := kimiQuotaTestAuth("a-claude", "claude", "busy-key")
	b := kimiQuotaTestAuth("b", "kimi", "idle-key")
	for range 10 {
		s.markPickedAuth("group:kimi", "kimi-k3", a)
	}
	picked, err := s.Pick(context.Background(), "group:kimi", "kimi-k3", cliproxyexecutor.Options{}, []*Auth{alias, b})
	if err != nil || picked.ID != b.ID {
		t.Fatalf("busy key hid load behind protocol alias: picked=%v err=%v", picked, err)
	}
	s.MarkRouteDone("group:kimi", picked.ID, "kimi-k3")
	for range 10 {
		s.MarkRouteDone("group:kimi", a.ID, "kimi-k3")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.load.records["group:kimi:kimi-k3"] {
		if record.inFlight != 0 {
			t.Fatal("physical key load leaked after completion")
		}
	}
}

func TestKimiForbiddenCooldownHonoredBySelector(t *testing.T) {
	now := time.Now()
	model := "kimi-k2.7"
	m := NewManager(nil, &SpreadSelector{channelAware: true}, nil)
	denied := kimiQuotaTestAuth("denied", "claude", "synthetic-a")
	denied.ModelStates = map[string]*ModelState{model: {
		Health: HealthState{Observed: true, BreakerState: HealthBreakerOpen, OpenUntil: now.Add(4 * time.Minute), LastStatusCode: 403},
	}}
	for _, active := range []bool{false, true} {
		if active {
			m.reserveHalfOpenProbe(denied.ID, model, now)
		}
		got, err := m.availableAuthsForRouteModel([]*Auth{denied}, "mixed", model, now)
		if len(got) != 0 || statusCodeFromError(err) != 503 {
			t.Fatalf("active=%t: reselected a denied credential before its recovery time", active)
		}
	}
	ready := kimiQuotaTestAuth("ready", "kimi", "synthetic-b")
	got, err := m.availableAuthsForRouteModel([]*Auth{denied, ready}, "mixed", model, now)
	if err != nil || len(got) != 1 || got[0] != ready {
		t.Fatal("healthy alternative lost")
	}
	got, err = m.availableAuthsForRouteModel([]*Auth{denied}, "mixed", model, now.Add(5*time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatal("recovered credential never became eligible")
	}
}

func TestKimiPoolProofRequiresEveryEnabledEntrance(t *testing.T) {
	now := time.Now()
	m := NewManager(nil, nil, nil)
	model := "kimi-k2.6"
	first := kimiQuotaTestAuth("local-pool-proof-1", "claude", "a")
	second := kimiQuotaTestAuth("local-pool-proof-2", "kimi", "b")
	reg := registry.GetGlobalRegistry()
	for _, a := range []*Auth{first, second} {
		reg.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		m.auths[a.ID] = a
	}
	wait := 4 * time.Minute
	local := newRouteUnavailableError(&wait)
	var h HealthState
	applyHealthFailure(&h, now, 403)
	applyHealthSuccess(&h, now.Add(time.Second))
	first.ModelStates = map[string]*ModelState{model: {Health: h}}
	check := func(want bool) {
		t.Helper()
		e := m.markKimiPoolExhausted(local, model, now.Add(time.Second))
		if KimiPoolExhausted(e) != want || statusCodeFromError(e) != 503 {
			t.Fatalf("verified=%v expected %v", KimiPoolExhausted(e), want)
		}
	}
	check(false)
	second.Unavailable = true
	second.NextRetryAfter = now.Add(wait)
	second.LastError = &Error{HTTPStatus: 403}
	check(true)
	second.LastError = &Error{HTTPStatus: 500}
	check(false)
	if KimiPoolExhausted(local) {
		t.Fatal("shared selection failure was mutated")
	}
}
