package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	spreadDynamicTestProvider = "codex"
	spreadDynamicTestModel    = "gpt-5.5"
)

func TestSpreadDynamicWeight_DefaultPriorityPrefersHigherSuccessEWMA(t *testing.T) {
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	unreliable := spreadDynamicTestAuth("a-unreliable")
	reliable := spreadDynamicTestAuth("z-reliable")

	spreadDynamicObserve(selector, unreliable.ID, false, 0)
	spreadDynamicObserve(selector, reliable.ID, true, 500*time.Millisecond)

	picked := spreadDynamicPick(t, selector, unreliable, reliable)
	if picked.ID != reliable.ID {
		t.Fatalf("picked auth = %q, want higher-success auth %q", picked.ID, reliable.ID)
	}
	selector.MarkDone(picked.ID, spreadDynamicTestModel)
}

func TestSpreadDynamicWeight_DefaultPriorityPrefersLowerTTFT(t *testing.T) {
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	slow := spreadDynamicTestAuth("a-slow")
	fast := spreadDynamicTestAuth("z-fast")

	spreadDynamicObserve(selector, slow.ID, true, 3*time.Second)
	spreadDynamicObserve(selector, fast.ID, true, 100*time.Millisecond)

	picked := spreadDynamicPick(t, selector, slow, fast)
	if picked.ID != fast.ID {
		t.Fatalf("picked auth = %q, want lower-TTFT auth %q", picked.ID, fast.ID)
	}
	selector.MarkDone(picked.ID, spreadDynamicTestModel)
}

func TestSpreadDynamicWeight_DefaultPriorityPrefersLowerInflight(t *testing.T) {
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	overloaded := spreadDynamicTestAuth("a-overloaded")
	idle := spreadDynamicTestAuth("z-idle")

	for range 4 {
		selector.MarkPicked(spreadDynamicTestProvider, spreadDynamicTestModel, overloaded.ID)
		selector.MarkPicked(spreadDynamicTestProvider, spreadDynamicTestModel, idle.ID)
		selector.MarkDone(idle.ID, spreadDynamicTestModel)
	}

	picked := spreadDynamicPick(t, selector, overloaded, idle)
	if picked.ID != idle.ID {
		t.Fatalf("picked auth = %q, want lower-inflight auth %q", picked.ID, idle.ID)
	}
	selector.MarkDone(picked.ID, spreadDynamicTestModel)
}

func TestSpreadDynamicWeight_GPTSameChannelSharesCapacityAndRotatesCredentials(t *testing.T) {
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	firstCredential := spreadDynamicTestAuth("channel-key-a")
	secondCredential := spreadDynamicTestAuth("channel-key-b")
	secondCredential.Attributes["base_url"] = firstCredential.Attributes["base_url"]

	first := spreadDynamicPick(t, selector, firstCredential, secondCredential)
	second := spreadDynamicPick(t, selector, firstCredential, secondCredential)
	if first.ID == second.ID {
		t.Fatalf("same channel picked credential %q twice, want credential rotation", first.ID)
	}

	key := spreadDynamicTestProvider + ":" + canonicalModelKey(spreadDynamicTestModel)
	channelKey := routingChannelBaseKey(firstCredential)
	selector.mu.Lock()
	records := selector.load.snapshot(key, []string{channelKey}, time.Now(), spreadLoadDefaultKeyLimit)
	recordCount := len(selector.load.records[key])
	selector.mu.Unlock()
	if recordCount != 1 {
		t.Fatalf("channel load record count = %d, want one shared record", recordCount)
	}
	if got := records[channelKey].inFlight; got != 2 {
		t.Fatalf("shared channel inflight = %d, want 2", got)
	}

	selector.MarkResult(first.ID, spreadDynamicTestModel, true, 100*time.Millisecond)
	sharedAfterResult := spreadDynamicRecord(selector, second.ID)
	if got := sharedAfterResult.inFlight; got != 1 {
		t.Fatalf("shared inflight after first result = %d, want 1", got)
	}
	if !sharedAfterResult.outcomeObserved || !sharedAfterResult.ttftObserved {
		t.Fatalf("shared outcome = %+v, want success and TTFT visible to peer credential", sharedAfterResult)
	}
	selector.MarkDone(second.ID, spreadDynamicTestModel)
	if got := spreadDynamicRecord(selector, first.ID).inFlight; got != 0 {
		t.Fatalf("shared inflight after both releases = %d, want 0", got)
	}
}

func TestSpreadDynamicWeight_GPTCredentialCountDoesNotMultiplyChannelWeight(t *testing.T) {
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	channelAKey1 := spreadDynamicTestAuth("channel-a-key-1")
	channelAKey2 := spreadDynamicTestAuth("channel-a-key-2")
	channelAKey2.Attributes["base_url"] = channelAKey1.Attributes["base_url"]
	channelB := spreadDynamicTestAuth("channel-b-key")
	channelA := routingChannelBaseKey(channelAKey1)
	channelBKey := routingChannelBaseKey(channelB)

	counts := map[string]int{}
	credentialCounts := map[string]int{}
	for range 30 {
		picked := spreadDynamicPick(t, selector, channelAKey1, channelAKey2, channelB)
		counts[routingChannelBaseKey(picked)]++
		credentialCounts[picked.ID]++
		selector.MarkDone(picked.ID, spreadDynamicTestModel)
	}

	if counts[channelA] != counts[channelBKey] {
		t.Fatalf("channel counts = %+v, want equal capacity despite two credentials on channel A", counts)
	}
	if credentialCounts[channelAKey1.ID] == 0 || credentialCounts[channelAKey2.ID] == 0 {
		t.Fatalf("channel A credential counts = %+v, want rotation within selected channel", credentialCounts)
	}
}

