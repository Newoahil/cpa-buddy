package policy

import (
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func ptr(v float64) *float64 { return &v }

func cred(key string, p domain.ProviderKind) domain.Credential {
	return domain.Credential{Key: key, Provider: p, Alias: key, ShortID: "s1"}
}

func acctSnap(key string, p domain.ProviderKind, used float64, reset *time.Time) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential: cred(key, p), OK: true,
		Windows: []domain.QuotaWindow{{Scope: domain.ScopeAccount, Name: "5h", UsedPercent: ptr(used), ResetAt: reset}},
	}
}

func failSnap(key string, p domain.ProviderKind, lastSuccess time.Time) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential: cred(key, p), OK: false, Failure: domain.FailureTransport,
		LastSuccessAt: lastSuccess,
	}
}

func report(p domain.ProviderKind, states map[string]domain.CredentialState, snaps ...domain.QuotaSnapshot) domain.Report {
	return domain.Report{
		GeneratedAt: t0,
		Providers: []domain.ProviderReport{{
			Provider: p, States: states, Snapshots: snaps,
		}},
	}
}

func defaultCfg() Config {
	return Config{
		Mode:                    Actionable,
		ChannelUnavailableAfter: 30 * time.Minute,
		MonitorDownAfter:        30 * time.Minute,
		OMCOutagePendingAfter:   2 * time.Hour,
	}
}

func kinds(alerts []domain.Alert) []domain.AlertKind {
	out := make([]domain.AlertKind, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Kind)
	}
	return out
}

func hasKind(alerts []domain.Alert, k domain.AlertKind) bool {
	for _, a := range alerts {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func TestLegacyPassesEverythingThrough(t *testing.T) {
	in := Input{
		Alerts: []domain.Alert{
			{Kind: domain.AlertQuotaThreshold}, {Kind: domain.AlertQuotaReset},
			{Kind: domain.AlertRecovered}, {Kind: domain.AlertCredential},
			{Kind: domain.AlertRateLimited}, {Kind: domain.AlertBootstrap},
		},
		Prev: []state.PendingItem{{ID: "x", Status: state.PendingOpen}},
		Now:  t0,
	}
	res := Evaluate(Config{Mode: Legacy}, in)
	if len(res.Immediate) != len(in.Alerts) {
		t.Fatalf("legacy immediate = %d, want %d", len(res.Immediate), len(in.Alerts))
	}
	if len(res.Items) != 1 || res.Items[0].ID != "x" {
		t.Fatalf("legacy items = %+v, want pass-through", res.Items)
	}
	if len(res.Dropped) != 0 || len(res.Opened) != 0 {
		t.Fatalf("legacy must not drop or open: dropped=%v opened=%v", res.Dropped, res.Opened)
	}
}

// TestActionableDropsNoisyEvents is the table of "new home" for the old events.
func TestActionableDropsNoisyEvents(t *testing.T) {
	now := t0
	reset := now.Add(5 * time.Minute)
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateNotice},
		acctSnap("k1", domain.ProviderCodex, 92, &reset))
	alerts := []domain.Alert{
		{Kind: domain.AlertQuotaThreshold, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertQuotaExhausted, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertQuotaReset, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertRecovered, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertRateLimited, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertRateLimitCleared, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertStale, Credential: cred("k1", domain.ProviderCodex)},
		{Kind: domain.AlertSuspect, Credential: cred("k1", domain.ProviderCodex)},
	}
	res := Evaluate(defaultCfg(), Input{Report: rep, Alerts: alerts, Now: now})
	if len(res.Immediate) != 0 {
		t.Fatalf("noisy events pushed: %v", kinds(res.Immediate))
	}
	if len(res.Dropped) != len(alerts) {
		t.Fatalf("dropped = %d, want %d", len(res.Dropped), len(alerts))
	}
}

func TestBootstrapAlwaysPassesThrough(t *testing.T) {
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateHealthy},
		acctSnap("k1", domain.ProviderCodex, 10, nil))
	res := Evaluate(defaultCfg(), Input{Report: rep, Alerts: []domain.Alert{{Kind: domain.AlertBootstrap}}, Now: t0})
	if !hasKind(res.Immediate, domain.AlertBootstrap) {
		t.Fatalf("bootstrap dropped: %v", kinds(res.Immediate))
	}
}

