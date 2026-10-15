package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// stubOutage is a controllable OMC outage source for app tests. It implements
// both policy.OutageSource and the app's outageTracker.
type stubOutage struct {
	active bool
	since  time.Time
}

func (s *stubOutage) OutageSince(domain.ProviderKind, time.Time) (time.Time, bool) {
	if !s.active {
		return time.Time{}, false
	}
	return s.since, true
}

func (s *stubOutage) OutageStart() time.Time {
	if !s.active {
		return time.Time{}
	}
	return s.since
}

func (s *stubOutage) RestoreOutage(t time.Time) {
	if !s.active || t.IsZero() {
		return
	}
	if s.since.IsZero() || t.Before(s.since) {
		s.since = t
	}
}

func outageTestTime() time.Time {
	return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
}

// TestMonitorDownWholeCollectorFailure checks that when an entire collector
// fails and yields no snapshots, the channel is still timed from its last valid
// reading and pushes monitor_down once past the threshold, without repeating.
func TestMonitorDownWholeCollectorFailure(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	c := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}}
	a := newActionableApp(store, base, []domain.Collector{c})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})

	a.runCycle(context.Background())
	if n.calls.Load() != 0 {
		t.Fatalf("healthy cycle pushed: calls=%d", n.calls.Load())
	}

	// The whole collector now fails: no snapshots at all.
	c.snaps = nil
	c.err = errors.New("cpa unreachable")
	a.now = func() time.Time { return base.Add(40 * time.Minute) }
	a.runCycle(context.Background())
	if n.calls.Load() != 1 {
		t.Fatalf("monitor_down not pushed: calls=%d", n.calls.Load())
	}
	items := openItems(store.st)
	if len(items) != 1 || items[0].Kind != string(domain.AlertMonitorDown) {
		t.Fatalf("monitor_down not opened: %+v", items)
	}

	// Still failing: no repeat.
	a.now = func() time.Time { return base.Add(41 * time.Minute) }
	a.runCycle(context.Background())
	if n.calls.Load() != 1 {
		t.Fatalf("monitor_down repeated: calls=%d", n.calls.Load())
	}
}

func TestMonitorDownWholeCollectorFailureBelowThreshold(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	c := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}}
	a := newActionableApp(store, base, []domain.Collector{c})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})
	a.runCycle(context.Background())

	c.snaps = nil
	c.err = errors.New("cpa unreachable")
	a.now = func() time.Time { return base.Add(20 * time.Minute) }
	a.runCycle(context.Background())
	if n.calls.Load() != 0 {
		t.Fatalf("below-threshold failure pushed: calls=%d", n.calls.Load())
	}
	if len(openItems(store.st)) != 0 {
		t.Fatalf("below-threshold failure opened an item: %+v", openItems(store.st))
	}
}

// TestMonitorDownWholeCollectorFailureRestartNoRepush is the durability check:
// the last-valid memory is persisted, so a restart during the outage neither
// forgets it nor re-pushes.
func TestMonitorDownWholeCollectorFailureRestartNoRepush(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	c := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}}
	first := newActionableApp(store, base, []domain.Collector{c})
	first.SetNotifiers([]domain.Notifier{&fakeNotifier{name: "feishu"}})
	first.runCycle(context.Background())

	c.snaps = nil
	c.err = errors.New("cpa unreachable")
	first.now = func() time.Time { return base.Add(40 * time.Minute) }
	first.runCycle(context.Background())

	// Restart: a fresh app with the same persisted state and a still-failing
	// collector.
	down := &fakeCollector{name: "cpa", err: errors.New("cpa unreachable")}
	second := newActionableApp(store, base.Add(41*time.Minute), []domain.Collector{down})
	n2 := &fakeNotifier{name: "feishu"}
	second.SetNotifiers([]domain.Notifier{n2})
	second.runCycle(context.Background())
	if n2.calls.Load() != 0 {
		t.Fatalf("restart re-pushed monitor_down: calls=%d", n2.calls.Load())
	}
	if len(openItems(store.st)) != 1 {
		t.Fatalf("open item lost across restart: %+v", store.st.PendingItems)
	}
}

