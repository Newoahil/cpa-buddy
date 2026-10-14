package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/history"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// --- fakes -----------------------------------------------------------------

type fakeCollector struct {
	name    string
	snaps   []domain.QuotaSnapshot
	err     error
	block   chan struct{}
	started chan struct{}
	calls   atomic.Int64
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Collect(ctx context.Context) ([]domain.QuotaSnapshot, error) {
	f.calls.Add(1)
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.snaps, f.err
}

type fakeNotifier struct {
	name  string
	calls atomic.Int64
	fail  bool
	block bool

	mu   sync.Mutex
	last domain.Message
}

func (f *fakeNotifier) Name() string { return f.name }

func (f *fakeNotifier) Notify(ctx context.Context, msg domain.Message) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.last = msg
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.fail {
		return errors.New("channel down")
	}
	return nil
}

func (f *fakeNotifier) lastMessage() domain.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// memStore is an in-memory state.Store.
type memStore struct {
	st *state.State
}

func newMemStore() *memStore { return &memStore{st: state.Empty()} }

func (m *memStore) Load() (*state.State, error) {
	if m.st == nil {
		return state.Empty(), nil
	}
	// Return a deep-ish copy so callers cannot mutate the stored value through
	// the pointer the way a real JSON round-trip would prevent.
	return cloneState(m.st), nil
}

func (m *memStore) Save(st *state.State) error {
	if st == nil {
		return errors.New("nil state")
	}
	m.st = cloneState(st)
	return nil
}

func cloneState(in *state.State) *state.State {
	out := &state.State{
		Bootstrapped: in.Bootstrapped,
		UpdatedAt:    in.UpdatedAt,
		Credentials:  map[string]state.CredentialRecord{},
		Pending:      map[string][]domain.Alert{},
	}
	for k, v := range in.Credentials {
		out.Credentials[k] = v
	}
	for k, v := range in.Pending {
		out.Pending[k] = append([]domain.Alert(nil), v...)
	}
	return out
}

func testConfig() config.Config {
	return config.Config{
		PollInterval:       time.Minute,
		StaleAfterFailures: 2,
		AnomalyConsecutive: 3,
		CooldownAlertAfter: 5 * time.Minute,
	}
}

func TestAnnotateFreshnessAcrossEvaluation(t *testing.T) {
	e := evaluate.New(testConfig(), nil)
	now := time.Now()
	prev := state.Empty()
	s := domain.QuotaSnapshot{Credential: domain.Credential{Key: "fixture", Provider: domain.ProviderCodex}, Failure: domain.FailureUnsupported, FetchedAt: now}
	for i := 0; i < 10; i++ {
		r, _, next := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now)
		annotateFreshness(&r, prev)
		got := r.Providers[0].Snapshots[0]
		if got.Stale || !got.LastSuccessAt.IsZero() || got.Failure != domain.FailureUnsupported {
			t.Fatalf("first unsupported became stale: %+v", got)
		}
		prev = next
	}
	fresh := freshSnap("fixture")
	fresh.Windows[0].Scope = domain.ScopeAccount
	_, _, prev = e.Evaluate(prev, []domain.QuotaSnapshot{fresh}, now)
	r, _, _ := e.Evaluate(prev, []domain.QuotaSnapshot{s}, now.Add(time.Minute))
	annotateFreshness(&r, prev)
	got := r.Providers[0].Snapshots[0]
	if !got.Stale || len(got.Windows) != 1 || !got.LastSuccessAt.Equal(now) {
		t.Fatalf("prior reading lost freshness: %+v", got)
	}
}

