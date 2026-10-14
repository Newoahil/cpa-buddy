// Package evaluate turns raw quota snapshots into a report plus the alerts that
// should be delivered.
//
// The engine is deliberately pure: it performs no IO, writes no logs and reads
// the clock only from the now argument. That makes every rule table-testable
// and keeps the caller in charge of persistence and delivery.
package evaluate

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/calendar"
	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// Fixed remediation strings. The engine never executes anything; it only tells
// the reader what a human should do next.
const (
	adviceOAuth   = "前往 CPA 重新完成 OAuth 登录"
	adviceOllama  = "更新 Ollama 的 __Secure-session cookie"
	adviceExhaust = "建议暂停使用该单个凭证直到额度刷新"
	adviceSuspect = "先排查网络与上游可用性，暂不建议停用凭证"
	adviceRecover = "仅表示所列观测恢复；容量以当前各 scope 为准，不自动操作"
)

// reauthAdvice maps a provider to the ONE remediation that applies to it.
//
// The two must never be concatenated: the __Secure-session cookie is an Ollama
// implementation detail, and telling a Codex or Claude reader to update it is
// both wrong and unactionable.
func reauthAdvice(p domain.ProviderKind) string {
	if p == domain.ProviderOllama {
		return adviceOllama
	}
	return adviceOAuth
}

// dropResetThreshold is the percentage-point drop that counts as a quota reset
// even when we have no reliable reset timestamp to compare against.
const dropResetThreshold = 20.0

// RequestStats is the optional request-sample window used by the anomaly branch
// of rule (e). The caller is responsible for aggregating it over
// config.AnomalyWindow; the engine only applies the count/rate thresholds.
type RequestStats struct {
	Window time.Duration
	Total  int
	Failed int
}

// Engine holds the configuration and calendar used for evaluation.
type Engine struct {
	cfg config.Config
	cal *calendar.Calendar
}

// New builds an Engine. cal may be nil, in which case the configured location
// (or time.Local) is used lazily.
func New(cfg config.Config, cal *calendar.Calendar) *Engine {
	return &Engine{cfg: cfg, cal: cal}
}

// Evaluate is the MVP entry point: no request samples, so rule (e) only uses
// the consecutive-failure branch.
func (e *Engine) Evaluate(prev *state.State, snaps []domain.QuotaSnapshot, now time.Time) (domain.Report, []domain.Alert, *state.State) {
	return e.EvaluateWithSamples(prev, snaps, nil, now)
}

// EvaluateWithSamples evaluates with per-credential request statistics.
// samples is keyed by domain.Credential.Key; a missing entry means "no sample".
func (e *Engine) EvaluateWithSamples(prev *state.State, snaps []domain.QuotaSnapshot, samples map[string]RequestStats, now time.Time) (domain.Report, []domain.Alert, *state.State) {
	if prev == nil {
		prev = state.Empty()
	}
	next := &state.State{
		Bootstrapped: true,
		UpdatedAt:    now,
		Credentials:  cloneCredentials(prev.Credentials),
		Pending:      clonePending(prev.Pending),
	}
	bootstrap := !prev.Bootstrapped

	results := make([]credResult, 0, len(snaps))
	for _, snap := range snaps {
		key := snap.Credential.Key
		if key == "" {
			// Without a stable key we cannot dedup or persist safely. Skip it
			// rather than collapse every anonymous credential onto "".
			continue
		}
		rec := prev.Credentials[key]
		updated, newState, credAlerts, reportSnap := e.evaluateCredential(rec, snap, samples, now)
		next.Credentials[key] = updated
		if bootstrap {
			credAlerts = nil
		}
		results = append(results, credResult{
			key:      key,
			provider: snap.Credential.Provider,
			cred:     snap.Credential,
			snap:     reportSnap,
			rec:      updated,
			state:    newState,
			alerts:   credAlerts,
		})
	}

	var alerts []domain.Alert
	for _, r := range results {
		alerts = append(alerts, r.alerts...)
	}

	report, notes, degraded := e.buildReport(results, now)
	report.Degraded = degraded
	report.Notes = notes
	if bootstrap {
		alerts = append(alerts, e.bootstrapAlert(results, now))
	}
	return report, alerts, next
}