func TestMonitorDownWholeCollectorFailureRecovers(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	c := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}}
	a := newActionableApp(store, base, []domain.Collector{c})
	a.SetNotifiers([]domain.Notifier{&fakeNotifier{name: "feishu"}})
	a.runCycle(context.Background())

	c.snaps = nil
	c.err = errors.New("cpa unreachable")
	a.now = func() time.Time { return base.Add(40 * time.Minute) }
	a.runCycle(context.Background())
	if len(openItems(store.st)) != 1 {
		t.Fatalf("setup: monitor_down not open: %+v", openItems(store.st))
	}

	// Collector comes back: the item closes.
	c.snaps = []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}
	c.err = nil
	a.now = func() time.Time { return base.Add(42 * time.Minute) }
	a.runCycle(context.Background())
	if len(openItems(store.st)) != 0 {
		t.Fatalf("monitor_down not resolved on recovery: %+v", openItems(store.st))
	}
}

// TestOMCOutageOpensAfterThresholdWithoutPush verifies omc_outage becomes a
// pending item once past the threshold and never an immediate push, and that its
// start is persisted.
func TestOMCOutageOpensAfterThresholdWithoutPush(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	c := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}}
	a := newActionableApp(store, base, []domain.Collector{c})
	a.SetOutageSource(&stubOutage{active: true, since: base.Add(-3 * time.Hour)})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})

	a.runCycle(context.Background())
	if n.calls.Load() != 0 {
		t.Fatalf("omc_outage must not push immediately: calls=%d", n.calls.Load())
	}
	items := openItems(store.st)
	if len(items) != 1 || items[0].Kind != string(domain.AlertOMCOutage) {
		t.Fatalf("omc_outage not opened: %+v", items)
	}
	wantSince := base.Add(-3 * time.Hour)
	if !store.st.OMCOutageSince.Equal(wantSince) {
		t.Fatalf("OMCOutageSince = %v, want %v", store.st.OMCOutageSince, wantSince)
	}
}

// TestOMCOutageSinceSurvivesRestart checks the start is bridged back to the
// persisted instant after a restart, so the threshold is not restarted.
func TestOMCOutageSinceSurvivesRestart(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	originalStart := base.Add(-3 * time.Hour)

	first := newActionableApp(store, base, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}},
	})
	first.SetOutageSource(&stubOutage{active: true, since: originalStart})
	first.SetNotifiers([]domain.Notifier{&fakeNotifier{name: "feishu"}})
	first.runCycle(context.Background())
	opened, ok := store.st.PendingItem(pendingIDForTest(t, store, domain.AlertOMCOutage))
	if !ok {
		t.Fatalf("setup: omc_outage not opened: %+v", store.st.PendingItems)
	}

	// Restart: the fresh process only knows the outage from its own first
	// failure, later than the real start.
	restarted := newActionableApp(store, base.Add(10*time.Minute), []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}},
	})
	restarted.SetOutageSource(&stubOutage{active: true, since: base.Add(10 * time.Minute)})
	restarted.SetNotifiers([]domain.Notifier{&fakeNotifier{name: "feishu"}})
	restarted.runCycle(context.Background())

	if !store.st.OMCOutageSince.Equal(originalStart) {
		t.Fatalf("outage start reset across restart: got %v want %v", store.st.OMCOutageSince, originalStart)
	}
	after, ok := store.st.PendingItem(opened.ID)
	if !ok || !after.Since.Equal(opened.Since) {
		t.Fatalf("open item Since changed across restart: %+v -> %+v", opened, after)
	}
}

func TestOMCOutageClosesOnRecovery(t *testing.T) {
	store := newMemStore()
	store.st.Bootstrapped = true
	base := outageTestTime()
	c := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{accountSnap("codex-1", 10, nil)}}
	src := &stubOutage{active: true, since: base.Add(-3 * time.Hour)}
	a := newActionableApp(store, base, []domain.Collector{c})
	a.SetOutageSource(src)
	a.SetNotifiers([]domain.Notifier{&fakeNotifier{name: "feishu"}})
	a.runCycle(context.Background())
	if len(openItems(store.st)) != 1 {
		t.Fatalf("setup: omc_outage not open: %+v", openItems(store.st))
	}

	src.active = false
	a.now = func() time.Time { return base.Add(time.Minute) }
	a.runCycle(context.Background())
	if len(openItems(store.st)) != 0 {
		t.Fatalf("omc_outage not resolved on recovery: %+v", openItems(store.st))
	}
	if !store.st.OMCOutageSince.IsZero() {
		t.Fatalf("OMCOutageSince not cleared: %v", store.st.OMCOutageSince)
	}
}

// pendingIDForTest returns the single open pending item id of kind k.
func pendingIDForTest(t *testing.T, store *memStore, kind domain.AlertKind) string {
	t.Helper()
	for _, it := range store.st.PendingItems {
		if it.Kind == string(kind) && it.Status == state.PendingOpen {
			return it.ID
		}
	}
	t.Fatalf("no open %s item", kind)
	return ""
}
