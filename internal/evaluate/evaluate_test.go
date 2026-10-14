package evaluate

import (
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/calendar"
	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func baseCfg(t *testing.T) config.Config {
	return config.Config{
		DefaultThresholds:  config.Thresholds{Notice: 90, Warn: 95, Urgent: 100},
		ProviderThresholds: map[domain.ProviderKind]config.Thresholds{},
		StaleAfterFailures: 2,
		AnomalyConsecutive: 3,
		AnomalyWindow:      5 * time.Minute,
		AnomalyFailureRate: 0.2,
		AnomalyMinRequests: 5,
		Location:           shanghai(t),
	}
}

func newEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := baseCfg(t)
	return New(cfg, calendar.New(cfg.Location))
}

func fp(v float64) *float64     { return &v }
func tp(v time.Time) *time.Time { return &v }

func win(name string, used float64) domain.QuotaWindow {
	return domain.QuotaWindow{Name: name, UsedPercent: fp(used), Scope: domain.ScopeAccount}
}

func okSnap(key string, p domain.ProviderKind, windows ...domain.QuotaWindow) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential: domain.Credential{Key: key, Provider: p, Alias: "acct", ShortID: "x1"},
		Windows:    windows,
		Source:     domain.SourceCPA,
		Confidence: domain.ConfidenceReported,
		FetchedAt:  time.Unix(1000, 0).UTC(),
		OK:         true,
	}
}

func failSnap(key string, p domain.ProviderKind, errText string) domain.QuotaSnapshot {
	return failSnapKind(key, p, errText, domain.FailureTransport)
}

func failSnapKind(key string, p domain.ProviderKind, errText string, kind domain.FailureKind) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential:   domain.Credential{Key: key, Provider: p, Alias: "acct", ShortID: "x1"},
		Source:       domain.SourceCPA,
		Confidence:   domain.ConfidenceUnknown,
		OK:           false,
		Failure:      kind,
		FailureScope: domain.ScopeAccount, // this fixture explicitly models an account rejection
		Err:          errText,
	}
}

// recWith builds a record explicitly by dimension, as the state machine would
// have persisted it.
func recWith(st domain.CredentialState, q state.QuotaLevel, c state.CredentialHealth, f state.Freshness, reached bool) state.CredentialRecord {
	return state.CredentialRecord{
		State:         st,
		Quota:         q,
		Credential:    c,
		Freshness:     f,
		ReachedNotice: reached,
	}
}

func kinds(alerts []domain.Alert) []domain.AlertKind {
	out := make([]domain.AlertKind, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Kind)
	}
	return out
}

func countKind(alerts []domain.Alert, k domain.AlertKind) int {
	n := 0
	for _, a := range alerts {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func findAlert(alerts []domain.Alert, k domain.AlertKind) (domain.Alert, bool) {
	for _, a := range alerts {
		if a.Kind == k {
			return a, true
		}
	}
	return domain.Alert{}, false
}

func bootstrapped() *state.State {
	return &state.State{Bootstrapped: true, Credentials: map[string]state.CredentialRecord{}}
}

// ---------------------------------------------------------------------------
// Rule (a): per-window threshold, worst window wins
// ---------------------------------------------------------------------------

func TestThresholdPerWindow(t *testing.T) {
	eng := newEngine(t)
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		windows []domain.QuotaWindow
		want    domain.CredentialState
	}{
		{"below notice", []domain.QuotaWindow{win("5h", 10), win("7d", 50)}, domain.StateHealthy},
		{"at notice", []domain.QuotaWindow{win("5h", 10), win("7d", 90)}, domain.StateNotice},
		{"at warn", []domain.QuotaWindow{win("5h", 10), win("7d", 95)}, domain.StateWarning},
		{"at urgent", []domain.QuotaWindow{win("5h", 10), win("7d", 100)}, domain.StateExhausted},
		{"worst across windows", []domain.QuotaWindow{win("5h", 100), win("7d", 10)}, domain.StateExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, tc.windows...)}, now)
			if got := next.Credentials["k"].State; got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoNumbersIsUnknownNotHealthy(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	s := okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "5h"}) // nil percent
	_, _, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	if got := next.Credentials["k"].State; got != domain.StateUnknown {
		t.Errorf("state = %q, want %q (no number is unknown, never 0%% or healthy)", got, domain.StateUnknown)
	}
}

// ---------------------------------------------------------------------------
// Rule (b): per-dimension alert dedup
// ---------------------------------------------------------------------------

func TestAlertOnlyOnChange(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()

	// Same quota dimension as before: no alert.
	prev := bootstrapped()
	_, _, prev = eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 96))}, now)
	_, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 96))}, now)
	if countKind(alerts, domain.AlertQuotaThreshold) != 0 {
		t.Errorf("unchanged warning re-alerted: %v", kinds(alerts))
	}

	// Changed from nothing to warning: one alert.
	_, alerts, _ = eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 96))}, now)
	if countKind(alerts, domain.AlertQuotaThreshold) != 1 {
		t.Errorf("warning transition alerts = %v, want one quota_threshold", kinds(alerts))
	}
}