// evaluateCredential applies rules (a)-(e) to one credential and returns the
// updated record, its projected state, any per-credential alerts and the
// snapshot that should appear in the report (with stale windows preserved).
//
// Attribution is driven by snapshot.Failure, which the collector decides from
// the HTTP status and payload shape. Error text is only consulted as a narrow,
// explicit supplement; a timeout can therefore never be re-read as "quota
// exhausted" just because a gateway error body happens to mention quota.
func (e *Engine) evaluateCredential(rec state.CredentialRecord, snap domain.QuotaSnapshot, samples map[string]RequestStats, now time.Time) (state.CredentialRecord, domain.CredentialState, []domain.Alert, domain.QuotaSnapshot) {
	out := cloneRecord(rec)
	snap.Windows = normalizedWindows(snap.Windows)
	if _, _, measured := snap.WorstUsedPercent(); snap.OK && !measured {
		// An empty success envelope cannot restore authentication or collection
		// evidence: the shared snapshot contract requires a usable measurement.
		snap.OK, snap.Failure = false, domain.FailureParse
		snap.Err = "成功响应未提供可用额度测量"
	}
	reportSnap := snap

	// Seed the orthogonal dimensions, tolerating records written before the
	// split by deriving them from the projected state.
	prevQuota := previousQuota(rec)
	prevCred := previousCredential(rec)
	prevFresh := previousFreshness(rec)
	out.Quota = prevQuota
	out.Credential = prevCred
	out.Freshness = prevFresh
	if rec.ReachedNotice || quotaRank(prevQuota) >= quotaRank(state.QuotaNotice) {
		out.ReachedNotice = true
	}

	failure, evidence := effectiveFailure(snap)
	if !snap.OK && failure == domain.FailureUnsupported {
		out.ConsecutiveFailures = 0
		out.CapabilityUnavailable = true
		reportSnap = preserveStaleWindows(snap, rec)
		// Missing capability is not recovery. Retain auth and account evidence,
		// but never present a previous observation as a fresh measurement.
		if reportSnap.Stale || prevFresh == state.Stale || !rec.LastSuccessAt.IsZero() || out.Quota != state.QuotaUnknown {
			out.Freshness = state.Stale
			reportSnap.Stale = true
		}
		out.State = out.Project()
		reportSnap.Err = "额度能力不可用（unsupported），不能据此判断网络、认证或额度耗尽。"
		return out, out.State, nil, reportSnap
	}

	var alerts []domain.Alert
	var resetFired bool
	if snap.OK {
		out.CapabilityUnavailable = false
		out.ConsecutiveFailures = 0
		out.LastSuccessAt = now
		// A collector may have already stamped the real observation instant
		// (the OMC source reads OMC's observed_at_ms, not the cycle clock). Only
		// fill it in when it is absent, so a reader is never shown a stale
		// reading dated now. The persisted record still advances to the cycle
		// time, preserving the existing staleness rules.
		if snap.LastSuccessAt.IsZero() {
			snap.LastSuccessAt = now
		}
		reportSnap = snap

		// Recompute explicit account applicability and independently evaluate
		// every scope/window. Unknown applicability stays visible as numbers.
		var scopedAlerts []domain.Alert
		out.Quota, scopedAlerts, resetFired = e.evaluateScopes(&out, rec, snap, now)
		alerts = append(alerts, scopedAlerts...)
		alerts = append(alerts, recoverRejections(&out, snap, now)...)
		if hasAccountRejection(out) {
			// A successful local query is not evidence against an unresolved
			// account rejection. Only an account measurement clears that entry.
			out.Quota = state.QuotaExhausted
		}

		// Credential dimension: a parseable read proves the credential works,
		// unless the collector explicitly reported an auth rejection.
		if failure == domain.FailureAuth {
			out.Credential = state.CredInvalid
		} else {
			out.Credential = state.CredValid
		}
		out.Freshness = state.Fresh

	} else {
		// Auth, quota and control-plane results interrupt a network-failure
		// sequence. Only transport/parse/unclassified attempts count below.
		out.ConsecutiveFailures = 0
		reportSnap = preserveStaleWindows(snap, rec)

		// Quota dimension is never changed by a failure: a temporary outage
		// must not erase the fact that the account was recently near its limit.
		out.Quota = prevQuota

		switch failure {
		case domain.FailureAuth:
			// Confirmed credential rejection. It must survive later timeouts.
			out.Credential = state.CredInvalid
			// Not a data-age problem: keep the previous freshness.
			out.Freshness = prevFresh
		case domain.FailureQuota:
			var rejectionAlerts []domain.Alert
			rejectionAlerts = e.recordRejection(&out, snap, evidence, now)
			alerts = append(alerts, rejectionAlerts...)
			if snap.FailureScope == domain.ScopeAccount {
				out.Quota = state.QuotaExhausted
				if out.Scopes == nil {
					out.Scopes = map[string]state.ScopeRecord{}
				}
				key := scopeKey(domain.QuotaWindow{Scope: domain.ScopeAccount})
				sr := out.Scopes[key]
				sr.Scope, sr.Level = domain.ScopeAccount, state.QuotaExhausted
				out.Scopes[key] = sr
			}
		case domain.FailureControlPlane:
			// A control-plane failure belongs to CPA, never to this credential.
			// The provider-level Error carries it instead.
			out.Freshness = prevFresh
		case domain.FailureTransport, domain.FailureParse, domain.FailureNone:
			out.ConsecutiveFailures = rec.ConsecutiveFailures + 1
			// transport / parse / unclassified: never a verdict
			// on the credential. These only accumulate failure counts.
			if out.ConsecutiveFailures >= e.anomalyConsecutive() || e.samplesAnomalous(samples[snap.Credential.Key]) {
				if out.Credential != state.CredInvalid {
					out.Credential = state.CredSuspect
				}
			}
			if out.ConsecutiveFailures >= e.staleAfterFailures() {
				out.Freshness = state.Stale
			}
		}
	}

	// Recompute ReachedNotice from the new quota level.
	if quotaRank(out.Quota) >= quotaRank(state.QuotaNotice) {
		out.ReachedNotice = true
	}

	// ---- credential dimension ---------------------------------------------
	if out.Credential != prevCred {
		switch out.Credential {
		case state.CredInvalid:
			alerts = append(alerts, e.invalidAlert(snap.Credential, evidence, now))
		case state.CredSuspect:
			alerts = append(alerts, e.suspectAlert(snap.Credential, out, reportSnap, samples[snap.Credential.Key], now))
		}
	}

	// ---- freshness dimension ----------------------------------------------
	// A confirmed auth failure is not a data-freshness problem, and a
	// control-plane failure is not the credential's fault, so only the plain
	// branch advances staleness.
	becameStale := out.Freshness == state.Stale && prevFresh != state.Stale
	if becameStale && !(failure == domain.FailureAuth || failure == domain.FailureControlPlane) {
		suspectJustRaised := out.Credential == state.CredSuspect && prevCred != state.CredSuspect
		if suspectJustRaised {
			// Both bars crossed in the same round: emit only the more severe
			// (suspect) alert so one incident produces one notification.
		} else {
			alerts = append(alerts, staleAlert(snap.Credential, out, reportSnap, now, e.loc()))
		}
	}

	// ---- recovery: any dimension improving notifies once -------------------
	var recovered []string
	if snap.OK && !resetFired && !hasAccountRejection(rec) && out.Quota == state.QuotaHealthy && quotaRank(prevQuota) >= quotaRank(state.QuotaNotice) {
		recovered = append(recovered, "额度用量已回落")
	}
	if out.Credential == state.CredValid && (prevCred == state.CredInvalid || prevCred == state.CredSuspect) {
		if prevCred == state.CredInvalid {
			recovered = append(recovered, "凭证已重新可用")
		} else {
			recovered = append(recovered, "凭证异常已解除")
		}
	}
	if out.Freshness == state.Fresh && prevFresh == state.Stale {
		recovered = append(recovered, "额度数据已恢复更新")
	}
	if len(recovered) > 0 {
		alerts = append(alerts, e.recoveredAlert(snap.Credential, recovered, now))
	}

	out.State = out.Project()
	return out, out.State, alerts, reportSnap
}