func TestSpreadDynamicWeight_NonGPTIgnoresOutcomeAndTTFTFactors(t *testing.T) {
	const (
		provider = "claude"
		model    = "claude-3-7-sonnet"
	)
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	first := &Auth{ID: "a-first", Provider: provider, Status: StatusActive}
	second := &Auth{ID: "z-second", Provider: provider, Status: StatusActive}

	selector.MarkPicked(provider, model, first.ID)
	selector.MarkResult(first.ID, model, false, 3*time.Second)
	selector.MarkPicked(provider, model, second.ID)
	selector.MarkResult(second.ID, model, true, 100*time.Millisecond)

	picked, errPick := selector.Pick(context.Background(), provider, model, cliproxyexecutor.Options{}, []*Auth{first, second})
	if errPick != nil {
		t.Fatalf("pick non-GPT auth: %v", errPick)
	}
	if picked.ID != first.ID {
		t.Fatalf("non-GPT picked auth = %q, want legacy tie-break %q", picked.ID, first.ID)
	}
	selector.MarkDone(picked.ID, model)
}

func TestManagerMarkResult_UpdatesSpreadOutcomeAndReleasesInflight(t *testing.T) {
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	manager := NewManager(nil, selector, nil)
	auth := spreadDynamicTestAuth("result-release")
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	selector.MarkPicked(spreadDynamicTestProvider, spreadDynamicTestModel, auth.ID)
	before := spreadDynamicRecord(selector, auth.ID)
	if before.inFlight != 1 {
		t.Fatalf("inflight before MarkResult = %d, want 1", before.inFlight)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: spreadDynamicTestProvider,
		Model:    spreadDynamicTestModel,
		Success:  true,
		Duration: 250 * time.Millisecond,
		TTFT:     100 * time.Millisecond,
	})

	after := spreadDynamicRecord(selector, auth.ID)
	if after.inFlight != 0 {
		t.Fatalf("inflight after MarkResult = %d, want 0", after.inFlight)
	}
	if !after.outcomeObserved || after.successEWMA != 1 {
		t.Fatalf("outcome after MarkResult = observed:%v ewma:%v, want observed success", after.outcomeObserved, after.successEWMA)
	}
	if !after.ttftObserved || after.ttftEWMA != 100*time.Millisecond {
		t.Fatalf("TTFT after MarkResult = observed:%v ewma:%v, want 100ms", after.ttftObserved, after.ttftEWMA)
	}
}

func TestManagerMarkResult_ReleasesSpreadInflightByRequestedAlias(t *testing.T) {
	const alias = "gpt-latest"
	selector := &SpreadSelector{load: newSpreadLoadTracker()}
	manager := NewManager(nil, selector, nil)
	auth := spreadDynamicTestAuth("alias-release")
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	selector.MarkPicked(spreadDynamicTestProvider, alias, auth.ID)
	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: spreadDynamicTestProvider,
		Model:    spreadDynamicTestModel,
		Success:  true,
		TTFT:     100 * time.Millisecond,
	})

	if got := spreadDynamicRecordForModel(selector, alias, auth.ID).inFlight; got != 0 {
		t.Fatalf("alias inflight after MarkResult = %d, want 0", got)
	}
}

func spreadDynamicTestAuth(id string) *Auth {
	return &Auth{
		ID:       id,
		Provider: spreadDynamicTestProvider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":  "test-" + id,
			"base_url": "https://" + id + ".example.com/v1",
		},
	}
}

func spreadDynamicObserve(selector *SpreadSelector, authID string, success bool, ttft time.Duration) {
	selector.MarkPicked(spreadDynamicTestProvider, spreadDynamicTestModel, authID)
	selector.MarkResult(authID, spreadDynamicTestModel, success, ttft)
}

func spreadDynamicPick(t *testing.T, selector *SpreadSelector, auths ...*Auth) *Auth {
	t.Helper()
	picked, errPick := selector.Pick(
		context.Background(),
		spreadDynamicTestProvider,
		spreadDynamicTestModel,
		cliproxyexecutor.Options{},
		auths,
	)
	if errPick != nil {
		t.Fatalf("pick auth: %v", errPick)
	}
	if picked == nil {
		t.Fatal("picked auth is nil")
	}
	return picked
}

func spreadDynamicRecord(selector *SpreadSelector, authID string) spreadLoadRecord {
	return spreadDynamicRecordForModel(selector, spreadDynamicTestModel, authID)
}

func spreadDynamicRecordForModel(selector *SpreadSelector, model, authID string) spreadLoadRecord {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	key := spreadDynamicTestProvider + ":" + canonicalModelKey(model)
	recordKey := selector.load.recordKey(key, authID)
	snapshot := selector.load.snapshot(key, []string{recordKey}, time.Now(), spreadLoadDefaultKeyLimit)
	return snapshot[recordKey]
}