func TestRecoveredAlertOnImprovement(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()

	cases := []struct {
		name string
		rec  state.CredentialRecord
	}{
		{"warning", recWith(domain.StateWarning, state.QuotaWarning, state.CredValid, state.Fresh, true)},
		{"exhausted account", state.CredentialRecord{Quota: state.QuotaExhausted, Credential: state.CredValid, Freshness: state.Fresh,
			Scopes: map[string]state.ScopeRecord{scopeKey(domain.QuotaWindow{Scope: domain.ScopeAccount}): {Scope: domain.ScopeAccount, Level: state.QuotaExhausted}}}},
		{"invalid", recWith(domain.StateInvalid, state.QuotaUnknown, state.CredInvalid, state.Fresh, false)},
		{"suspect", recWith(domain.StateSuspect, state.QuotaUnknown, state.CredSuspect, state.Fresh, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := bootstrapped()
			prev.Credentials["k"] = tc.rec
			_, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 10))}, now)
			if countKind(alerts, domain.AlertRecovered) != 1 {
				t.Errorf("from %q: recovered alerts = %v, want one", tc.name, kinds(alerts))
			}
		})
	}
}

// TestRecoveryFromNotice: notice is a quota-dimension worsening, so returning
// to healthy must notify (the old engine suppressed this and could leave a
// recovered account looking permanently tight).
func TestRecoveryFromNotice(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateNotice, state.QuotaNotice, state.CredValid, state.Fresh, true)
	_, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 10))}, now)
	if countKind(alerts, domain.AlertRecovered) != 1 {
		t.Errorf("notice->healthy should notify recovery, got %v", kinds(alerts))
	}
}

// TestFreshnessRecoveryNotifies: a credential that went stale must announce
// that data is fresh again, and the recovery must not be duplicated per
// dimension in the same cycle.
func TestFreshnessRecoveryNotifies(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = state.CredentialRecord{
		State:         domain.StateStale,
		Quota:         state.QuotaWarning,
		Credential:    state.CredValid,
		Freshness:     state.Stale,
		ReachedNotice: true,
		LastUsedPercent: map[string]float64{
			"7d": 96,
		},
	}
	// Fresh reading at the same level: freshness improves, quota does not.
	_, alerts, next := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 96))}, now)
	if countKind(alerts, domain.AlertRecovered) != 1 {
		t.Fatalf("stale->fresh recovered alerts = %v, want exactly one", kinds(alerts))
	}
	al, _ := findAlert(alerts, domain.AlertRecovered)
	if !strings.Contains(al.Detail, "额度数据已恢复更新") {
		t.Errorf("recovery detail should mention the freshness dimension: %q", al.Detail)
	}
	if next.Credentials["k"].Freshness != state.Fresh {
		t.Errorf("freshness = %q, want fresh", next.Credentials["k"].Freshness)
	}
}

// ---------------------------------------------------------------------------
// Rule (c): quota reset detection
// ---------------------------------------------------------------------------

func TestQuotaResetOnSharpDrop(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateExhausted, state.QuotaExhausted, state.CredValid, state.Fresh, true)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 99})
	_, alerts, next := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 5))}, now)
	if countKind(alerts, domain.AlertQuotaReset) != 1 {
		t.Errorf("reset alerts = %v, want one", kinds(alerts))
	}
	if next.Credentials["k"].LastUsedPercent["7d"] != 5 {
		t.Errorf("last used percent not updated: %v", next.Credentials["k"].LastUsedPercent)
	}
	// The same cycle's quota recovery is folded into the reset notice.
	if countKind(alerts, domain.AlertRecovered) != 0 {
		t.Errorf("reset plus recovered duplicated: %v", kinds(alerts))
	}
}

func TestQuotaResetOnElapsedResetAt(t *testing.T) {
	eng := newEngine(t)
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateWarning, state.QuotaWarning, state.CredValid, state.Fresh, true)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 96})
	prev.Credentials["k"] = withReset(prev.Credentials["k"], map[string]time.Time{"7d": time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	// Usage barely changed, but the known reset instant has passed.
	s := okSnap("k", domain.ProviderCodex, win("7d", 90))
	_, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{s}, now)
	if countKind(alerts, domain.AlertQuotaReset) != 1 {
		t.Errorf("elapsed reset should alert, got %v", kinds(alerts))
	}
}

func TestResetSuppressedWhenNeverHigh(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateHealthy, state.QuotaHealthy, state.CredValid, state.Fresh, false)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 80})
	_, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 5))}, now)
	if countKind(alerts, domain.AlertQuotaReset) != 0 {
		t.Errorf("reset below notice should be suppressed, got %v", kinds(alerts))
	}
}

