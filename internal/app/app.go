// Package app wires collection, evaluation, persistence and notification into
// the watcher's control loop.
//
// Collection is entirely scheduler-driven: runCycle is the only path that
// touches upstreams. It evaluates against the persisted state, saves the new
// state and queues alerts to the durable outbox. Readers (@Bot queries, the
// status page) serve LastReport, the most recent evaluated report, and never
// trigger a scrape. This removes the on-demand refresh that used to let a burst
// of card clicks fan out into upstream requests.
//
// Delivery is at-least-once. Alerts are first written to a per-channel outbox
// inside the state file, and only cleared after a successful send. A Feishu
// hiccup therefore delays an alert instead of losing it; it is retried on the
// next cycle and once more at startup.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cooldown"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/history"
	"github.com/Newoahil/CPA-Manager/internal/notify/policy"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// sendTimeout bounds one channel delivery attempt. It is intentionally short:
// a cycle waits on it while the outbox is locked, so a hanging endpoint must not
// stall the watcher for a full poll interval.
const sendTimeout = 10 * time.Second

// App is the orchestrator. It implements domain.ReportSource.
type App struct {
	cfg        config.Config
	collectors []domain.Collector
	engine     *evaluate.Engine
	store      state.Store
	log        *slog.Logger
	history    *history.Writer

	// run is the single-flight collection coordinator. Only one collection may
	// be in flight; callers that arrive while one runs observe the same result
	// instead of starting a second scrape. Guarded by runMu.
	runMu sync.Mutex
	run   *runState

	// persistMu serialises evaluation + persistence between concurrent cycles.
	// It is a plain mutex because cycles are not latency-sensitive.
	persistMu sync.Mutex

	// outboxMu serialises the read-send-clear-save transaction of the alert
	// outbox so a concurrent producer cannot be lost by a clear.
	outboxMu sync.Mutex

	reportMu sync.RWMutex
	last     domain.Report
	hasLast  bool

	notifyMu  sync.RWMutex
	notifiers []domain.Notifier

	// cooldown is the rate-limit cooldown watcher; nil until a lister is
	// installed with SetCooldownLister. Guarded by cooldownMu.
	cooldownMu sync.RWMutex
	cooldown   *cooldown.Watcher

	// controlPlaneErr, when set, is matched with errors.Is to recognise a CPA
	// control-plane failure. The composition root wires collect.ErrControlPlane
	// here so app does not need to import the collect package.
	controlPlaneErr error

	// fallbackActive, when set, reports whether quota collection is currently
	// reading CPA directly because the OMC source failed. It is consulted only
	// by nextPollWait, to keep the fast poll from accelerating direct upstream
	// quota queries during a fallback.
	fallbackActive func() bool

	// policy is the push classification (internal/notify/policy). In legacy
	// mode it is a pass-through and the app behaves exactly as before.
	policy policy.Config

	// outage is the OMC fallback source, installed by SetOutageSource once a
	// FallbackCollector exists. Nil leaves omc_outage dormant.
	outageMu sync.RWMutex
	outage   policy.OutageSource

	now func() time.Time
}

// runState is one in-flight collection. Observers block on done and then read
// the result under runMu.
type runState struct {
	done     chan struct{}
	snaps    []domain.QuotaSnapshot
	failures []collectFailure
	report   domain.Report
	ok       bool
	err      error
}

// collectFailure records a collector that could not run at all.
type collectFailure struct {
	name string
	err  error
}

// New builds the orchestrator. Notifiers are attached later with SetNotifiers.
func New(cfg config.Config, collectors []domain.Collector, engine *evaluate.Engine, store state.Store, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	pol := policy.Config{
		Mode:                    policy.ModeOf(cfg.AlertMode),
		ChannelUnavailableAfter: cfg.ChannelUnavailableAfter,
		MonitorDownAfter:        cfg.MonitorDownAfter,
		OMCOutagePendingAfter:   cfg.OMCOutagePendingAfter,
		Location:                cfg.Location,
	}
	// A directly-constructed config (tests, embedders) may leave these zero.
	if pol.ChannelUnavailableAfter <= 0 {
		pol.ChannelUnavailableAfter = config.DefaultChannelUnavailableAfter
	}
	if pol.MonitorDownAfter <= 0 {
		pol.MonitorDownAfter = config.DefaultMonitorDownAfter
	}
	if pol.OMCOutagePendingAfter <= 0 {
		pol.OMCOutagePendingAfter = config.DefaultOMCOutagePendingAfter
	}
	return &App{
		cfg:        cfg,
		collectors: collectors,
		engine:     engine,
		store:      store,
		log:        log,
		policy:     pol,
		now:        time.Now,
	}
}