// quotaLevelFromWindows returns the most severe quota level across all
// reported windows (rule a). Any single window crossing is enough to raise it.
func quotaLevelFromWindows(windows []domain.QuotaWindow, th config.Thresholds) (state.QuotaLevel, []domain.QuotaWindow) {
	worst := state.QuotaUnknown
	var crossed []domain.QuotaWindow
	reported := false
	for _, w := range windows {
		if w.UsedPercent == nil {
			continue
		}
		reported = true
		pct := *w.UsedPercent
		switch {
		case pct >= th.Urgent:
			crossed = append(crossed, w)
			worst = state.QuotaExhausted
		case pct >= th.Warn:
			crossed = append(crossed, w)
			if quotaRank(state.QuotaWarning) > quotaRank(worst) {
				worst = state.QuotaWarning
			}
		case pct >= th.Notice:
			crossed = append(crossed, w)
			if quotaRank(state.QuotaNotice) > quotaRank(worst) {
				worst = state.QuotaNotice
			}
		}
	}
	if !reported {
		return state.QuotaUnknown, nil
	}
	if worst == state.QuotaUnknown {
		worst = state.QuotaHealthy
	}
	return worst, crossed
}

// ---------------------------------------------------------------------------
// Alert builders
// ---------------------------------------------------------------------------

func (e *Engine) invalidAlert(cred domain.Credential, evidence string, now time.Time) domain.Alert {
	advice := adviceOAuth
	providerNote := "CPA"
	if cred.Provider == domain.ProviderOllama {
		advice = adviceOllama
		providerNote = "Ollama"
	}
	facts := []string{"来源: " + providerNote}
	if evidence != "" {
		facts = append(facts, "证据: "+evidence)
	}
	return domain.Alert{
		Kind:       domain.AlertCredential,
		Severity:   domain.SeverityUrgent,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      "凭证失效",
		Detail:     cred.Label() + " 出现明确的认证失败证据，已判定为失效。",
		Facts:      facts,
		Advice:     advice,
		OccurredAt: now,
	}
}

func (e *Engine) exhaustedAlert(cred domain.Credential, snap domain.QuotaSnapshot, evidence string, now time.Time) domain.Alert {
	facts := crossedFacts(snap.Windows, e.loc())
	if evidence != "" {
		facts = append([]string{"证据: " + evidence}, facts...)
	}
	return domain.Alert{
		Kind:       domain.AlertQuotaExhausted,
		Scope:      domain.ScopeAccount,
		Severity:   domain.SeverityUrgent,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      "额度已耗尽",
		Detail:     cred.Label() + " 的额度窗口已达到上限。",
		Facts:      facts,
		Advice:     adviceExhaust,
		OccurredAt: now,
	}
}

func (e *Engine) thresholdAlert(cred domain.Credential, snap domain.QuotaSnapshot, level state.QuotaLevel, now time.Time) domain.Alert {
	sev := domain.SeverityInfo
	title := "额度接近上限"
	if level == state.QuotaWarning {
		sev = domain.SeverityWarn
		title = "额度告警"
	}
	if level == state.QuotaExhausted {
		sev = domain.SeverityUrgent
		title = "额度窗口达到上限"
	}
	return domain.Alert{
		Kind:       domain.AlertQuotaThreshold,
		Severity:   sev,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      title,
		Detail:     cred.Label() + " 有额度窗口越过阈值。",
		Facts:      crossedFacts(snap.Windows, e.loc()),
		OccurredAt: now,
	}
}

func (e *Engine) suspectAlert(cred domain.Credential, rec state.CredentialRecord, snap domain.QuotaSnapshot, stats RequestStats, now time.Time) domain.Alert {
	facts := []string{
		"连续失败: " + strconv.Itoa(rec.ConsecutiveFailures) + " 次",
		"失败原因: " + sanitize(snap.Err),
	}
	if !rec.LastSuccessAt.IsZero() {
		facts = append(facts, "最后成功: "+displayTime(rec.LastSuccessAt, e.loc()))
	}
	if stats.Total > 0 {
		facts = append(facts, "样本失败率: "+strconv.Itoa(stats.Failed)+"/"+strconv.Itoa(stats.Total))
	}
	return domain.Alert{
		Kind:       domain.AlertSuspect,
		Severity:   domain.SeverityWarn,
		Evidence:   domain.EvidenceSuspected,
		Credential: cred,
		Title:      "凭证异常（原因未确认）",
		Detail:     "候选原因：网络抖动、上游拥塞、CPA 侧超时。尚无证据表明凭证过期或额度耗尽。",
		Facts:      facts,
		Advice:     adviceSuspect,
		OccurredAt: now,
	}
}