// TestResetUsesPersistedQuotaHistory: freshness going stale must not erase the
// fact that the account recently reached notice or worse. The reset reminder
// uses the persisted quota history, not the stale-overlaid projection.
func TestResetUsesPersistedQuotaHistory(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = state.CredentialRecord{
		State:         domain.StateStale, // projection masked by freshness
		Quota:         state.QuotaWarning,
		Credential:    state.CredValid,
		Freshness:     state.Stale,
		ReachedNotice: true,
		LastUsedPercent: map[string]float64{
			"7d": 96,
		},
		LastSuccessAt: now.Add(-3 * time.Hour),
	}
	_, alerts, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 5))}, now)
	if countKind(alerts, domain.AlertQuotaReset) != 1 {
		t.Errorf("stale projection masked the reset reminder: %v", kinds(alerts))
	}
}

// TestResetAtDoesNotRefireEveryCycle guards against an upstream that keeps
// reporting an already-past ResetAt: it must alert once and then stay quiet.
func TestResetAtDoesNotRefireEveryCycle(t *testing.T) {
	eng := newEngine(t)
	oldReset := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateWarning, state.QuotaWarning, state.CredValid, state.Fresh, true)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 96})
	prev.Credentials["k"] = withReset(prev.Credentials["k"], map[string]time.Time{"7d": oldReset})
	record := prev.Credentials["k"]
	record.LastSuccessAt = time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	prev.Credentials["k"] = record

	// First sighting after the reset: one alert.
	s := okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "7d", UsedPercent: fp(96), ResetAt: tp(oldReset)})
	_, alerts, next := eng.Evaluate(prev, []domain.QuotaSnapshot{s}, now)
	if countKind(alerts, domain.AlertQuotaReset) != 1 {
		t.Fatalf("first cycle reset alerts = %v, want one", kinds(alerts))
	}
	// Next cycle with the same stale timestamp: no repeat.
	now2 := now.Add(time.Hour)
	_, alerts, _ = eng.Evaluate(next, []domain.QuotaSnapshot{s}, now2)
	if countKind(alerts, domain.AlertQuotaReset) != 0 {
		t.Errorf("repeated stale ResetAt re-alerted: %v", kinds(alerts))
	}
}

// TestRelativeResetDriftDoesNotRefire models Ollama's relative "Resets in 3h"
// text, whose recomputed timestamp moves forward every scrape. Merely moving
// later must not count as a reset.
func TestRelativeResetDriftDoesNotRefire(t *testing.T) {
	eng := newEngine(t)
	base := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	first := okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "5h", UsedPercent: fp(40), ResetAt: tp(base.Add(3 * time.Hour))})
	_, alerts, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{first}, base)
	if countKind(alerts, domain.AlertQuotaReset) != 0 {
		t.Fatalf("first relative reset should not alert: %v", kinds(alerts))
	}

	// Next cycle, five minutes later, the rounded "in 3h" now points at a new,
	// slightly later instant. Usage is unchanged.
	now2 := base.Add(5 * time.Minute)
	second := okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "5h", UsedPercent: fp(40), ResetAt: tp(now2.Add(3 * time.Hour))})
	_, alerts, _ = eng.Evaluate(next, []domain.QuotaSnapshot{second}, now2)
	if countKind(alerts, domain.AlertQuotaReset) != 0 {
		t.Errorf("drifted relative reset re-alerted: %v", kinds(alerts))
	}
}

// ---------------------------------------------------------------------------
// Rule (d): stale data
// ---------------------------------------------------------------------------

func TestStaleAfterThreshold(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()

	// One failure: below the 2-failure bar, no stale alert yet.
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateHealthy, state.QuotaHealthy, state.CredValid, state.Fresh, false)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 42})
	{
		record := prev.Credentials["k"]
		record.LastSuccessAt = now.Add(-time.Hour)
		prev.Credentials["k"] = record
	}

	_, alerts, next := eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "timeout")}, now)
	if countKind(alerts, domain.AlertStale) != 0 {
		t.Errorf("single failure must not be stale yet: %v", kinds(alerts))
	}
	if next.Credentials["k"].ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", next.Credentials["k"].ConsecutiveFailures)
	}

	// Second failure: stale alert with last-success facts, percent preserved.
	prev = next
	_, alerts, next = eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "connection refused")}, now)
	if countKind(alerts, domain.AlertStale) != 1 {
		t.Fatalf("two failures should alert stale, got %v", kinds(alerts))
	}
	if got := next.Credentials["k"].State; got != domain.StateStale {
		t.Errorf("state = %q, want stale", got)
	}
	al, _ := findAlert(alerts, domain.AlertStale)
	joined := strings.Join(al.Facts, " | ")
	if !strings.Contains(joined, "最后成功") {
		t.Errorf("stale facts missing last-success time: %v", al.Facts)
	}
	if !strings.Contains(joined, "connection refused") {
		t.Errorf("stale facts missing failure reason: %v", al.Facts)
	}
}