func TestCredentialInvalidPushesOnceThenResolves(t *testing.T) {
	rep := report(domain.ProviderClaude, map[string]domain.CredentialState{"k1": domain.StateInvalid},
		domain.QuotaSnapshot{Credential: cred("k1", domain.ProviderClaude)})
	alert := domain.Alert{Kind: domain.AlertCredential, Severity: domain.SeverityUrgent,
		Credential: cred("k1", domain.ProviderClaude), Title: "凭证失效"}

	first := Evaluate(defaultCfg(), Input{Report: rep, Alerts: []domain.Alert{alert}, Now: t0})
	if !hasKind(first.Immediate, domain.AlertCredential) {
		t.Fatalf("credential_invalid not pushed: %v", kinds(first.Immediate))
	}
	if len(first.Opened) != 1 || first.Opened[0].Kind != string(domain.AlertCredential) {
		t.Fatalf("pending not opened: %+v", first.Opened)
	}

	// Same condition next round: no re-push.
	second := Evaluate(defaultCfg(), Input{Report: rep, Alerts: []domain.Alert{alert}, Prev: first.Items, Now: t0.Add(time.Minute)})
	if len(second.Immediate) != 0 {
		t.Fatalf("credential_invalid re-pushed: %v", kinds(second.Immediate))
	}
	if len(second.Opened) != 0 {
		t.Fatalf("already-open item reopened: %+v", second.Opened)
	}

	// Recovered: the credential is valid again.
	okRep := report(domain.ProviderClaude, map[string]domain.CredentialState{"k1": domain.StateHealthy},
		acctSnap("k1", domain.ProviderClaude, 10, nil))
	third := Evaluate(defaultCfg(), Input{Report: okRep, Prev: second.Items, Now: t0.Add(2 * time.Minute)})
	if len(third.Resolved) != 1 {
		t.Fatalf("credential_invalid not resolved: %+v", third.Items)
	}
	if third.Items[0].Status != state.PendingResolved {
		t.Fatalf("item status = %q, want resolved", third.Items[0].Status)
	}
}

func TestCredentialInvalidIDStable(t *testing.T) {
	a := ItemID(domain.AlertCredential, domain.ProviderCodex, "k1", "")
	b := ItemID(domain.AlertCredential, domain.ProviderCodex, "k1", "")
	c := ItemID(domain.AlertCredential, domain.ProviderCodex, "k2", "")
	if a != b {
		t.Fatalf("ID not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("different credentials must not collide")
	}
}

func TestChannelUnavailablePushesWhenRecoveryIsFar(t *testing.T) {
	now := t0
	reset := now.Add(2 * time.Hour)
	snap := acctSnap("k1", domain.ProviderCodex, 100, &reset)
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateExhausted}, snap)

	res := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if !hasKind(res.Immediate, domain.AlertChannelUnavailable) {
		t.Fatalf("far recovery not pushed: %v", kinds(res.Immediate))
	}
	if len(res.Opened) != 1 || res.Opened[0].Kind != string(domain.AlertChannelUnavailable) {
		t.Fatalf("channel_unavailable not opened: %+v", res.Opened)
	}

	// No repeat while it lasts.
	again := Evaluate(defaultCfg(), Input{Report: rep, Prev: res.Items, Now: now.Add(time.Minute)})
	if len(again.Immediate) != 0 {
		t.Fatalf("repeated channel_unavailable: %v", kinds(again.Immediate))
	}
}

func TestChannelUnavailableSuppressedWhenRecoveryIsNear(t *testing.T) {
	now := t0
	reset := now.Add(10 * time.Minute)
	snap := acctSnap("k1", domain.ProviderCodex, 100, &reset)
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateExhausted}, snap)
	res := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if len(res.Immediate) != 0 || len(res.Opened) != 0 {
		t.Fatalf("near recovery must not alert: immediate=%v opened=%v", kinds(res.Immediate), res.Opened)
	}
}

func TestChannelUnavailableIgnoresModelScopedExhaustion(t *testing.T) {
	now := t0
	u := 100.0
	snap := domain.QuotaSnapshot{
		Credential: cred("k1", domain.ProviderCodex), OK: true,
		Windows: []domain.QuotaWindow{{Scope: domain.ScopeModel, ScopeID: "m1", Name: "5h", UsedPercent: &u}},
	}
	// Model-only exhaustion projects to StateLimited, not account exhaustion.
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateLimited}, snap)
	res := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if len(res.Immediate) != 0 || len(res.Opened) != 0 {
		t.Fatalf("model-scoped exhaustion alerted: immediate=%v opened=%v", kinds(res.Immediate), res.Opened)
	}
}