func staleAlert(cred domain.Credential, rec state.CredentialRecord, snap domain.QuotaSnapshot, now time.Time, loc *time.Location) domain.Alert {
	facts := []string{"失败原因: " + sanitize(snap.Err)}
	if rec.LastSuccessAt.IsZero() {
		facts = append(facts, "最后成功: 从未成功")
	} else {
		facts = append(facts, "最后成功: "+displayTime(rec.LastSuccessAt, loc))
	}
	return domain.Alert{
		Kind:       domain.AlertStale,
		Severity:   domain.SeverityWarn,
		Evidence:   domain.EvidenceSuspected,
		Credential: cred,
		Title:      "额度数据已过期",
		Detail:     cred.Label() + " 连续多次无法读取额度，当前数值为上一次的旧值，不代表 0%。",
		Facts:      facts,
		OccurredAt: now,
	}
}

func (e *Engine) recoveredAlert(cred domain.Credential, recovered []string, now time.Time) domain.Alert {
	return domain.Alert{
		Kind:       domain.AlertRecovered,
		Severity:   domain.SeverityInfo,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      "观测状态已恢复",
		Detail:     cred.Label() + " 已恢复以下观测：" + strings.Join(recovered, "；") + "。",
		Advice:     adviceRecover,
		OccurredAt: now,
	}
}

func resetAlert(cred domain.Credential, w domain.QuotaWindow, now time.Time, loc *time.Location) domain.Alert {
	facts := []string{"窗口: " + w.DisplayLabel()}
	facts = append(facts, windowFact(w, loc))
	return domain.Alert{
		Kind:       domain.AlertQuotaReset,
		Severity:   domain.SeverityInfo,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      "额度已刷新",
		Detail:     cred.Label() + " 的窗口 " + w.DisplayLabel() + " 已刷新，额度重新可用。",
		Facts:      facts,
		OccurredAt: now,
	}
}

// bootstrapAlert is rule (f): one summary instead of per-credential noise.
func (e *Engine) bootstrapAlert(results []credResult, now time.Time) domain.Alert {
	var invalid, tight, healthy, limited, other int
	for _, r := range results {
		switch r.state {
		case domain.StateInvalid:
			invalid++
		case domain.StateExhausted, domain.StateWarning, domain.StateNotice:
			tight++
		case domain.StateHealthy:
			healthy++
		case domain.StateLimited:
			limited++
		default:
			other++
		}
	}
	detail := "启动汇总：" + strconv.Itoa(invalid) + " 个失效、" +
		strconv.Itoa(tight) + " 个额度告急、" + strconv.Itoa(healthy) + " 个正常"
	if other > 0 {
		detail += "、" + strconv.Itoa(other) + " 个数据异常"
	}
	if limited > 0 {
		detail += "、" + strconv.Itoa(limited) + " 个范围受限（局部额度，不代表全账号健康）"
	}
	sev := domain.SeverityInfo
	ev := domain.EvidenceUnknown
	if invalid > 0 || tight > 0 {
		sev = domain.SeverityWarn
		ev = domain.EvidenceConfirmed
	}
	return domain.Alert{
		Kind:       domain.AlertBootstrap,
		Severity:   sev,
		Evidence:   ev,
		Title:      "首次启动汇总",
		Detail:     detail,
		Facts:      []string{"本轮未对单个凭证重复告警，后续仅状态变化时通知。"},
		OccurredAt: now,
	}
}

// ---------------------------------------------------------------------------
// Report and recommendations
// ---------------------------------------------------------------------------

type credResult struct {
	key      string
	provider domain.ProviderKind
	cred     domain.Credential
	snap     domain.QuotaSnapshot
	rec      state.CredentialRecord
	state    domain.CredentialState
	alerts   []domain.Alert
}

// bestPick is the safest usable credential of a provider: the one whose worst
// window is the lowest among fresh, healthy credentials.
type bestPick struct {
	ok      bool
	pct     float64
	name    string
	windows []domain.QuotaWindow
}