// TestStaleSnapshotFlaggedAndNotFresh is fix #5: a failed snapshot that keeps
// last-known windows must be marked Stale with the real last-success time, and
// the report must be marked degraded with a reader-facing note.
func TestStaleSnapshotFlaggedAndNotFresh(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	lastSuccess := now.Add(-2 * time.Hour)
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateWarning, state.QuotaWarning, state.CredValid, state.Fresh, true)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 96})
	{
		record := prev.Credentials["k"]
		record.LastSuccessAt = lastSuccess
		prev.Credentials["k"] = record
	}

	report, _, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "i/o timeout")}, now)

	if !report.Degraded {
		t.Error("report should be degraded when a stale snapshot is shown")
	}
	if len(report.Notes) == 0 || !strings.Contains(strings.Join(report.Notes, " "), "过期") {
		t.Errorf("report notes should explain the staleness, got %v", report.Notes)
	}
	var found bool
	for _, pr := range report.Providers {
		for _, s := range pr.Snapshots {
			if s.OK {
				continue
			}
			if !s.Stale {
				t.Error("preserved snapshot not marked Stale")
			}
			if !s.LastSuccessAt.Equal(lastSuccess) {
				t.Errorf("LastSuccessAt = %v, want %v", s.LastSuccessAt, lastSuccess)
			}
			if s.LastSuccessAt.IsZero() {
				t.Error("LastSuccessAt must be set on a stale snapshot")
			}
			for _, w := range s.Windows {
				found = true
				if w.UsedPercent == nil || *w.UsedPercent != 96 {
					t.Errorf("stale window lost its last known percent: %+v", w)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected a stale snapshot with preserved windows in the report")
	}
}

// TestStaleDoesNotDriveRecommendation: stale numbers must never produce
// "use more" capacity advice.
func TestStaleDoesNotDriveRecommendation(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateHealthy, state.QuotaHealthy, state.CredValid, state.Fresh, false)
	prev.Credentials["k"] = withUsage(prev.Credentials["k"], map[string]float64{"7d": 10})
	{
		record := prev.Credentials["k"]
		record.LastSuccessAt = now.Add(-24 * time.Hour)
		prev.Credentials["k"] = record
	}

	report, _, _ := eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "timeout")}, now)
	if len(report.Providers) != 1 {
		t.Fatal("expected one provider")
	}
	if len(report.Providers[0].BestWindows) != 0 {
		t.Errorf("stale snapshot contributed BestWindows: %+v", report.Providers[0].BestWindows)
	}
	if report.Recommendations[0].Direction == domain.DirectionUseMore {
		t.Errorf("stale data produced use_more: %q", report.Recommendations[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// Rule (e): structured, evidence-graded attribution
// ---------------------------------------------------------------------------

func TestStructuredAttributionTable(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()

	cases := []struct {
		name         string
		failure      domain.FailureKind
		errText      string
		provider     domain.ProviderKind
		wantState    domain.CredentialState
		wantKind     domain.AlertKind
		wantEvidence domain.EvidenceLevel
		wantAdvice   string
	}{
		{
			name: "auth", failure: domain.FailureAuth, errText: "HTTP 401 Unauthorized",
			provider: domain.ProviderCodex, wantState: domain.StateInvalid,
			wantKind: domain.AlertCredential, wantEvidence: domain.EvidenceConfirmed,
			wantAdvice: adviceOAuth,
		},
		{
			name: "auth ollama", failure: domain.FailureAuth, errText: "session expired",
			provider: domain.ProviderOllama, wantState: domain.StateInvalid,
			wantKind: domain.AlertCredential, wantEvidence: domain.EvidenceConfirmed,
			wantAdvice: adviceOllama,
		},
		{
			name: "quota", failure: domain.FailureQuota, errText: "quota_exceeded for plan",
			provider: domain.ProviderCodex, wantState: domain.StateExhausted,
			wantKind: domain.AlertQuotaExhausted, wantEvidence: domain.EvidenceConfirmed,
			wantAdvice: adviceExhaust,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, alerts, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{failSnapKind("k", tc.provider, tc.errText, tc.failure)}, now)
			if got := next.Credentials["k"].State; got != tc.wantState {
				t.Errorf("state = %q, want %q", got, tc.wantState)
			}
			al, ok := findAlert(alerts, tc.wantKind)
			if !ok {
				t.Fatalf("missing %q alert, got %v", tc.wantKind, kinds(alerts))
			}
			if al.Evidence != tc.wantEvidence {
				t.Errorf("evidence = %q, want %q", al.Evidence, tc.wantEvidence)
			}
			if al.Advice != tc.wantAdvice {
				t.Errorf("advice = %q, want %q", al.Advice, tc.wantAdvice)
			}
		})
	}
}

// TestGatewayQuotaWordIsNotQuotaEvidence is the core fix #1 regression: a 502
// gateway body that happens to contain "quota" is a transport failure, not an
// exhausted quota.
func TestGatewayQuotaWordIsNotQuotaEvidence(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	s := failSnapKind("k", domain.ProviderCodex, "failed to fetch quota: 502 Bad Gateway (upstream timed out)", domain.FailureTransport)
	_, alerts, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	rec := next.Credentials["k"]
	if rec.Quota == state.QuotaExhausted {
		t.Error("a transport failure was read as an exhausted quota")
	}
	if rec.Credential == state.CredInvalid {
		t.Error("a transport failure was read as an invalid credential")
	}
	if countKind(alerts, domain.AlertQuotaExhausted) != 0 || countKind(alerts, domain.AlertCredential) != 0 {
		t.Errorf("transport failure produced a credential verdict: %v", kinds(alerts))
	}
}

// TestControlPlaneNotAttributedToCredential: a control-plane failure must not
// touch the credential dimension; the provider-level Error carries it instead.
func TestControlPlaneNotAttributedToCredential(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	s := failSnapKind("k", domain.ProviderCodex, "管理 API 鉴权失败（HTTP 401）", domain.FailureControlPlane)
	report, alerts, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	if got := next.Credentials["k"].Credential; got != state.CredValid {
		t.Errorf("control-plane failure changed credential dimension to %q", got)
	}
	if countKind(alerts, domain.AlertCredential) != 0 {
		t.Errorf("control-plane failure raised a credential alert: %v", kinds(alerts))
	}
	if report.Providers[0].Error == "" {
		t.Error("provider Error not populated for a control-plane failure")
	}
}

// TestTimeoutNeverBlamedOnCredential is the explicit guard: a plain timeout
// must not become invalid/exhausted, and on the first failures must not even be
// suspect.
func TestTimeoutNeverBlamedOnCredential(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()

	for i := 1; i <= 2; i++ {
		_, alerts, next := eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "context deadline exceeded (Client.Timeout)")}, now)
		if countKind(alerts, domain.AlertCredential) != 0 {
			t.Fatalf("timeout attributed to credential expiry: %v", kinds(alerts))
		}
		if countKind(alerts, domain.AlertQuotaExhausted) != 0 {
			t.Fatalf("timeout attributed to quota exhaustion: %v", kinds(alerts))
		}
		if got := next.Credentials["k"].State; got == domain.StateInvalid || got == domain.StateExhausted {
			t.Fatalf("iteration %d state = %q, timeout must not be confirmed bad", i, got)
		}
		prev = next
	}

	// Third consecutive failure: credential suspect and freshness stale, but
	// only one alert (the more severe suspect), per fix #3.
	_, alerts, next := eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "context deadline exceeded")}, now)
	rec := next.Credentials["k"]
	if rec.Credential != state.CredSuspect {
		t.Fatalf("third timeout credential = %q, want suspect", rec.Credential)
	}
	if rec.Freshness != state.Stale {
		t.Errorf("third timeout freshness = %q, want stale", rec.Freshness)
	}
	if got := rec.State; got != domain.StateSuspect {
		t.Errorf("projection = %q, want suspect", got)
	}
	if countKind(alerts, domain.AlertSuspect) != 1 {
		t.Fatalf("suspect alerts = %v, want one", kinds(alerts))
	}
	if countKind(alerts, domain.AlertStale) != 0 {
		t.Errorf("stale and suspect alerted in the same round: %v", kinds(alerts))
	}
	al, _ := findAlert(alerts, domain.AlertSuspect)
	if al.Evidence != domain.EvidenceSuspected {
		t.Errorf("evidence = %q, want suspected", al.Evidence)
	}
	if al.Advice != adviceSuspect {
		t.Errorf("advice = %q, want %q", al.Advice, adviceSuspect)
	}
	if !strings.Contains(al.Detail, "网络") || !strings.Contains(al.Detail, "超时") {
		t.Errorf("suspect detail should list candidate causes, got %q", al.Detail)
	}
}