// SetOutageSource installs the OMC fallback outage signal (a
// *collect.FallbackCollector). A nil source leaves omc_outage dormant. When the
// source also implements outageTracker the app persists the outage start so the
// pending threshold survives a restart.
func (a *App) SetOutageSource(src policy.OutageSource) {
	a.outageMu.Lock()
	a.outage = src
	a.policy.Outage = src
	a.outageMu.Unlock()
}

// outageTracker is the persistence half of the OMC outage signal: the concrete
// collector exposes the current outage start and can be seeded from state. A
// source that does not implement it is still usable, just not restart-durable.
type outageTracker interface {
	OutageStart() time.Time
	RestoreOutage(time.Time)
}

func (a *App) outageSource() policy.OutageSource {
	a.outageMu.RLock()
	defer a.outageMu.RUnlock()
	return a.outage
}

// reconcileOMCOutage merges the live collector outage with the persisted start
// so the OMC_OUTAGE_PENDING_AFTER clock is measured from the true beginning
// across a restart. It returns the instant to persist, or the zero time when no
// outage is active.
func (a *App) reconcileOMCOutage(prev *state.State, rep domain.Report, now time.Time) time.Time {
	src := a.outageSource()
	if src == nil {
		return time.Time{}
	}
	var (
		liveStart time.Time
		active    bool
		seen      = map[domain.ProviderKind]bool{}
	)
	for _, p := range rep.Providers {
		if seen[p.Provider] {
			continue
		}
		seen[p.Provider] = true
		s, isActive := src.OutageSince(p.Provider, now)
		if !isActive {
			continue
		}
		active = true
		if liveStart.IsZero() || s.Before(liveStart) {
			liveStart = s
		}
	}
	if !active {
		return time.Time{}
	}
	if prev != nil && !prev.OMCOutageSince.IsZero() && prev.OMCOutageSince.Before(liveStart) {
		// The outage predates this process: bridge back to its real start.
		liveStart = prev.OMCOutageSince
	}
	if tr, ok := src.(outageTracker); ok {
		tr.RestoreOutage(liveStart)
	}
	return liveStart
}