func (e *Engine) buildReport(results []credResult, now time.Time) (domain.Report, []string, bool) {
	byProvider := map[domain.ProviderKind][]credResult{}
	for _, r := range results {
		byProvider[r.provider] = append(byProvider[r.provider], r)
	}
	providers := make([]domain.ProviderKind, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i] < providers[j] })

	rep := domain.Report{GeneratedAt: now}
	rep.Holiday = e.calendar().Context(now)

	var notes []string
	degraded := false

	for _, p := range providers {
		rs := byProvider[p]
		pr := domain.ProviderReport{
			Provider:  p,
			Total:     len(rs),
			States:    map[string]domain.CredentialState{},
			Snapshots: make([]domain.QuotaSnapshot, 0, len(rs)),
		}
		worst := domain.StateHealthy
		var staleLabels []string
		// Identical scope summaries are collapsed: three credentials of the
		// same provider produced the same long sentence three times, which
		// buried the one line that differed.
		scoped := newGroupedNotes()
		for _, r := range rs {
			if r.snap.Failure == domain.FailureUnsupported {
				degraded = true
				scoped.add("额度能力不可用（unsupported），本次缺测不产生新的网络、认证或耗尽判定；已有凭证证据保留，额度证据仅为旧值。", r.cred.Label())
			}
			if r.snap.OK {
				if summary := scopedSummary(r.rec.Scopes); summary != "" {
					scoped.add(summary, r.cred.Label())
				}
			}
			for _, rejection := range sortedRejections(r.rec) {
				scoped.add("存在 "+rejectionLabel(rejection)+" 额度拒绝记录，尚无对应范围的成功查询。", r.cred.Label())
			}
			pr.Snapshots = append(pr.Snapshots, r.snap)
			pr.States[r.key] = r.state
			if r.state == domain.StateHealthy {
				pr.Healthy++
			}
			switch domain.ClassOf(r.state) {
			case domain.ClassNormal:
				pr.Normal++
			case domain.ClassLimited:
				pr.Limited++
			default:
				pr.Abnormal++
			}
			if worstRank(r.state) > worstRank(worst) {
				worst = r.state
			}
			if r.snap.Stale {
				staleLabels = append(staleLabels, r.cred.Label())
			}
			if r.snap.Failure == domain.FailureControlPlane {
				pr.Error = "CPA 控制面故障，无法读取凭证额度：" + sanitize(r.snap.Err)
			}
		}
		notes = append(notes, scoped.lines()...)
		pr.WorstState = worst

		pick := pickBest(rs)
		if pick.ok {
			pr.BestWindows = append(pr.BestWindows, pick.windows...)
		}
		recommendation := e.recommend(pr, rep.Holiday, now, pick)

		if len(staleLabels) > 0 {
			degraded = true
			notes = append(notes, string(p)+"：以下凭证本轮缺测，额度数据已过期；保留的旧读数或拒绝证据未参与容量建议："+strings.Join(staleLabels, "、"))
		}
		if pr.Error != "" {
			degraded = true
			notes = append(notes, string(p)+"："+pr.Error)
		}

		rep.Providers = append(rep.Providers, pr)
		rep.Recommendations = append(rep.Recommendations, recommendation)
	}
	return rep, notes, degraded
}

// groupedNotes collapses repeated wording: one sentence, all the credentials
// it applies to. Emitting the same paragraph once per credential is what made
// a three-credential provider produce three identical advice lines.
type groupedNotes struct {
	order []string
	byMsg map[string][]string
}

func newGroupedNotes() *groupedNotes {
	return &groupedNotes{byMsg: map[string][]string{}}
}

func (g *groupedNotes) add(message, label string) {
	if message == "" {
		return
	}
	if _, seen := g.byMsg[message]; !seen {
		g.order = append(g.order, message)
	}
	for _, existing := range g.byMsg[message] {
		if existing == label {
			return
		}
	}
	g.byMsg[message] = append(g.byMsg[message], label)
}

func (g *groupedNotes) lines() []string {
	out := make([]string, 0, len(g.order))
	for _, message := range g.order {
		out = append(out, strings.Join(g.byMsg[message], "、")+"："+message)
	}
	return out
}

// pickBest selects the safest fresh, healthy credential and returns the full
// window set of that single credential.
//
// Windows from different accounts must never be merged: the per-window minimum
// across accounts can invent a combination no real credential has.
func pickBest(rs []credResult) bestPick {
	var pick bestPick
	for _, r := range rs {
		if r.snap.Stale || !r.snap.OK {
			continue
		}
		if r.rec.Credential != state.CredValid || r.rec.Freshness == state.Stale {
			continue
		}
		if len(r.rec.Rejections) > 0 {
			continue
		}
		// Only a complete, explicitly account-scoped set supports broad advice.
		// Model/group evidence remains visible in snapshots and scoped notes.
		safe := len(r.snap.Windows) > 0
		for _, w := range r.snap.Windows {
			if w.Scope != domain.ScopeAccount || w.UsedPercent == nil {
				safe = false
			}
		}
		if !safe {
			continue
		}
		val, name, ok := worstWindowOf(r.snap.Windows)
		if !ok {
			continue
		}
		if !pick.ok || val < pick.pct {
			pick = bestPick{ok: true, pct: val, name: name, windows: append([]domain.QuotaWindow(nil), r.snap.Windows...)}
		}
	}
	return pick
}

// worstWindowOf returns the highest reported usage and its window name.
func worstWindowOf(windows []domain.QuotaWindow) (float64, string, bool) {
	var worst float64
	var name string
	ok := false
	for _, w := range windows {
		if w.UsedPercent == nil {
			continue
		}
		if !ok || *w.UsedPercent > worst {
			worst, name, ok = *w.UsedPercent, w.Name, true
		}
	}
	return worst, name, ok
}