// TestConfirmedInvalidSurvivesTimeouts is fix #2: a confirmed auth failure must
// not be downgraded to stale/suspect by later ordinary timeouts.
func TestConfirmedInvalidSurvivesTimeouts(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = recWith(domain.StateInvalid, state.QuotaUnknown, state.CredInvalid, state.Fresh, false)

	var alerts []domain.Alert
	for i := 0; i < 3; i++ {
		_, alerts, prev = eng.Evaluate(prev, []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "context deadline exceeded")}, now)
		if got := prev.Credentials["k"].Credential; got != state.CredInvalid {
			t.Fatalf("iteration %d credential = %q, invalid was overwritten by timeouts", i, got)
		}
		if got := prev.Credentials["k"].State; got != domain.StateInvalid {
			t.Fatalf("iteration %d projection = %q, want invalid", i, got)
		}
	}
	if countKind(alerts, domain.AlertStale) != 0 || countKind(alerts, domain.AlertSuspect) != 0 {
		t.Errorf("timeouts alerted stale/suspect over a confirmed invalid: %v", kinds(alerts))
	}
}

// TestSuspectFromRequestSamples covers the optional sample channel: a single
// failure with a high in-window failure rate is already suspect.
func TestSuspectFromRequestSamples(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	samples := map[string]RequestStats{"k": {Window: 5 * time.Minute, Total: 10, Failed: 5}}
	_, alerts, next := eng.EvaluateWithSamples(bootstrapped(), []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "i/o timeout")}, samples, now)
	if got := next.Credentials["k"].Credential; got != state.CredSuspect {
		t.Errorf("credential = %q, want suspect from samples", got)
	}
	if countKind(alerts, domain.AlertSuspect) != 1 {
		t.Errorf("sample-based suspect alerts = %v", kinds(alerts))
	}

	// Too few requests: the rate is ignored.
	samples["k"] = RequestStats{Window: 5 * time.Minute, Total: 3, Failed: 3}
	_, alerts, next = eng.EvaluateWithSamples(bootstrapped(), []domain.QuotaSnapshot{failSnap("k", domain.ProviderCodex, "i/o timeout")}, samples, now)
	if got := next.Credentials["k"].Credential; got == state.CredSuspect {
		t.Errorf("credential = %q, small sample must not trigger suspect", got)
	}
	if countKind(alerts, domain.AlertSuspect) != 0 {
		t.Errorf("small sample alerted suspect: %v", kinds(alerts))
	}
}