// updateChannelTracks advances the per-provider last-valid-data memory for
// monitor_down. A channel's FirstSeen is stamped the first time it appears; its
// LastValid only moves when it actually produced a valid reading. Providers
// missing from the report are left untouched.
func updateChannelTracks(prev map[domain.ProviderKind]state.ChannelTrack, rep domain.Report, now time.Time) map[domain.ProviderKind]state.ChannelTrack {
	out := make(map[domain.ProviderKind]state.ChannelTrack, len(prev)+len(rep.Providers))
	for k, v := range prev {
		out[k] = v
	}
	for _, p := range rep.Providers {
		t := out[p.Provider]
		if t.FirstSeen.IsZero() {
			t.FirstSeen = now
		}
		if reportProviderHasValid(p) {
			t.LastValid = now
		}
		out[p.Provider] = t
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reportProviderHasValid reports whether any enabled account produced a valid
// reading in this provider's report.
func reportProviderHasValid(p domain.ProviderReport) bool {
	for _, s := range p.Snapshots {
		if !s.Credential.Disabled && s.OK {
			return true
		}
	}
	return false
}

// actionable reports whether the new push policy is active.
func (a *App) actionable() bool { return a.policy.Mode == policy.Actionable }

// inQuiet reports whether immediate pushes are currently held back. Legacy mode
// never holds anything back, so a rollback is behaviour-identical.
func (a *App) inQuiet(now time.Time) bool {
	if !a.actionable() {
		return false
	}
	loc := a.cfg.Location
	if loc == nil {
		loc = time.Local
	}
	return a.cfg.QuietHours.Contains(now.In(loc))
}

// SetHistory installs the quota-history writer. Without it, successful cycles
// are not recorded. It is called once before RunPoll.
func (a *App) SetHistory(w *history.Writer) { a.history = w }

// SetControlPlaneError installs the sentinel used to recognise CPA control-plane
// failures (for example collect.ErrControlPlane). A nil value disables the
// distinction and every collector failure is treated as a generic channel error.
func (a *App) SetControlPlaneError(err error) { a.controlPlaneErr = err }

// SetFallbackActive installs a predicate reporting whether quota collection is
// currently falling back to direct CPA queries because the OMC source failed.
// While it reports true, nextPollWait keeps the regular interval so the fallback
// cannot turn into a faster stream of direct upstream quota calls.
func (a *App) SetFallbackActive(f func() bool) { a.fallbackActive = f }

// SetNotifiers installs the outbound channels.
func (a *App) SetNotifiers(n []domain.Notifier) {
	a.notifyMu.Lock()
	defer a.notifyMu.Unlock()
	a.notifiers = n
}

// SetCooldownLister installs the credential lister the rate-limit cooldown
// watcher polls (typically cpa.Client.ListCredentials). Without it, or with
// COOLDOWN_POLL_INTERVAL=0, the watcher does not run. Call it before RunPoll.
func (a *App) SetCooldownLister(list cooldown.Lister) {
	var w *cooldown.Watcher
	if list != nil {
		w = cooldown.New(list, cooldown.Options{
			AlertAfter: a.cfg.CooldownAlertAfter,
			// Late-bound so a test that swaps a.now drives the watcher too.
			Now: func() time.Time { return a.now() },
		})
	}
	a.cooldownMu.Lock()
	a.cooldown = w
	a.cooldownMu.Unlock()
}

func (a *App) cooldownWatcher() *cooldown.Watcher {
	a.cooldownMu.RLock()
	defer a.cooldownMu.RUnlock()
	return a.cooldown
}

// LastReport returns the most recent evaluated report without touching any
// upstream.
func (a *App) LastReport() (domain.Report, bool) {
	a.reportMu.RLock()
	defer a.reportMu.RUnlock()
	return a.last, a.hasLast
}

func (a *App) setLast(r domain.Report) {
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	a.last, a.hasLast = r, true
}

// ---------------------------------------------------------------------------
// Scheduled authoritative path
// ---------------------------------------------------------------------------

// RunPoll runs the scheduled collection loop until ctx is done.
func (a *App) RunPoll(ctx context.Context) error {
	// The cooldown watcher runs beside the quota loop but makes only credential
	// list calls, so it can poll far more often without touching upstreams.
	var cooldownDone sync.WaitGroup
	cooldownDone.Add(1)
	go func() {
		defer cooldownDone.Done()
		a.runCooldown(ctx)
	}()
	defer cooldownDone.Wait()

	// Deliver anything the previous process left queued before touching
	// upstreams, so a restart does not delay a pending alert. During quiet
	// hours this is held back instead.
	if !a.inQuiet(a.now()) {
		a.FlushPending(ctx)
	}
	// Run once immediately so the status page and the first summary are not
	// empty for a whole interval.
	report, _ := a.runCycle(ctx)

	for {
		wait := a.nextPollWait(report)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
			report, _ = a.runCycle(ctx)
		}
	}
}

// nextPollWait is the normal interval, or the fast one while any account
// window is past its notice threshold: the stretch from "nearly out" to empty
// can be shorter than one normal interval, and missing it means no alert
// until after the account is already exhausted.
//
// During a direct-CPA fallback the regular interval is used regardless: the
// fast poll exists to watch an upstream closely through OMC, and applying it
// while reading CPA directly would just query the upstream more often.
func (a *App) nextPollWait(rep domain.Report) time.Duration {
	// A direct-CPA fallback keeps the regular interval: the fast poll exists to
	// watch an upstream closely through OMC, and applying it while reading CPA
	// directly would just query the upstream more often.
	if a.fallbackActive != nil && a.fallbackActive() {
		return a.cfg.PollInterval
	}
	// Actionable mode drops the "past 90% -> 3-minute poll" rule: nothing is
	// pushed at that threshold any more, so a faster poll would only add
	// upstream load. The fast interval is still honoured in legacy mode.
	if a.actionable() {
		return a.cfg.PollInterval
	}
	fast := a.cfg.FastPollInterval
	if fast <= 0 || fast >= a.cfg.PollInterval {
		return a.cfg.PollInterval
	}
	for _, p := range rep.Providers {
		notice := a.cfg.ThresholdsFor(p.Provider).Notice
		for _, s := range p.Snapshots {
			for _, w := range s.Windows {
				if w.UsedPercent != nil && *w.UsedPercent >= notice && *w.UsedPercent < 100 {
					return fast
				}
			}
		}
	}
	return a.cfg.PollInterval
}

// runCooldown polls CPA's cooldown data every cfg.CooldownPollInterval until
// ctx is done. It does nothing when disabled (interval 0) or no lister is set.
func (a *App) runCooldown(ctx context.Context) {
	w := a.cooldownWatcher()
	if w == nil || a.cfg.CooldownPollInterval <= 0 {
		return
	}
	a.cooldownTick(ctx, w)
	ticker := time.NewTicker(a.cfg.CooldownPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cooldownTick(ctx, w)
		}
	}
}