// recommend implements rule (h). Advice is provider-level only; it never names
// an agent configuration.
func (e *Engine) recommend(pr domain.ProviderReport, holiday domain.HolidayContext, now time.Time, pick bestPick) domain.Recommendation {
	th := e.thresholdsFor(pr.Provider)

	// A confirmed-dead credential dominates: re-auth first, everything else is
	// secondary until it is resolved.
	//
	// The failing credential is named explicitly. A provider-level "失效" that
	// does not say which credential died reads as if every credential in the
	// channel were dead, including the ones that just reported a real number.
	if invalid := invalidEvidence(pr); len(invalid) > 0 {
		return domain.Recommendation{
			Provider:  pr.Provider,
			Direction: domain.DirectionReauth,
			Reason: "失效凭证：" + strings.Join(invalid, "、") +
				"；建议先" + reauthAdvice(pr.Provider) +
				"。其余凭证的额度读数不受影响。",
		}
	}

	// Without a fresh, healthy credential there is no safe capacity to advise
	// on. Never say "use more" off stale or unreadable numbers.
	if !pick.ok {
		// Credentials that reach the same conclusion are grouped, so a
		// three-credential provider gets one sentence naming three credentials
		// instead of the same paragraph three times.
		summaries := newGroupedNotes()
		for _, snap := range pr.Snapshots {
			if !snap.OK || snap.Stale {
				continue
			}
			groups := map[string]state.ScopeRecord{}
			for _, w := range snap.Windows {
				key := scopeKey(w)
				s := groups[key]
				s.Scope, s.ScopeID = w.Scope, w.ScopeID
				level, _ := quotaLevelFromWindows([]domain.QuotaWindow{w}, th)
				if quotaRank(level) > quotaRank(s.Level) {
					s.Level = level
				}
				groups[key] = s
			}
			summaries.add(scopedSummary(groups), snap.Credential.Label())
		}
		if lines := summaries.lines(); len(lines) > 0 {
			return domain.Recommendation{Provider: pr.Provider, Direction: domain.DirectionSteady, Reason: strings.Join(lines, "；")}
		}
		return domain.Recommendation{
			Provider:  pr.Provider,
			Direction: domain.DirectionSteady,
			Reason:    "暂无新鲜且健康的额度数据，维持现状。",
		}
	}

	// Rule (h): a long holiday may tilt advice toward using spare capacity, but
	// it must never loosen alert thresholds. If capacity is already tight we
	// keep easing off.
	if pick.pct >= th.Notice {
		return domain.Recommendation{
			Provider:  pr.Provider,
			Direction: domain.DirectionEaseOff,
			Reason:    "所选单个凭证的 " + pick.name + " 窗口已用 " + pct(pick.pct) + "，达到通知阈值，建议放缓该凭证的使用。",
		}
	}

	if holiday.HolidayRunLength >= 2 {
		if reset, window := resetsBeforeReturn(pick.windows, holiday, now); reset {
			return domain.Recommendation{
				Provider:  pr.Provider,
				Direction: domain.DirectionUseMore,
				Reason:    "所选单个凭证的 " + pick.name + " 窗口已用 " + pct(pick.pct) + "，" + window + " 将在复工前刷新，闲置额度会被浪费。",
			}
		}
	}

	anomaly := false
	for _, st := range pr.States {
		if st == domain.StateSuspect || st == domain.StateStale {
			anomaly = true
		}
	}

	if pick.pct < 50 && !anomaly {
		return domain.Recommendation{
			Provider:  pr.Provider,
			Direction: domain.DirectionUseMore,
			Reason:    "所选单个凭证的 " + pick.name + " 窗口已用 " + pct(pick.pct) + "，该凭证已报告窗口余量充足，可继续使用。",
		}
	}

	return domain.Recommendation{
		Provider:  pr.Provider,
		Direction: domain.DirectionSteady,
		Reason:    "所选单个凭证的 " + pick.name + " 窗口已用 " + pct(pick.pct) + "，其已报告窗口暂无明显余量压力。",
	}
}

// invalidEvidence lists the credentials this provider projected to
// StateInvalid, each with the evidence that produced the verdict.
//
// This is the visible half of the attribution: the projection already refuses
// to call an exhausted credential invalid, but a reader cannot verify that
// unless the report says which credential the auth evidence came from.
func invalidEvidence(pr domain.ProviderReport) []string {
	var out []string
	for _, snap := range pr.Snapshots {
		if pr.States[snap.Credential.Key] != domain.StateInvalid {
			continue
		}
		reason := "此前已确认认证失败"
		if snap.Failure == domain.FailureAuth {
			reason = domain.FailureReason(domain.FailureAuth)
		}
		out = append(out, snap.Credential.Label()+"（"+reason+"）")
	}
	return out
}