func TestChannelUnavailableNotPushedWhenAllDisabled(t *testing.T) {
	now := t0
	reset := now.Add(2 * time.Hour)
	snap := acctSnap("k1", domain.ProviderCodex, 100, &reset)
	snap.Credential.Disabled = true
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateExhausted}, snap)
	res := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if len(res.Immediate) != 0 || len(res.Opened) != 0 {
		t.Fatalf("all-disabled channel alerted: immediate=%v opened=%v", kinds(res.Immediate), res.Opened)
	}
}

func TestChannelUnavailableResolvesOnRecovery(t *testing.T) {
	now := t0
	reset := now.Add(2 * time.Hour)
	down := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateExhausted},
		acctSnap("k1", domain.ProviderCodex, 100, &reset))
	first := Evaluate(defaultCfg(), Input{Report: down, Now: now})
	if len(first.Opened) != 1 {
		t.Fatalf("setup: %+v", first.Opened)
	}
	up := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateHealthy},
		acctSnap("k1", domain.ProviderCodex, 10, nil))
	second := Evaluate(defaultCfg(), Input{Report: up, Prev: first.Items, Now: now.Add(5 * time.Minute)})
	if len(second.Resolved) != 1 {
		t.Fatalf("channel_unavailable not resolved: %+v", second.Items)
	}
}

func TestMonitorDownJitterDoesNotPush(t *testing.T) {
	now := t0
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateStale},
		failSnap("k1", domain.ProviderCodex, now.Add(-time.Minute)))
	res := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if len(res.Immediate) != 0 || len(res.Opened) != 0 {
		t.Fatalf("jitter alerted: immediate=%v opened=%v", kinds(res.Immediate), res.Opened)
	}
}

func TestMonitorDownSustainedPushesOnceThenResolvesAndRepeats(t *testing.T) {
	now := t0
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateStale},
		failSnap("k1", domain.ProviderCodex, now.Add(-40*time.Minute)))
	first := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if !hasKind(first.Immediate, domain.AlertMonitorDown) {
		t.Fatalf("sustained outage not pushed: %v", kinds(first.Immediate))
	}
	if len(first.Opened) != 1 || first.Opened[0].Kind != string(domain.AlertMonitorDown) {
		t.Fatalf("monitor_down not opened: %+v", first.Opened)
	}

	// Still down: no repeat.
	second := Evaluate(defaultCfg(), Input{Report: rep, Prev: first.Items, Now: now.Add(time.Minute)})
	if len(second.Immediate) != 0 {
		t.Fatalf("monitor_down repeated: %v", kinds(second.Immediate))
	}

	// Recovered.
	okRep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateHealthy},
		acctSnap("k1", domain.ProviderCodex, 10, nil))
	third := Evaluate(defaultCfg(), Input{Report: okRep, Prev: second.Items, Now: now.Add(2 * time.Minute)})
	if len(third.Resolved) != 1 {
		t.Fatalf("monitor_down not resolved: %+v", third.Items)
	}

	// A new outage later pushes again.
	fourth := Evaluate(defaultCfg(), Input{Report: rep, Prev: third.Items, Now: now.Add(time.Hour)})
	if !hasKind(fourth.Immediate, domain.AlertMonitorDown) {
		t.Fatalf("second outage not pushed: %v", kinds(fourth.Immediate))
	}
}

func TestMonitorDownIgnoresAuthOnlyOutage(t *testing.T) {
	now := t0
	snap := failSnap("k1", domain.ProviderCodex, now.Add(-40*time.Minute))
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateInvalid}, snap)
	res := Evaluate(defaultCfg(), Input{Report: rep, Now: now})
	if hasKind(res.Immediate, domain.AlertMonitorDown) {
		t.Fatal("auth-only outage must not be monitor_down")
	}
}

func TestTightChannels(t *testing.T) {
	rep := domain.Report{Providers: []domain.ProviderReport{
		{Provider: domain.ProviderClaude, States: map[string]domain.CredentialState{"a": domain.StateNotice},
			Snapshots: []domain.QuotaSnapshot{acctSnap("a", domain.ProviderClaude, 85, nil)}},
		{Provider: domain.ProviderCodex, States: map[string]domain.CredentialState{"b": domain.StateHealthy},
			Snapshots: []domain.QuotaSnapshot{acctSnap("b", domain.ProviderCodex, 10, nil)}},
	}}
	out := TightChannels(rep, 20)
	if len(out) != 1 || out[0].Provider != domain.ProviderClaude {
		t.Fatalf("tight = %+v", out)
	}
	if out[0].BestRemaining == nil || *out[0].BestRemaining != 15 {
		t.Fatalf("best remaining = %v, want 15", out[0].BestRemaining)
	}
}