// TestStatusFieldAuthEvidence verifies credential status, not just Err, can
// carry the auth verdict when no structured Failure is set.
func TestStatusFieldAuthEvidence(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	s := failSnapKind("k", domain.ProviderCodex, "", domain.FailureNone)
	s.Credential.Status = "unauthorized"
	_, alerts, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	if got := next.Credentials["k"].State; got != domain.StateInvalid {
		t.Errorf("state = %q, want invalid", got)
	}
	if countKind(alerts, domain.AlertCredential) != 1 {
		t.Errorf("alerts = %v, want credential_invalid", kinds(alerts))
	}
}

// ---------------------------------------------------------------------------
// Rule (f): first-boot summary
// ---------------------------------------------------------------------------

func TestBootstrapOnlyOneSummary(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := state.Empty() // Bootstrapped=false

	snaps := []domain.QuotaSnapshot{
		failSnapKind("bad", domain.ProviderCodex, "401 unauthorized", domain.FailureAuth),
		okSnap("tight", domain.ProviderClaude, win("7d", 97)),
		okSnap("good", domain.ProviderCodex, win("7d", 10)),
	}
	_, alerts, next := eng.Evaluate(prev, snaps, now)

	if countKind(alerts, domain.AlertBootstrap) != 1 {
		t.Fatalf("bootstrap alerts = %v, want exactly one", kinds(alerts))
	}
	if len(alerts) != 1 {
		t.Fatalf("bootstrap must suppress per-credential alerts, got %v", kinds(alerts))
	}
	al, _ := findAlert(alerts, domain.AlertBootstrap)
	if !strings.Contains(al.Detail, "1 个失效") || !strings.Contains(al.Detail, "1 个额度告急") || !strings.Contains(al.Detail, "1 个正常") {
		t.Errorf("bootstrap detail = %q, want counts", al.Detail)
	}
	if !next.Bootstrapped {
		t.Error("Bootstrapped must be set to true after first cycle")
	}
}

func TestSecondCycleIsNotBootstrap(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	_, _, first := eng.Evaluate(state.Empty(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 97))}, now)
	_, alerts, _ := eng.Evaluate(first, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 97))}, now)
	if countKind(alerts, domain.AlertBootstrap) != 0 {
		t.Errorf("second cycle re-bootstrapped: %v", kinds(alerts))
	}
}

// ---------------------------------------------------------------------------
// Rule (g): provider aggregation without cross-account averaging
// ---------------------------------------------------------------------------

// TestBestWindowsSingleCredential is fix #4: windows from different accounts
// must never be merged. A(5h=10,7d=96) and B(5h=96,7d=10) must not produce
// 10%/10%.
func TestBestWindowsSingleCredential(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	snaps := []domain.QuotaSnapshot{
		okSnap("a", domain.ProviderCodex, win("5h", 10), win("7d", 96)),
		okSnap("b", domain.ProviderCodex, win("5h", 96), win("7d", 10)),
	}
	report, _, _ := eng.Evaluate(bootstrapped(), snaps, now)
	pr := report.Providers[0]
	if len(pr.BestWindows) == 0 {
		t.Fatal("expected a best credential's windows")
	}
	// The chosen set must belong to exactly one account: either {10,96} or {96,10}.
	got := map[string]float64{}
	for _, w := range pr.BestWindows {
		got[w.Name] = *w.UsedPercent
	}
	// Account B is safer (worst window 96 vs account A's 96 -- tie). Both are
	// warning; the important property is that no cross-account 10/10 appears.
	if got["5h"] == 10 && got["7d"] == 10 {
		t.Fatalf("BestWindows invented a cross-account combination: %v", got)
	}
	if got["5h"] != got["7d"] && got["5h"]+got["7d"] != 106 {
		t.Errorf("BestWindows not from a single credential: %v", got)
	}
	if report.Recommendations[0].Direction == domain.DirectionUseMore {
		t.Errorf("two fully-flagged accounts produced use_more: %q", report.Recommendations[0].Reason)
	}
}

