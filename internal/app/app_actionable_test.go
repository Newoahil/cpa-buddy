package app

import (
	"context"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

func actionableConfig() config.Config {
	return config.Config{
		PollInterval:            time.Minute,
		AlertMode:               config.AlertModeActionable,
		ChannelUnavailableAfter: 30 * time.Minute,
		MonitorDownAfter:        30 * time.Minute,
		OMCOutagePendingAfter:   2 * time.Hour,
		SummaryTightRemaining:   20,
		QuietHours:              config.QuietWindow{Start: 23 * 60, End: 8 * 60, Set: true},
		Location:                time.UTC,
	}
}

func newActionableApp(store state.Store, now time.Time, collectors []domain.Collector) *App {
	cfg := actionableConfig()
	a := New(cfg, collectors, evaluate.New(cfg, nil), store, nil)
	a.now = func() time.Time { return now }
	return a
}

func accountSnap(key string, used float64, reset *time.Time) domain.QuotaSnapshot {
	c := domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: key, ShortID: "s1"}
	return domain.QuotaSnapshot{
		Credential:    c,
		Windows:       []domain.QuotaWindow{{Scope: domain.ScopeAccount, Name: "5h", UsedPercent: &used, ResetAt: reset}},
		Source:        domain.SourceCPA,
		Confidence:    domain.ConfidenceReported,
		FetchedAt:     time.Now(),
		LastSuccessAt: time.Now(),
		OK:            true,
	}
}

func hasAlertKind(alerts []domain.Alert, k domain.AlertKind) bool {
	for _, a := range alerts {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func openItems(st *state.State) []state.PendingItem {
	var out []state.PendingItem
	for _, it := range st.PendingItems {
		if it.Status == state.PendingOpen {
			out = append(out, it)
		}
	}
	return out
}

func TestQuietHoursStashThenDeliverOnEnd(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 14, 23, 30, 0, 0, time.UTC)
	// Far enough that the channel is still unavailable well after quiet ends.
	reset := now.Add(20 * time.Hour)
	a := newActionableApp(store, now, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 100, &reset)}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})

	if _, ok := a.runCycle(context.Background()); !ok {
		t.Fatal("cycle failed")
	}
	// Quiet: only the one-off bootstrap confirmation goes out.
	if n.calls.Load() != 1 {
		t.Fatalf("quiet cycle calls = %d, want 1 (bootstrap only)", n.calls.Load())
	}
	if !hasAlertKind(n.lastMessage().Alerts, domain.AlertBootstrap) {
		t.Fatalf("expected bootstrap, got %v", n.lastMessage().Alerts)
	}
	if len(store.st.QuietQueue) == 0 {
		t.Fatal("quiet queue should hold the channel_unavailable event")
	}
	items := openItems(store.st)
	if len(items) != 1 || !items[0].NotifiedAt.IsZero() {
		t.Fatalf("item should be open and un-notified: %+v", items)
	}

	// Quiet ends: the still-unresolved event is delivered merged.
	a.now = func() time.Time { return time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC) }
	if _, ok := a.runCycle(context.Background()); !ok {
		t.Fatal("cycle failed")
	}
	last := n.lastMessage()
	if last.Kind != string(domain.AlertQuietDigest) {
		t.Fatalf("last kind = %q, want quiet_digest", last.Kind)
	}
	if !hasAlertKind(last.Alerts, domain.AlertChannelUnavailable) {
		t.Fatalf("merged message missing channel_unavailable: %+v", last.Alerts)
	}
	if len(store.st.QuietQueue) != 0 {
		t.Fatalf("quiet queue should be cleared: %+v", store.st.QuietQueue)
	}
	if n.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", n.calls.Load())
	}
}

func TestQuietAllRecoveredSendsNothing(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 14, 23, 30, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	down := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 100, &reset)}}
	a := newActionableApp(store, now, []domain.Collector{down})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.runCycle(context.Background())
	if len(store.st.QuietQueue) == 0 {
		t.Fatal("setup: nothing queued")
	}

	// Quiet ends, but the channel recovered on its own.
	a.now = func() time.Time { return time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC) }
	down.snaps = []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}
	a.runCycle(context.Background())

	if n.calls.Load() != 1 {
		t.Fatalf("recovered events must not be sent: calls = %d, want 1 (bootstrap only)", n.calls.Load())
	}
	if len(store.st.QuietQueue) != 0 {
		t.Fatalf("queue should be cleared even when nothing is sent: %+v", store.st.QuietQueue)
	}
	if len(openItems(store.st)) != 0 {
		t.Fatal("resolved item should be closed")
	}
}