func TestAnnotateFreshnessUsesOnlyExistingEvidence(t *testing.T) {
	now := time.Now()
	u := 20.0
	for _, tc := range []struct {
		name      string
		snap      domain.QuotaSnapshot
		projected domain.CredentialState
		last      time.Time
		want      bool
	}{
		{"no evidence", domain.QuotaSnapshot{}, domain.StateUnknown, time.Time{}, false},
		{"old windows", domain.QuotaSnapshot{Windows: []domain.QuotaWindow{{UsedPercent: &u}}}, domain.StateUnknown, time.Time{}, true},
		{"last success", domain.QuotaSnapshot{}, domain.StateUnknown, now, true},
		{"evaluator flag", domain.QuotaSnapshot{Stale: true}, domain.StateUnknown, time.Time{}, true},
		{"evaluator projection", domain.QuotaSnapshot{}, domain.StateStale, time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.snap.Credential.Key = "fixture"
			r := domain.Report{Providers: []domain.ProviderReport{{Snapshots: []domain.QuotaSnapshot{tc.snap}, States: map[string]domain.CredentialState{"fixture": tc.projected}}}}
			prev := state.Empty()
			prev.Credentials["fixture"] = state.CredentialRecord{LastSuccessAt: tc.last}
			annotateFreshness(&r, prev)
			if r.Providers[0].Snapshots[0].Stale != tc.want {
				t.Fatalf("unexpected stale flag: %+v", r)
			}
		})
	}
}

// freshSnap is a healthy snapshot with one reported window.
func freshSnap(key string) domain.QuotaSnapshot {
	used := 10.0
	return domain.QuotaSnapshot{
		Credential:    domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: key, ShortID: "s1"},
		Windows:       []domain.QuotaWindow{{Name: "5h", UsedPercent: &used}},
		Source:        domain.SourceCPA,
		Confidence:    domain.ConfidenceReported,
		FetchedAt:     time.Now(),
		LastSuccessAt: time.Now(),
		OK:            true,
	}
}

// exhaustSnap crosses the urgent threshold so the engine emits an alert.
func exhaustSnap(key string) domain.QuotaSnapshot {
	used := 100.0
	return domain.QuotaSnapshot{
		Credential:    domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: key, ShortID: "s1"},
		Windows:       []domain.QuotaWindow{{Name: "5h", UsedPercent: &used}},
		Source:        domain.SourceCPA,
		Confidence:    domain.ConfidenceReported,
		FetchedAt:     time.Now(),
		LastSuccessAt: time.Now(),
		OK:            true,
	}
}

func newTestApp(store state.Store, collectors []domain.Collector) *App {
	cfg := testConfig()
	engine := evaluate.New(cfg, nil)
	return New(cfg, collectors, engine, store, nil)
}

// --- tests -----------------------------------------------------------------

func TestOutboxQueuesBeforeNotifyAndClearsOnSuccess(t *testing.T) {
	store := newMemStore()
	// First cycle with an exhausted credential: emits an alert and queues it.
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{exhaustSnap("codex-1")}},
	})
	okNotifier := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{okNotifier})

	if _, fresh := a.runCycle(context.Background()); !fresh {
		t.Fatal("cycle should be fresh")
	}
	if okNotifier.calls.Load() != 1 {
		t.Fatalf("notifier calls = %d, want 1", okNotifier.calls.Load())
	}
	// Delivered successfully, so the outbox must be empty and persisted.
	if got := store.st.PendingFor("feishu"); len(got) != 0 {
		t.Fatalf("outbox not cleared after success: %+v", got)
	}
}

func TestOutboxRetriesAfterFailure(t *testing.T) {
	store := newMemStore()
	okSnaps := []domain.QuotaSnapshot{exhaustSnap("codex-1")}

	// Cycle 1: alert emitted, channel down -> stays queued.
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: okSnaps},
	})
	down := &fakeNotifier{name: "feishu", fail: true}
	a.SetNotifiers([]domain.Notifier{down})
	if _, fresh := a.runCycle(context.Background()); !fresh {
		t.Fatal("cycle should be fresh")
	}
	if down.calls.Load() != 1 {
		t.Fatalf("first attempt calls = %d, want 1", down.calls.Load())
	}
	queued := store.st.PendingFor("feishu")
	if len(queued) == 0 {
		t.Fatal("alert was not queued after a failed send")
	}

	// Cycle 2: state unchanged, so the engine emits no new alerts; the queued
	// alert must still be delivered.
	up := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{up})
	if _, fresh := a.runCycle(context.Background()); !fresh {
		t.Fatal("cycle should be fresh")
	}
	if up.calls.Load() != 1 {
		t.Fatalf("retry calls = %d, want 1", up.calls.Load())
	}
	if len(up.lastMessage().Alerts) != len(queued) {
		t.Errorf("retried message alerts = %d, want %d", len(up.lastMessage().Alerts), len(queued))
	}
	if got := store.st.PendingFor("feishu"); len(got) != 0 {
		t.Fatalf("outbox not cleared after successful retry: %+v", got)
	}
}