func TestProviderAggregation(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	snaps := []domain.QuotaSnapshot{
		okSnap("a", domain.ProviderCodex, win("5h", 80), win("7d", 40)),
		okSnap("b", domain.ProviderCodex, win("5h", 20), win("7d", 60)),
	}
	report, _, _ := eng.Evaluate(bootstrapped(), snaps, now)
	if len(report.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(report.Providers))
	}
	pr := report.Providers[0]
	if pr.Total != 2 {
		t.Errorf("Total = %d, want 2", pr.Total)
	}
	if pr.Healthy != 2 {
		t.Errorf("Healthy = %d, want 2", pr.Healthy)
	}
	// Best windows belong to the safest credential: b (worst 60 < a's 80).
	best := map[string]float64{}
	for _, w := range pr.BestWindows {
		if w.UsedPercent == nil {
			t.Fatalf("best window %q has nil percent", w.Name)
		}
		best[w.Name] = *w.UsedPercent
	}
	if best["5h"] != 20 || best["7d"] != 60 {
		t.Errorf("BestWindows = %v, want the b credential 5h=20 7d=60", best)
	}
}

func TestProviderWorstState(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	snaps := []domain.QuotaSnapshot{
		okSnap("a", domain.ProviderCodex, win("7d", 10)),
		okSnap("b", domain.ProviderCodex, win("7d", 97)),
	}
	report, _, _ := eng.Evaluate(bootstrapped(), snaps, now)
	if got := report.Providers[0].WorstState; got != domain.StateWarning {
		t.Errorf("WorstState = %q, want warning", got)
	}
	if report.Providers[0].Healthy != 1 {
		t.Errorf("Healthy = %d, want 1", report.Providers[0].Healthy)
	}
}

// ---------------------------------------------------------------------------
// Rule (h): provider-level recommendations
// ---------------------------------------------------------------------------

func TestRecommendReauth(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	snaps := []domain.QuotaSnapshot{
		failSnapKind("bad", domain.ProviderCodex, "401 unauthorized", domain.FailureAuth),
		okSnap("ok", domain.ProviderCodex, win("7d", 10)),
	}
	report, _, _ := eng.Evaluate(bootstrapped(), snaps, now)
	rec := report.Recommendations[0]
	if rec.Direction != domain.DirectionReauth {
		t.Errorf("direction = %q, want reauth", rec.Direction)
	}
	if !strings.Contains(rec.Reason, "OAuth") {
		t.Errorf("reauth reason = %q", rec.Reason)
	}
}

func TestRecommendUseMoreWhenIdle(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	report, _, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 32))}, now)
	rec := report.Recommendations[0]
	if rec.Direction != domain.DirectionUseMore {
		t.Errorf("direction = %q, want use_more", rec.Direction)
	}
	if !strings.Contains(rec.Reason, "余量充足") || !strings.Contains(rec.Reason, "32%") {
		t.Errorf("reason = %q, want specific percent and margin note", rec.Reason)
	}
}

func TestRecommendEaseOffAtNotice(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	report, _, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 92))}, now)
	if got := report.Recommendations[0].Direction; got != domain.DirectionEaseOff {
		t.Errorf("direction = %q, want ease_off", got)
	}
}

// TestHolidayTiltsTowardUseMore: a long holiday with a window resetting before
// work resumes should recommend using the idle quota.
func TestHolidayTiltsTowardUseMore(t *testing.T) {
	eng := newEngine(t)
	// 2026-02-16 is inside the 春节 break; next workday is 8 days out.
	now := time.Date(2026, 2, 16, 10, 0, 0, 0, shanghai(t))
	reset := time.Date(2026, 2, 20, 0, 0, 0, 0, shanghai(t)) // before return
	s := okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "7d", UsedPercent: fp(60), ResetAt: tp(reset), Scope: domain.ScopeAccount})
	report, _, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)

	if report.Holiday.HolidayRunLength < 2 {
		t.Fatalf("holiday run length = %d, expected a long break", report.Holiday.HolidayRunLength)
	}
	rec := report.Recommendations[0]
	if rec.Direction != domain.DirectionUseMore {
		t.Errorf("direction = %q, want use_more during holiday", rec.Direction)
	}
	if !strings.Contains(rec.Reason, "复工前刷新") {
		t.Errorf("reason = %q, want reset-before-return explanation", rec.Reason)
	}
}