// cooldownTick runs one watcher poll and delivers whatever became due.
func (a *App) cooldownTick(ctx context.Context, w *cooldown.Watcher) {
	alerts, err := w.Poll(ctx)
	if err != nil {
		if ctx.Err() == nil {
			// The error text is CPA transport/status information, never
			// credential payload.
			a.log.Warn("cooldown poll failed", "err", err)
		}
		return
	}
	if len(alerts) == 0 {
		return
	}
	a.log.Info("cooldown alerts", "count", len(alerts))
	a.enqueueAlerts(ctx, alerts)
}

// enqueueAlerts hands alerts produced outside the evaluation engine to the same
// durable outbox runCycle uses, so they get the same at-least-once retry, then
// flushes. All alerts from one poll leave in one message (FlushPending builds it
// with buildAlertMessage). If the outbox cannot be used, the alerts are sent
// directly once rather than lost.
func (a *App) enqueueAlerts(ctx context.Context, alerts []domain.Alert) {
	if len(a.notifierSnapshot()) == 0 {
		return
	}
	// Actionable mode drops the cooldown watcher's rate_limited /
	// rate_limit_cleared events (§2.2): only a credential or a channel outage
	// is worth a push. Legacy mode keeps them.
	if a.actionable() {
		out := policy.Evaluate(a.policy, policy.Input{Alerts: alerts, Now: a.now()})
		alerts = out.Immediate
		if len(alerts) == 0 {
			return
		}
	}
	if a.queueAlerts(alerts) {
		a.FlushPending(ctx)
		return
	}
	var report *domain.Report
	if last, ok := a.LastReport(); ok {
		report = &last
	}
	a.dispatch(ctx, buildAlertMessage(report, alerts))
}

// queueAlerts appends alerts to the outbox under the same lock order as
// runCycle (persistMu, then outboxMu). It reports whether they were persisted.
func (a *App) queueAlerts(alerts []domain.Alert) bool {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	st, err := a.store.Load()
	if err != nil || st == nil {
		// Saving a substitute empty state could clobber a state file that is
		// only temporarily unreadable.
		a.log.Error("cooldown: state load failed, sending directly", "err", err)
		return false
	}
	a.outboxMu.Lock()
	defer a.outboxMu.Unlock()
	st.QueuePending(a.channelNames(), alerts)
	if err := a.store.Save(st); err != nil {
		a.log.Error("cooldown: state save failed, sending directly", "err", err)
		return false
	}
	return true
}

// SendSummary refreshes and pushes the scheduled digest. It is what the
// 10:00/17:00 scheduler calls.
//
// When this cycle could not produce a report it never presents the previous one
// as a normal daily digest: the fallback is explicitly marked degraded.
func (a *App) SendSummary(ctx context.Context) {
	report, fresh := a.runCycle(ctx)
	if !fresh {
		last, ok := a.LastReport()
		if !ok {
			a.log.Warn("summary skipped: no report yet")
			return
		}
		last.Degraded = true
		last.Notes = appendNote(last.Notes, "本轮采集未完成，以下为上一轮结果")
		report = last
	}
	if a.actionable() {
		a.sendActionableSummary(ctx, report)
		return
	}
	// Short rate-limit cooldowns since the previous digest ride along on this
	// copy of the report (never on the cached one) and are then reset. A digest
	// that fails to send is not retried, so neither are its tallies.
	if w := a.cooldownWatcher(); w != nil {
		report.RateLimits = w.TakeTallies()
	}
	a.dispatch(ctx, domain.Message{
		Title:  "额度日报",
		Kind:   string(domain.AlertBootstrap),
		Report: &report,
	})
}

// sendActionableSummary applies §3.1: send only when something needs attention
// (an open pending item or a tight channel), and never during quiet hours.
func (a *App) sendActionableSummary(ctx context.Context, report domain.Report) {
	if a.inQuiet(a.now()) {
		a.log.Info("summary skipped: quiet hours")
		return
	}
	var items []state.PendingItem
	if st, err := a.store.Load(); err == nil && st != nil {
		items = st.PendingItems
	}
	var open []state.PendingItem
	for _, it := range items {
		if it.Status == state.PendingOpen {
			open = append(open, it)
		}
	}
	tight := policy.TightChannels(report, a.cfg.SummaryTightRemaining)
	if len(open) == 0 && len(tight) == 0 {
		a.log.Info("summary skipped: nothing to report")
		return
	}

	card := &domain.ActionableCard{
		HeaderColor: summaryColor(open, tight),
		Conclusion:  summaryConclusion(open, tight),
		Tight:       tight,
	}
	for _, it := range open {
		card.Pending = append(card.Pending, domain.PendingRow{
			Kind:     domain.AlertKind(it.Kind),
			Provider: it.Provider,
			Alias:    it.Alias,
			Since:    it.Since,
			Action:   it.Action,
		})
	}
	a.dispatch(ctx, domain.Message{
		Title:      "额度汇总",
		Kind:       string(domain.AlertActionableSummary),
		Report:     &report,
		Actionable: card,
	})
}