func TestStartupFlushDeliversLeftoverPending(t *testing.T) {
	store := newMemStore()
	// Simulate a previous process that queued but crashed before delivering.
	st := state.Empty()
	st.QueuePending([]string{"feishu"}, []domain.Alert{{
		Kind:       domain.AlertQuotaExhausted,
		Severity:   domain.SeverityUrgent,
		Evidence:   domain.EvidenceConfirmed,
		Credential: domain.Credential{Key: "codex-1", Provider: domain.ProviderCodex},
		Title:      "额度已耗尽",
	}})
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(store, nil)
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})

	a.FlushPending(context.Background())
	if n.calls.Load() != 1 {
		t.Fatalf("startup flush calls = %d, want 1", n.calls.Load())
	}
	if len(n.lastMessage().Alerts) != 1 {
		t.Fatalf("flushed alerts = %d, want 1", len(n.lastMessage().Alerts))
	}
	if got := store.st.PendingFor("feishu"); len(got) != 0 {
		t.Fatalf("pending not cleared after startup flush: %+v", got)
	}
}

func TestControlPlaneFailureDoesNotSkipEvaluation(t *testing.T) {
	store := newMemStore()
	// Seed a prior healthy state so the credential can age toward stale.
	prev := state.Empty()
	prev.Bootstrapped = true
	prev.Credentials["codex-1"] = state.CredentialRecord{State: domain.StateHealthy}
	if err := store.Save(prev); err != nil {
		t.Fatal(err)
	}

	controlErr := errors.New("collect: CPA control plane unavailable")
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{}, err: fmt.Errorf("wrapped: %w", controlErr)},
	})
	a.SetControlPlaneError(controlErr)

	rep, fresh := a.runCycle(context.Background())
	if !fresh {
		t.Fatal("a control-plane failure must not skip evaluation")
	}
	if !rep.Degraded {
		t.Error("report should be marked degraded")
	}
	if len(rep.Notes) == 0 {
		t.Error("report should carry a degradation note")
	}
	found := false
	for _, p := range rep.Providers {
		if p.Provider == domain.ProviderCodex {
			found = true
			if p.Error == "" {
				t.Error("provider should carry a control-plane error")
			}
		}
	}
	if !found {
		t.Errorf("provider-level fault not attributed; providers=%+v", rep.Providers)
	}
	// The existing credential must still be present (aged), not vanish.
	if _, ok := store.st.Credentials["codex-1"]; !ok {
		t.Error("existing credential disappeared from state")
	}
}

func TestCollectorTimeoutIsNotCallerCancellation(t *testing.T) {
	store := newMemStore()
	// A collector returning its own deadline error must not abort the cycle.
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{freshSnap("codex-1")}, err: context.DeadlineExceeded},
	})
	rep, fresh := a.runCycle(context.Background())
	if !fresh {
		t.Fatal("collector timeout must not skip evaluation")
	}
	if !rep.Degraded {
		t.Error("report should be degraded when a collector fails")
	}
}

func TestCallerCancellationSkipsEvaluation(t *testing.T) {
	store := newMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", err: context.Canceled},
	})
	if _, fresh := a.runCycle(ctx); fresh {
		t.Fatal("caller cancellation should skip evaluation")
	}
}