// TestHolidayNeverLoosensThresholds: even mid-holiday, a tight window stays
// ease_off. The holiday only affects advice, not alerting.
func TestHolidayNeverLoosensThresholds(t *testing.T) {
	eng := newEngine(t)
	now := time.Date(2026, 2, 16, 10, 0, 0, 0, shanghai(t))
	reset := time.Date(2026, 2, 20, 0, 0, 0, 0, shanghai(t))
	s := okSnap("k", domain.ProviderCodex, domain.QuotaWindow{Name: "7d", UsedPercent: fp(96), ResetAt: tp(reset), Scope: domain.ScopeAccount})
	report, alerts, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)

	if next.Credentials["k"].State != domain.StateWarning {
		t.Errorf("state = %q, holiday must not change thresholds", next.Credentials["k"].State)
	}
	if countKind(alerts, domain.AlertQuotaThreshold) != 1 {
		t.Errorf("holiday suppressed a threshold alert: %v", kinds(alerts))
	}
	if got := report.Recommendations[0].Direction; got != domain.DirectionEaseOff {
		t.Errorf("direction = %q, want ease_off despite holiday", got)
	}
}

// TestRecommendReasonHasNumbers checks the requirement that reasons are specific
// and not generic.
func TestRecommendReasonHasNumbers(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	report, _, _ := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 21))}, now)
	if !strings.Contains(report.Recommendations[0].Reason, "21%") {
		t.Errorf("reason = %q, want the actual percentage", report.Recommendations[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// Purity and safety
// ---------------------------------------------------------------------------

// TestDoesNotMutatePreviousState confirms the engine is pure with respect to
// its input state.
func TestDoesNotMutatePreviousState(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	prev := bootstrapped()
	prev.Credentials["k"] = state.CredentialRecord{
		State:           domain.StateHealthy,
		LastUsedPercent: map[string]float64{"7d": 10},
	}
	eng.Evaluate(prev, []domain.QuotaSnapshot{okSnap("k", domain.ProviderCodex, win("7d", 50))}, now)
	if prev.Credentials["k"].LastUsedPercent["7d"] != 10 || prev.Credentials["k"].State != domain.StateHealthy {
		t.Error("Evaluate mutated the previous state")
	}
}

// TestAnonymousCredentialSkipped ensures a snapshot without a Key cannot corrupt
// persistence by collapsing onto an empty key.
func TestAnonymousCredentialSkipped(t *testing.T) {
	eng := newEngine(t)
	now := time.Now()
	s := okSnap("", domain.ProviderCodex, win("7d", 10))
	s.Credential.Key = ""
	_, _, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	if len(next.Credentials) != 0 {
		t.Errorf("anonymous credential was persisted: %v", next.Credentials)
	}
}

// TestEmptySnapshotListIsSafe covers a collector that returned nothing.
func TestEmptySnapshotListIsSafe(t *testing.T) {
	eng := newEngine(t)
	report, alerts, next := eng.Evaluate(bootstrapped(), nil, time.Now())
	if len(report.Providers) != 0 || len(alerts) != 0 {
		t.Errorf("empty input produced providers=%d alerts=%d", len(report.Providers), len(alerts))
	}
	if !next.Bootstrapped {
		t.Error("Bootstrapped should be set even for an empty cycle")
	}
}

// TestPendingOutboxPreservedAcrossCycle guards that evaluation does not drop a
// caller's undelivered notification queue.
func TestPendingOutboxPreservedAcrossCycle(t *testing.T) {
	eng := newEngine(t)
	prev := bootstrapped()
	prev.Pending = map[string][]domain.Alert{"feishu": {{Title: "queued"}}}
	_, _, next := eng.Evaluate(prev, nil, time.Now())
	if got := next.PendingFor("feishu"); len(got) != 1 || got[0].Title != "queued" {
		t.Errorf("pending queue lost: %+v", got)
	}
}

func withUsage(rec state.CredentialRecord, usage map[string]float64) state.CredentialRecord {
	rec.LastUsedPercent = usage
	return rec
}

func withReset(rec state.CredentialRecord, resets map[string]time.Time) state.CredentialRecord {
	rec.LastResetAt = resets
	return rec
}

// TestReportKeepsCollectorObservationTime: the OMC source stamps the real
// observation instant; evaluation must not overwrite it with the cycle clock in
// the report, even though the persisted record advances to the cycle time.
func TestReportKeepsCollectorObservationTime(t *testing.T) {
	eng := newEngine(t)
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	observed := now.Add(-90 * time.Minute)
	s := okSnap("k", domain.ProviderCodex, win("5h", 10))
	s.LastSuccessAt = observed
	report, _, next := eng.Evaluate(bootstrapped(), []domain.QuotaSnapshot{s}, now)
	if got := report.Providers[0].Snapshots[0].LastSuccessAt; !got.Equal(observed) {
		t.Fatalf("report LastSuccessAt = %v, want the observation time %v", got, observed)
	}
	if got := next.Credentials["k"].LastSuccessAt; !got.Equal(now) {
		t.Fatalf("persisted LastSuccessAt = %v, want the cycle time %v", got, now)
	}
}