// summaryColor is the header template: the worst pending item, else the tight
// channel's warning colour.
func summaryColor(pending []state.PendingItem, tight []domain.TightChannel) string {
	if len(pending) > 0 {
		for _, it := range pending {
			switch domain.AlertKind(it.Kind) {
			case domain.AlertCredential, domain.AlertChannelUnavailable:
				return "red"
			}
		}
		return "orange"
	}
	if len(tight) > 0 {
		return "orange"
	}
	return "blue"
}

// summaryConclusion is the one-line verdict, e.g. "Claude 紧张，有 1 项待处理".
func summaryConclusion(pending []state.PendingItem, tight []domain.TightChannel) string {
	var clauses []string
	if len(tight) > 0 {
		names := make([]string, 0, len(tight))
		for _, t := range tight {
			names = append(names, providerDisplay(t.Provider))
		}
		clauses = append(clauses, strings.Join(names, "、")+" 紧张")
	}
	if len(pending) > 0 {
		clauses = append(clauses, "有 "+strconv.Itoa(len(pending))+" 项待处理")
	}
	return strings.Join(clauses, "，")
}

func providerDisplay(p domain.ProviderKind) string {
	switch p {
	case domain.ProviderCodex:
		return "Codex"
	case domain.ProviderClaude:
		return "Claude"
	case domain.ProviderAntigravity:
		return "Antigravity"
	case domain.ProviderGeminiCLI:
		return "Gemini CLI"
	case domain.ProviderOllama:
		return "Ollama"
	default:
		return string(p)
	}
}

// runCycle is the authoritative path: collect, evaluate, persist, queue, notify.
//
// It returns the report produced and whether it came from this cycle's own
// collection (as opposed to a reused concurrent run that failed).
func (a *App) runCycle(ctx context.Context) (domain.Report, bool) {
	r, runner := a.beginRun()

	var (
		snaps    []domain.QuotaSnapshot
		failures []collectFailure
	)
	started := a.now()
	if runner {
		s, f, err := a.collect(ctx)
		if err != nil {
			// Only a cancelled/expired caller context aborts here; collector
			// failures are recorded as degraded data, not as an abort.
			a.finishRun(r, nil, nil, domain.Report{}, false, err)
			a.log.Warn("collection aborted, skipping evaluation", "err", err)
			return domain.Report{}, false
		}
		snaps, failures = s, f
	} else {
		// Another collection is already running: reuse its result instead of
		// scraping twice.
		select {
		case <-r.done:
			if !r.ok {
				return domain.Report{}, false
			}
			snaps, failures = r.snaps, r.failures
		case <-ctx.Done():
			return domain.Report{}, false
		}
	}

	now := a.now()

	// Record quota history once per successful collection, from this cycle's
	// own snapshots. Only the single-flight runner writes, so the non-concurrent
	// history.Writer is never called from two goroutines.
	if runner {
		a.recordHistory(now, snaps)
	}

	// Evaluation and persistence are serialised; the collection gate is released
	// only after the state (including the outbox) is durable.
	a.persistMu.Lock()
	prev, loadErr := a.store.Load()
	if loadErr != nil {
		a.log.Error("state load failed, continuing from empty state", "err", loadErr)
	}
	if prev == nil {
		prev = state.Empty()
	}

	report, alerts, next := a.engine.Evaluate(prev, snaps, now)
	a.decorateReport(&report, prev, failures)
	a.setLast(report)

	// Advance the monitor_down memory and reconcile the OMC outage window (so
	// the policy sees a start that survives this restart) before classifying.
	tracks := updateChannelTracks(prev.ChannelTracks, report, now)
	outageSince := a.reconcileOMCOutage(prev, report, now)

	// Classify: what to push, what to open/close as a pending item, what to
	// drop. In legacy mode this is a pass-through and nothing else changes.
	quiet := a.inQuiet(now)
	outcome := policy.Evaluate(a.policy, policy.Input{
		Report:        report,
		Alerts:        alerts,
		Prev:          prev.PendingItems,
		ChannelTracks: tracks,
		Now:           now,
		Quiet:         quiet,
	})

	var quietDigest, bootstrapNow []domain.Alert
	if next != nil {
		next.PendingItems = outcome.Items
		next.ChannelTracks = tracks
		next.OMCOutageSince = outageSince
		// Resolved items are kept briefly then dropped, so the file cannot grow
		// without bound.
		next.PruneResolved(now)
		next.QuietQueue = cloneAlerts(prev.QuietQueue)
		var hold []domain.Alert
		bootstrapNow, hold = splitBootstrap(outcome.Immediate)
		a.outboxMu.Lock()
		if quiet {
			// Hold the noise back; the queue is persisted so a restart keeps it.
			next.QueueQuiet(hold)
		} else {
			if len(outcome.Immediate) > 0 {
				next.QueuePending(a.channelNames(), outcome.Immediate)
			}
			// A quiet window that just ended sends the still-unresolved events
			// it held back, in one merged message.
			quietDigest = stillOpen(next.QuietQueue, outcome.Items)
			next.QuietQueue = nil
		}
		if err := a.store.Save(next); err != nil {
			// Losing the save means the next cycle may re-alert. Noisy but safe.
			a.log.Error("state save failed", "err", err)
		}
		a.outboxMu.Unlock()
	}
	a.persistMu.Unlock()

	// The startup bootstrap confirms the deployment succeeded. It is not noise
	// and fires once, so it is delivered even inside quiet hours.
	if quiet && len(bootstrapNow) > 0 {
		a.dispatch(ctx, buildAlertMessage(&report, bootstrapNow))
	}

	if runner {
		a.finishRun(r, snaps, failures, report, true, nil)
	}

	a.log.Info("cycle complete",
		"snapshots", len(snaps),
		"alerts", len(outcome.Immediate),
		"quiet", quiet,
		"duration_ms", a.now().Sub(started).Milliseconds(),
	)

	if len(quietDigest) > 0 {
		// If every accumulated event recovered on its own, nothing is sent.
		a.dispatch(ctx, domain.Message{
			Title:  "免打扰结束",
			Kind:   string(domain.AlertQuietDigest),
			Alerts: quietDigest,
			Report: &report,
		})
	}

	// Notification happens outside every lock. Failures leave the alerts queued.
	// During quiet hours the outbox is deliberately left untouched.
	if !quiet {
		a.FlushPending(ctx)
	}
	return report, true
}