func TestLastReportServesEvaluatedReportWithoutPersistOrNotify(t *testing.T) {
	store := newMemStore()
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{exhaustSnap("codex-1")}},
	})
	n := &fakeNotifier{name: "feishu"}
	a.SetNotifiers([]domain.Notifier{n})

	if _, ok := a.LastReport(); ok {
		t.Fatal("expected no last report before first cycle")
	}

	rep, ok := a.runCycle(context.Background())
	if !ok || len(rep.Providers) == 0 {
		t.Fatal("expected a report from runCycle")
	}
	last, ok := a.LastReport()
	if !ok || len(last.Providers) == 0 {
		t.Fatal("expected LastReport after runCycle")
	}
}

func TestRunCycleWritesHistory(t *testing.T) {
	dir := t.TempDir()
	hist, err := history.NewWriter(dir, time.UTC, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()

	store := newMemStore()
	snap := freshSnap("codex-1")
	a := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{snap}},
	})
	a.SetHistory(hist)

	if _, ok := a.runCycle(context.Background()); !ok {
		t.Fatal("runCycle failed")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected history file to be written")
	}
	recs, err := history.Read(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("expected history records")
	}
}

func TestRunCycleReusesInFlightRun(t *testing.T) {
	store := newMemStore()
	block := make(chan struct{})
	col := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{freshSnap("codex-1")}, block: block}
	a := newTestApp(store, []domain.Collector{col})

	// Start a cycle that blocks inside Collect.
	done := make(chan struct{})
	go func() {
		a.runCycle(context.Background())
		close(done)
	}()

	// Wait until the collector is actually running.
	deadline := time.After(2 * time.Second)
	for col.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("collector never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// A concurrent cycle now must observe the in-flight run, not start another.
	done2 := make(chan struct{})
	go func() {
		a.runCycle(context.Background())
		close(done2)
	}()

	time.Sleep(50 * time.Millisecond)
	if col.calls.Load() != 1 {
		t.Fatalf("collector calls = %d, want 1 (no second scrape)", col.calls.Load())
	}

	close(block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle 1 did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle 2 did not finish")
	}
}

func TestSendSummaryMarksDegradedFallback(t *testing.T) {
	store := newMemStore()
	// Seed a report from a previous successful cycle.
	good := newTestApp(store, []domain.Collector{
		&fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{freshSnap("codex-1")}},
	})
	n := &fakeNotifier{name: "feishu"}
	if _, ok := good.runCycle(context.Background()); !ok {
		t.Fatal("runCycle failed")
	}
	good.SetNotifiers([]domain.Notifier{n})

	// Now a summary whose cycle is aborted by a cancelled context must fall
	// back to the cached report and mark it degraded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	good.SendSummary(ctx)

	if n.calls.Load() != 1 {
		t.Fatalf("summary calls = %d, want 1", n.calls.Load())
	}
	if n.lastMessage().Report == nil || !n.lastMessage().Report.Degraded {
		t.Errorf("fallback summary must be degraded, got %+v", n.lastMessage().Report)
	}
	if len(n.lastMessage().Report.Notes) == 0 {
		t.Error("fallback summary should carry a note")
	}
}

func TestNextPollWaitSuppressesFastPollDuringFallback(t *testing.T) {
	cfg := testConfig()
	cfg.PollInterval = 15 * time.Minute
	cfg.FastPollInterval = 3 * time.Minute
	engine := evaluate.New(cfg, nil)
	a := New(cfg, nil, engine, newMemStore(), nil)
	used := 95.0
	rep := domain.Report{Providers: []domain.ProviderReport{{
		Provider:  domain.ProviderCodex,
		Snapshots: []domain.QuotaSnapshot{{OK: true, Windows: []domain.QuotaWindow{{UsedPercent: &used}}}},
	}}}
	if got := a.nextPollWait(rep); got != 3*time.Minute {
		t.Fatalf("normal fast poll = %v, want 3m", got)
	}
	a.SetFallbackActive(func() bool { return true })
	if got := a.nextPollWait(rep); got != 15*time.Minute {
		t.Fatalf("fallback poll = %v, want the regular 15m", got)
	}
	a.SetFallbackActive(func() bool { return false })
	if got := a.nextPollWait(rep); got != 3*time.Minute {
		t.Fatalf("recovered fast poll = %v, want 3m", got)
	}
}