func TestRestartDoesNotRepush(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	snaps := []domain.QuotaSnapshot{accountSnap("codex-1", 100, &reset)}

	first := newActionableApp(store, now, []domain.Collector{&fakeCollector{name: "cpa", snaps: snaps}})
	n1 := &fakeNotifier{name: "feishu"}
	first.SetNotifiers([]domain.Notifier{n1})
	first.runCycle(context.Background())
	if n1.calls.Load() != 1 {
		t.Fatalf("first push calls = %d, want 1", n1.calls.Load())
	}

	// A restart, same persisted state and same condition: no re-push.
	second := newActionableApp(store, now.Add(time.Minute), []domain.Collector{&fakeCollector{name: "cpa", snaps: snaps}})
	n2 := &fakeNotifier{name: "feishu"}
	second.SetNotifiers([]domain.Notifier{n2})
	second.runCycle(context.Background())
	if n2.calls.Load() != 0 {
		t.Fatalf("restart re-pushed: calls = %d, want 0", n2.calls.Load())
	}
}

func TestResolvedPendingPrunedAfterRetention(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	down := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 100, &reset)}}
	a := newActionableApp(store, now, []domain.Collector{down})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.runCycle(context.Background())
	if len(openItems(store.st)) != 1 {
		t.Fatalf("setup: item not opened: %+v", store.st.PendingItems)
	}

	// Recovered: the item is resolved but kept.
	a.now = func() time.Time { return now.Add(time.Minute) }
	down.snaps = []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}
	a.runCycle(context.Background())
	if len(store.st.PendingItems) != 1 || store.st.PendingItems[0].Status != state.PendingResolved {
		t.Fatalf("resolved item not kept: %+v", store.st.PendingItems)
	}

	// Past the 7-day retention: pruned.
	a.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	a.runCycle(context.Background())
	if len(store.st.PendingItems) != 0 {
		t.Fatalf("resolved item not pruned: %+v", store.st.PendingItems)
	}
}

func TestActionableDropsThresholdOnly(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true // skip the one-off bootstrap alert
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := newActionableApp(store, now, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 92, nil)}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.runCycle(context.Background())
	if n.calls.Load() != 0 {
		t.Fatalf("threshold-only event pushed: calls = %d", n.calls.Load())
	}
	if len(openItems(store.st)) != 0 {
		t.Fatal("threshold-only event must not open a pending item")
	}
}

func TestActionableSummarySkipsWhenIdle(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true // skip the one-off bootstrap alert
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := newActionableApp(store, now, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.SendSummary(context.Background())
	if n.calls.Load() != 0 {
		t.Fatalf("idle summary sent: calls = %d", n.calls.Load())
	}
}

func TestActionableSummarySendsWithPending(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	a := newActionableApp(store, now, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 100, &reset)}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.SendSummary(context.Background())
	last := n.lastMessage()
	if last.Kind != string(domain.AlertActionableSummary) || last.Actionable == nil {
		t.Fatalf("summary not sent: %+v", last)
	}
	if len(last.Actionable.Pending) != 1 {
		t.Fatalf("pending rows = %+v", last.Actionable.Pending)
	}
	if last.Report == nil || len(last.Report.RateLimits) != 0 {
		t.Fatal("actionable summary must not carry rate-limit tallies")
	}
}

func TestActionableSummarySendsWhenTight(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := newActionableApp(store, now, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 85, nil)}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.SendSummary(context.Background())
	last := n.lastMessage()
	if last.Actionable == nil || len(last.Actionable.Tight) != 1 {
		t.Fatalf("tight summary = %+v", last)
	}
	if last.Actionable.HeaderColor != "orange" {
		t.Errorf("header color = %q, want orange", last.Actionable.HeaderColor)
	}
}

func TestActionableSummarySkippedDuringQuiet(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 14, 23, 30, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	a := newActionableApp(store, now, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 100, &reset)}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.SendSummary(context.Background())
	if n.calls.Load() != 1 || !hasAlertKind(n.lastMessage().Alerts, domain.AlertBootstrap) {
		t.Fatalf("quiet summary should only emit bootstrap, got calls=%d msg=%+v", n.calls.Load(), n.lastMessage())
	}
}