// splitBootstrap separates the one-off deployment confirmation from the
// problem alerts. Only the latter are held back by quiet hours.
func splitBootstrap(alerts []domain.Alert) (bootstrap, rest []domain.Alert) {
	for _, a := range alerts {
		if a.Kind == domain.AlertBootstrap {
			bootstrap = append(bootstrap, a)
		} else {
			rest = append(rest, a)
		}
	}
	return bootstrap, rest
}

// cloneAlerts is a defensive copy of a slice of alerts.
func cloneAlerts(in []domain.Alert) []domain.Alert {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.Alert, len(in))
	copy(out, in)
	return out
}

// stillOpen filters the quiet-hours queue to the alerts whose pending item is
// still open. An event that resolved on its own is dropped, which is what makes
// "everything recovered -> send nothing" work.
func stillOpen(queued []domain.Alert, items []state.PendingItem) []domain.Alert {
	open := map[string]bool{}
	for _, it := range items {
		if it.Status == state.PendingOpen {
			open[it.ID] = true
		}
	}
	var out []domain.Alert
	seen := map[string]bool{}
	for _, a := range queued {
		id := policy.AlertItemID(a)
		if id != "" && !open[id] {
			continue
		}
		key := id
		if key == "" {
			key = string(a.Kind) + "|" + string(a.Credential.Provider) + "|" + a.Credential.Key
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// recordHistory appends one line per credential per quota window for a
// successful active collection. A write failure is logged and never fails the
// cycle: monitoring must not stop because the history volume is full.
func (a *App) recordHistory(now time.Time, snaps []domain.QuotaSnapshot) {
	if a.history == nil || len(snaps) == 0 {
		return
	}
	if err := a.history.Append(now, snaps); err != nil {
		a.log.Warn("quota history write failed", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Outbox
// ---------------------------------------------------------------------------

// FlushPending attempts to deliver every queued alert exactly once per attempt,
// clearing a channel only after a successful send. It is safe to call from the
// cycle and once at startup.
func (a *App) FlushPending(ctx context.Context) {
	notifiers := a.notifierSnapshot()
	if len(notifiers) == 0 {
		return
	}

	// persistMu first (same order as runCycle and queueAlerts): otherwise a
	// cycle that loaded the outbox before this clear would save it back and
	// deliver the same alerts a second time.
	a.persistMu.Lock()
	defer a.persistMu.Unlock()

	// Serialise the whole read-send-clear-save transaction: ClearPending removes
	// all queued alerts for a channel, so a producer that queues concurrently
	// must not have its new alerts swept away by our clear.
	a.outboxMu.Lock()
	defer a.outboxMu.Unlock()

	st, err := a.store.Load()
	if err != nil {
		a.log.Error("outbox: state load failed", "err", err)
		return
	}
	if st == nil {
		return
	}

	report, hasReport := a.LastReport()
	var reportPtr *domain.Report
	if hasReport {
		reportPtr = &report
	}

	changed := false
	for _, n := range notifiers {
		pending := st.PendingFor(n.Name())
		if len(pending) == 0 {
			continue
		}
		msg := buildAlertMessage(reportPtr, pending)

		// WithoutCancel: a shutdown must not abort a send that is already on the
		// wire. If it fails, the alert stays queued for the next run.
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
		sendErr := n.Notify(sendCtx, msg)
		cancel()
		if sendErr != nil {
			a.log.Error("notify failed; alert stays queued", "channel", n.Name(), "count", len(pending), "err", sendErr)
			continue
		}
		st.ClearPending(n.Name())
		changed = true
		a.log.Info("outbox delivered", "channel", n.Name(), "count", len(pending))
	}

	if changed {
		if err := a.store.Save(st); err != nil {
			a.log.Error("outbox: state save failed", "err", err)
		}
	}
}

// dispatch sends one informational message to every notifier. It is used for
// the scheduled digest, which is not an alert and is not queued: a missed digest
// is superseded by the next one, while an alert must not be lost.
func (a *App) dispatch(ctx context.Context, msg domain.Message) {
	for _, n := range a.notifierSnapshot() {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
		if err := n.Notify(sendCtx, msg); err != nil {
			a.log.Error("notify failed", "channel", n.Name(), "err", err)
		}
		cancel()
	}
}

func (a *App) notifierSnapshot() []domain.Notifier {
	a.notifyMu.RLock()
	defer a.notifyMu.RUnlock()
	out := make([]domain.Notifier, len(a.notifiers))
	copy(out, a.notifiers)
	return out
}

func (a *App) channelNames() []string {
	a.notifyMu.RLock()
	defer a.notifyMu.RUnlock()
	out := make([]string, 0, len(a.notifiers))
	for _, n := range a.notifiers {
		out = append(out, n.Name())
	}
	return out
}

// ---------------------------------------------------------------------------
// Collection
// ---------------------------------------------------------------------------

// collect fans out to every collector.
//
// A collector-level failure is never fatal: its providers are marked with an
// error and the report is degraded, but any snapshots that did arrive are still
// evaluated. The only condition that aborts the cycle is the caller's own
// context being cancelled, because that means we have no trustworthy readings
// rather than a broken upstream.
func (a *App) collect(ctx context.Context) ([]domain.QuotaSnapshot, []collectFailure, error) {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		out   []domain.QuotaSnapshot
		fails []collectFailure
	)
	for _, c := range a.collectors {
		wg.Add(1)
		go func(c domain.Collector) {
			defer wg.Done()
			snaps, err := c.Collect(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fails = append(fails, collectFailure{name: c.Name(), err: err})
			}
			out = append(out, snaps...)
		}(c)
	}
	wg.Wait()

	// Caller cancellation is the only abort condition. A collector's own HTTP
	// deadline is not: it is an upstream failure and belongs in the report.
	if ctx.Err() != nil {
		return nil, nil, fmt.Errorf("collection context: %w", ctx.Err())
	}
	return out, fails, nil
}

// ---------------------------------------------------------------------------
// Report decoration
// ---------------------------------------------------------------------------

// providersByCollector maps a collector to the providers it speaks for, so a
// collector-wide failure can be attributed to provider reports. A collector
// owning several providers (CPA) marks them all.
var providersByCollector = map[string][]domain.ProviderKind{
	"cpa": {domain.ProviderCodex, domain.ProviderClaude, domain.ProviderAntigravity, domain.ProviderGeminiCLI},
	// The OMC source stands in for the CPA channel: same providers, same
	// fallback target, so a collector-level failure is attributed identically.
	"omc":    {domain.ProviderCodex, domain.ProviderClaude, domain.ProviderAntigravity, domain.ProviderGeminiCLI},
	"ollama": {domain.ProviderOllama},
}

// decorateReport stamps freshness onto snapshots and records collector failures
// as provider-level degradation. It only ever fills gaps; it does not overwrite
// values the engine already set.
func (a *App) decorateReport(rep *domain.Report, prev *state.State, failures []collectFailure) {
	annotateFreshness(rep, prev)
	if len(failures) == 0 {
		return
	}
	rep.Degraded = true
	for _, f := range failures {
		note, providerErr := a.classifyFailure(f)
		rep.Notes = appendNote(rep.Notes, note)
		for _, p := range providersByCollector[f.name] {
			setProviderError(rep, p, providerErr)
		}
	}
}

func (a *App) classifyFailure(f collectFailure) (note, providerErr string) {
	if a.controlPlaneErr != nil && errors.Is(f.err, a.controlPlaneErr) {
		return "CPA 控制面不可用（认证失败或不可达），本轮回退到已有数据", "CPA 控制面不可用"
	}
	switch f.name {
	case "ollama":
		return "Ollama 采集失败，本轮回退到已有数据", "Ollama 采集失败"
	default:
		return f.name + " 采集失败，本轮回退到已有数据", "渠道采集失败"
	}
}

// annotateFreshness marks failed snapshots with prior evidence stale and gives both failed and
// successful ones a LastSuccessAt, so no surface can present a stale number as
// current and none shows the failed attempt's timestamp as the data time.
func annotateFreshness(rep *domain.Report, prev *state.State) {
	for pi := range rep.Providers {
		for si := range rep.Providers[pi].Snapshots {
			s := &rep.Providers[pi].Snapshots[si]
			var recLast time.Time
			if prev != nil {
				if rec, ok := prev.Credentials[s.Credential.Key]; ok {
					recLast = rec.LastSuccessAt
				}
			}
			if s.OK {
				if s.LastSuccessAt.IsZero() {
					s.LastSuccessAt = recLast
				}
				if s.LastSuccessAt.IsZero() {
					s.LastSuccessAt = s.FetchedAt
				}
				continue
			}
			if s.LastSuccessAt.IsZero() {
				s.LastSuccessAt = recLast
			}
			s.Stale = s.Stale || len(s.Windows) > 0 || !s.LastSuccessAt.IsZero() || rep.Providers[pi].States[s.Credential.Key] == domain.StateStale
		}
	}
}

func setProviderError(rep *domain.Report, p domain.ProviderKind, msg string) {
	for i := range rep.Providers {
		if rep.Providers[i].Provider == p {
			if rep.Providers[i].Error == "" {
				rep.Providers[i].Error = msg
			}
			return
		}
	}
	rep.Providers = append(rep.Providers, domain.ProviderReport{
		Provider:   p,
		WorstState: domain.StateUnknown,
		Error:      msg,
		States:     map[string]domain.CredentialState{},
	})
}

func appendNote(notes []string, note string) []string {
	for _, n := range notes {
		if n == note {
			return notes
		}
	}
	return append(notes, note)
}

// ---------------------------------------------------------------------------
// Single-flight coordinator
// ---------------------------------------------------------------------------

func (a *App) beginRun() (*runState, bool) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.run != nil {
		return a.run, false
	}
	r := &runState{done: make(chan struct{})}
	a.run = r
	return r, true
}

func (a *App) finishRun(r *runState, snaps []domain.QuotaSnapshot, failures []collectFailure, report domain.Report, ok bool, err error) {
	a.runMu.Lock()
	r.snaps, r.failures, r.report, r.ok, r.err = snaps, failures, report, ok, err
	if a.run == r {
		a.run = nil
	}
	a.runMu.Unlock()
	close(r.done)
}

// buildAlertMessage groups alerts into a single message so one bad cycle cannot
// spam the group with one card per credential. report may be nil (a retry that
// no longer has a fresh report).
func buildAlertMessage(report *domain.Report, alerts []domain.Alert) domain.Message {
	kind := string(domain.AlertBootstrap)
	title := ""
	worst := -1
	for _, al := range alerts {
		if r := severityRank(al.Severity); r > worst {
			worst, kind = r, string(al.Kind)
		}
	}
	if len(alerts) == 1 {
		title = alerts[0].Title
	}
	return domain.Message{
		Title:  title,
		Kind:   kind,
		Alerts: alerts,
		Report: report,
	}
}

func severityRank(s domain.Severity) int {
	switch s {
	case domain.SeverityUrgent:
		return 3
	case domain.SeverityWarn:
		return 2
	case domain.SeverityInfo:
		return 1
	default:
		return 0
	}
}