// resetsBeforeReturn reports whether any window of the chosen credential resets
// before the next working day. This is the evidence behind the holiday advice.
func resetsBeforeReturn(windows []domain.QuotaWindow, holiday domain.HolidayContext, now time.Time) (bool, string) {
	if holiday.DaysToNextWork <= 0 {
		return false, ""
	}
	deadline := now.AddDate(0, 0, holiday.DaysToNextWork)
	for _, w := range windows {
		if w.ResetAt == nil {
			continue
		}
		if w.ResetAt.Before(deadline) {
			return true, w.Name
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (e *Engine) calendar() *calendar.Calendar {
	if e.cal != nil {
		return e.cal
	}
	return calendar.New(e.cfg.Location)
}

// loc is the display zone for reader-facing timestamps inside alerts.
func (e *Engine) loc() *time.Location {
	if e.cfg.Location != nil {
		return e.cfg.Location
	}
	return time.Local
}

func (e *Engine) thresholdsFor(p domain.ProviderKind) config.Thresholds {
	th := e.cfg.ThresholdsFor(p)
	if th.Notice <= 0 && th.Warn <= 0 && th.Urgent <= 0 {
		return config.Thresholds{Notice: 90, Warn: 95, Urgent: 100}
	}
	return th
}

func (e *Engine) staleAfterFailures() int {
	if e.cfg.StaleAfterFailures > 0 {
		return e.cfg.StaleAfterFailures
	}
	return 2
}

func (e *Engine) anomalyConsecutive() int {
	if e.cfg.AnomalyConsecutive > 0 {
		return e.cfg.AnomalyConsecutive
	}
	return 3
}

func (e *Engine) anomalyMinRequests() int {
	if e.cfg.AnomalyMinRequests > 0 {
		return e.cfg.AnomalyMinRequests
	}
	return 5
}

func (e *Engine) anomalyFailureRate() float64 {
	if e.cfg.AnomalyFailureRate > 0 {
		return e.cfg.AnomalyFailureRate
	}
	return 0.2
}

func (e *Engine) samplesAnomalous(s RequestStats) bool {
	if s.Total < e.anomalyMinRequests() {
		return false
	}
	rate := float64(s.Failed) / float64(s.Total)
	return rate >= e.anomalyFailureRate()
}

// quotaRank orders the quota dimension for comparisons.
func quotaRank(q state.QuotaLevel) int {
	switch q {
	case state.QuotaHealthy:
		return 1
	case state.QuotaNotice:
		return 2
	case state.QuotaWarning:
		return 3
	case state.QuotaExhausted:
		return 4
	default:
		return 0
	}
}

// previousQuota recovers the quota dimension, falling back to the projected
// state for records written before the dimension split.
func previousQuota(rec state.CredentialRecord) state.QuotaLevel {
	if hasAccountRejection(rec) {
		return state.QuotaExhausted
	}
	// Scope-aware files are authoritative over older credential-wide summary
	// fields (which used to be contaminated by local/unknown rejections).
	if len(rec.Scopes) > 0 {
		q := state.QuotaUnknown
		for _, s := range rec.Scopes {
			if s.Scope == domain.ScopeAccount && quotaRank(s.Level) > quotaRank(q) {
				q = s.Level
			}
		}
		return q
	}
	// Old files persisted a cross-window worst value without applicability.
	// It is not evidence of account exhaustion after the scope migration.
	if rec.Quota == state.QuotaExhausted || (rec.Quota == "" && rec.State == domain.StateExhausted) {
		account := false
		for _, s := range rec.Scopes {
			if s.Scope == domain.ScopeAccount && s.Level == state.QuotaExhausted {
				account = true
			}
		}
		if !account {
			return state.QuotaUnknown
		}
	}
	if rec.Quota != "" {
		return rec.Quota
	}
	switch rec.State {
	case domain.StateExhausted:
		return state.QuotaExhausted
	case domain.StateWarning:
		return state.QuotaWarning
	case domain.StateNotice:
		return state.QuotaNotice
	case domain.StateHealthy:
		return state.QuotaHealthy
	default:
		return state.QuotaUnknown
	}
}

func previousCredential(rec state.CredentialRecord) state.CredentialHealth {
	if rec.Credential != "" {
		return rec.Credential
	}
	switch rec.State {
	case domain.StateInvalid:
		return state.CredInvalid
	case domain.StateSuspect:
		return state.CredSuspect
	default:
		return state.CredValid
	}
}

func previousFreshness(rec state.CredentialRecord) state.Freshness {
	if rec.Freshness != "" {
		return rec.Freshness
	}
	if rec.State == domain.StateStale {
		return state.Stale
	}
	return state.Fresh
}

// worstRank orders projected states from best to worst for aggregation.
func worstRank(s domain.CredentialState) int {
	switch s {
	case domain.StateHealthy:
		return 0
	case domain.StateUnknown:
		return 1
	case domain.StateLimited:
		return 2
	case domain.StateNotice:
		return 3
	case domain.StateWarning:
		return 4
	case domain.StateStale:
		return 5
	case domain.StateSuspect:
		return 6
	case domain.StateExhausted:
		return 7
	case domain.StateInvalid:
		return 8
	default:
		return 1
	}
}

func crossedFacts(windows []domain.QuotaWindow, loc *time.Location) []string {
	facts := make([]string, 0, len(windows))
	for _, w := range windows {
		if w.UsedPercent == nil {
			continue
		}
		facts = append(facts, windowFact(w, loc))
	}
	return facts
}

// displayTime renders an instant in the configured zone. Alert facts are shown
// verbatim in chat, so they follow the same rule as every other user-facing
// timestamp: the reader's wall clock, never the upstream's.
func displayTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "未知"
	}
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// windowFact is reader-facing evidence, so it uses the display label and the
// readable scope. The internal Name/ScopeID stay in the snapshot for dedup and
// persistence but are never spelled out to a person.
func windowFact(w domain.QuotaWindow, loc *time.Location) string {
	fact := "[" + w.ScopeText() + "] " + w.DisplayLabel() + " 已用 " + pct(*w.UsedPercent)
	if w.ResetAt != nil {
		fact += "，刷新时间 " + displayTime(*w.ResetAt, loc)
	} else if w.ResetText != "" {
		fact += "，刷新 " + w.ResetText
	}
	return fact
}

func pct(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "%"
}

// preserveStaleWindows reconstructs the last known percentages so a stale
// report is not mistaken for an empty/zero reading. It marks the snapshot stale
// and stamps the real last-success time, so every surface can distinguish the
// numbers from a fresh reading and exclude them from capacity advice.
func preserveStaleWindows(snap domain.QuotaSnapshot, rec state.CredentialRecord) domain.QuotaSnapshot {
	out := snap
	out.Confidence = domain.ConfidenceUnknown
	out.LastSuccessAt = rec.LastSuccessAt
	if len(rec.Scopes) > 0 {
		out.Windows = nil
		keys := make([]string, 0, len(rec.Scopes))
		for k := range rec.Scopes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := rec.Scopes[k]
			names := make([]string, 0, len(s.Windows))
			for name := range s.Windows {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				out.Windows = append(out.Windows, s.Windows[name].Window)
			}
		}
		out.Stale = len(out.Windows) > 0
		return out
	}
	if len(rec.LastUsedPercent) == 0 {
		// Nothing to preserve: leave it as a plain failure, not a stale value.
		out.Windows = nil
		return out
	}
	out.Stale = true
	names := make([]string, 0, len(rec.LastUsedPercent))
	for name := range rec.LastUsedPercent {
		names = append(names, name)
	}
	sort.Strings(names)
	out.Windows = make([]domain.QuotaWindow, 0, len(names))
	for _, name := range names {
		v := rec.LastUsedPercent[name]
		w := domain.QuotaWindow{Name: name, UsedPercent: &v, Scope: domain.ScopeUnknown}
		if rt, ok := rec.LastResetAt[name]; ok && !rt.IsZero() {
			t := rt
			w.ResetAt = &t
		}
		out.Windows = append(out.Windows, w)
	}
	return out
}

func cloneCredentials(in map[string]state.CredentialRecord) map[string]state.CredentialRecord {
	out := make(map[string]state.CredentialRecord, len(in))
	for k, v := range in {
		out[k] = cloneRecord(v)
	}
	return out
}

func cloneRecord(in state.CredentialRecord) state.CredentialRecord {
	out := in
	if in.Rejections != nil {
		out.Rejections = make(map[string]state.QuotaRejection, len(in.Rejections))
		for k, v := range in.Rejections {
			out.Rejections[k] = v
		}
	}
	if in.Scopes != nil {
		out.Scopes = make(map[string]state.ScopeRecord, len(in.Scopes))
		for k, s := range in.Scopes {
			s.Windows = make(map[string]state.ScopeWindowRecord, len(in.Scopes[k].Windows))
			for name, w := range in.Scopes[k].Windows {
				s.Windows[name] = w
			}
			out.Scopes[k] = s
		}
	}
	if in.LastUsedPercent != nil {
		out.LastUsedPercent = make(map[string]float64, len(in.LastUsedPercent))
		for k, v := range in.LastUsedPercent {
			out.LastUsedPercent[k] = v
		}
	}
	if in.LastResetAt != nil {
		out.LastResetAt = make(map[string]time.Time, len(in.LastResetAt))
		for k, v := range in.LastResetAt {
			out.LastResetAt[k] = v
		}
	}
	return out
}

func clonePending(in map[string][]domain.Alert) map[string][]domain.Alert {
	if in == nil {
		return nil
	}
	out := make(map[string][]domain.Alert, len(in))
	for k, v := range in {
		cp := make([]domain.Alert, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// ---------------------------------------------------------------------------
// Failure attribution (rule e)
// ---------------------------------------------------------------------------

// effectiveFailure resolves the structured failure kind. snapshot.Failure set
// by the collector is authoritative. Only when it is empty do we consult the
// credential status field and a narrow, explicit phrase list as a supplement;
// a bare "quota" is never matched, because gateway errors routinely mention it
// while actually being transport failures.
func effectiveFailure(snap domain.QuotaSnapshot) (domain.FailureKind, string) {
	if snap.OK {
		return domain.FailureNone, ""
	}
	if snap.Failure != domain.FailureNone {
		switch snap.Failure {
		case domain.FailureAuth:
			return domain.FailureAuth, "采集层判定认证失败"
		case domain.FailureQuota:
			return domain.FailureQuota, "采集层判定额度耗尽"
		default:
			return snap.Failure, ""
		}
	}
	if hit, ev := authFromStatus(snap.Credential.Status); hit {
		return domain.FailureAuth, ev
	}
	if hit, ev := quotaFromStatus(snap.Credential.Status); hit {
		return domain.FailureQuota, ev
	}
	if hit, ev := authFromText(snap.Err); hit {
		return domain.FailureAuth, ev
	}
	if hit, ev := quotaFromText(snap.Err); hit {
		return domain.FailureQuota, ev
	}
	return domain.FailureNone, ""
}

// authStatuses are credential statuses that name an authentication verdict.
var authStatuses = []string{"unauthorized", "invalid", "expired", "revoked"}
var quotaStatuses = []string{"quota_exceeded", "quota exhausted", "exhausted"}

func authFromStatus(status string) (bool, string) {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return false, ""
	}
	for _, needle := range authStatuses {
		if strings.Contains(s, needle) {
			return true, "status=" + needle
		}
	}
	return false, ""
}

func quotaFromStatus(status string) (bool, string) {
	s := strings.ToLower(strings.TrimSpace(status))
	for _, needle := range quotaStatuses {
		if strings.Contains(s, needle) {
			return true, "status=" + needle
		}
	}
	return false, ""
}

// authPhrases are explicit authentication markers. They do not include generic
// words that appear in unrelated failures.
var authPhrases = []string{
	"401",
	"unauthorized",
	"invalid_grant",
	"invalid grant",
	"token expired",
	"token has expired",
	"credential revoked",
	"revoked",
	"authentication failed",
}

// quotaPhrases are the only phrases that prove an upstream quota rejection.
// A bare "quota" is deliberately absent.
var quotaPhrases = []string{
	"quota_exceeded",
	"quota exceeded",
	"insufficient_quota",
	"insufficient quota",
	"exceeded your current quota",
	"quota exhausted",
	"billing hard limit",
}

func authFromText(errText string) (bool, string) {
	t := strings.ToLower(strings.TrimSpace(errText))
	if t == "" {
		return false, ""
	}
	for _, p := range authPhrases {
		if strings.Contains(t, p) {
			return true, p
		}
	}
	return false, ""
}

func quotaFromText(errText string) (bool, string) {
	t := strings.ToLower(strings.TrimSpace(errText))
	if t == "" {
		return false, ""
	}
	for _, p := range quotaPhrases {
		if strings.Contains(t, p) {
			return true, p
		}
	}
	return false, ""
}

var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	tokenRE = regexp.MustCompile(`[A-Za-z0-9_\-\.]{24,}`)
)

// sanitize keeps a failure reason readable while removing anything that looks
// like an email or an opaque credential. Alerts end up in chat channels, so we
// never forward a raw upstream string unredacted.
func sanitize(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未提供"
	}
	s = emailRE.ReplaceAllString(s, "[redacted-email]")
	s = tokenRE.ReplaceAllString(s, "[redacted-secret]")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
