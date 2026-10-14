// Package render turns a domain.Message into channel-agnostic output: plain
// text for webhooks and logs, and a Feishu interactive-card structure.
//
// It is deliberately pure: it only reads the fields it is given and never
// touches upstream payloads. Two rules are enforced here rather than trusted
// to callers:
//
//   - Everything precise (numbers, window names, reset times, data source,
//     fetch time, evidence level) survives in both tones. The tone changes the
//     voice, not the facts.
//   - Secrets never reach the output. Credentials are printed through
//     domain.Credential.Label and a raw upstream error (Snapshot.Err) is never
//     rendered verbatim, so a token or cookie that leaked into a collector
//     cannot escape through a card.
//   - Freshness is never implied. A stale snapshot, a degraded report and an
//     estimated confidence are all labelled, and the "last success" time comes
//     from LastSuccessAt, not from the failed attempt's timestamp.
package render

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// KindQuery marks an on-demand status reply (the @Bot query path).
const KindQuery = "query"

// RefreshAction is the legacy value of the removed "刷新额度" button. Cards
// already in the wild still carry it, so the Feishu router recognises it and
// answers with a "button removed" toast. It is never emitted by this renderer.
const RefreshAction = "refresh_quota"

// Renderer renders messages in one tone.
//
// It is safe for concurrent use: it holds only immutable configuration. The
// time zone is only used to display times; the values themselves are never
// rewritten, so callers whose config carries a specific location may use
// WithLocation.
type Renderer struct {
	tone          string
	cpaURL        string
	loc           *time.Location
	ignoredGroups []string
	// charts enables the card's progress chart. It defaults to true; the
	// composition root can turn it off with CARD_CHARTS_ENABLED without a code
	// change, because a chart component is the one element whose client
	// rendering we cannot guarantee.
	charts bool
}

// WithCharts toggles the card's progress chart.
func (r *Renderer) WithCharts(enabled bool) *Renderer {
	r.charts = enabled
	return r
}

// WithLocation sets the location used to format timestamps. A nil location is
// ignored so the default (time.Local) is kept.
func (r *Renderer) WithLocation(loc *time.Location) *Renderer {
	if loc != nil {
		r.loc = loc
	}
	return r
}

// WithIgnoredGroups configures model/group names to be filtered out of notifications.
func (r *Renderer) WithIgnoredGroups(groups []string) *Renderer {
	r.ignoredGroups = groups
	return r
}

// Default titles. They are intentionally usable as-is; env vars only override.
var defaultTitles = map[string]string{
	string(domain.AlertQuotaThreshold):     "额度提醒",
	string(domain.AlertQuotaExhausted):     "额度告急",
	string(domain.AlertCredential):         "凭证失效",
	string(domain.AlertSuspect):            "异常疑似",
	string(domain.AlertStale):              "数据过期",
	string(domain.AlertRecovered):          "额度恢复",
	string(domain.AlertQuotaReset):         "额度刷新",
	string(domain.AlertBootstrap):          "额度日报",
	string(domain.AlertRateLimited):        "限流告警",
	string(domain.AlertRateLimitCleared):   "额度恢复",
	string(domain.AlertChannelUnavailable): "渠道不可用",
	string(domain.AlertMonitorDown):        "监控停摆",
	string(domain.AlertQuietDigest):        "免打扰结束",
	string(domain.AlertActionableSummary):  "额度汇总",
	KindQuery:                              "额度查询",
}

// New builds a renderer for the given tone (config.ToneCasual or
// config.ToneFormal; anything else falls back to casual).
//
// Title templates can be overridden without touching code:
//
//	NOTIFY_TITLE              generic fallback template
//	NOTIFY_TITLE_<KIND>       per-kind override, kind upper-cased
//	                          e.g. NOTIFY_TITLE_QUOTA_THRESHOLD=配额预警
//	NOTIFY_CPA_PAGE_URL       optional CPA admin link shown in advice
func New(tone string) *Renderer {
	var ignored []string
	if raw, set := os.LookupEnv("QUOTA_IGNORED_GROUPS"); set {
		if raw != "" {
			for _, p := range strings.Split(raw, ",") {
				if s := strings.TrimSpace(p); s != "" {
					ignored = append(ignored, s)
				}
			}
		}
	}
	r := &Renderer{
		tone:          tone,
		loc:           time.Local,
		ignoredGroups: ignored,
		// Charts are off unless a caller explicitly opts in with WithCharts:
		// the text lines already carry the exact numbers, and the chart is an
		// optional visual, not the default.
	}
	return r
}

// cpaPageURL is read per render so configuration can change without a rebuild
// and is simply absent when unset.
func (r *Renderer) cpaPageURL() string {
	return strings.TrimSpace(os.Getenv("NOTIFY_CPA_PAGE_URL"))
}

// Tone returns the active tone.
func (r *Renderer) Tone() string {
	if r.tone == config.ToneFormal {
		return config.ToneFormal
	}
	return config.ToneCasual
}

// Title exposes the resolved title so other channels (the webhook projection)
// can reuse exactly the same wording instead of re-implementing the override
// rules.
func (r *Renderer) Title(msg domain.Message) string { return r.titleFor(msg) }

// titleFor resolves the card/message title: per-kind env override, then generic
// env override, then the message's own title, then the built-in default.
func (r *Renderer) titleFor(msg domain.Message) string {
	kind := msg.Kind
	if kind == "" {
		kind = string(domain.AlertBootstrap)
	}
	if v := strings.TrimSpace(os.Getenv("NOTIFY_TITLE_" + strings.ToUpper(kind))); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("NOTIFY_TITLE")); v != "" {
		return v
	}
	if strings.TrimSpace(msg.Title) != "" {
		return msg.Title
	}
	if t, ok := defaultTitles[kind]; ok {
		return t
	}
	return "额度通知"
}

func (r *Renderer) formal() bool { return r.tone == config.ToneFormal }

func (r *Renderer) displayName(p domain.ProviderKind) string {
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
		if p == "" {
			return "未知"
		}
		return string(p)
	}
}

func stateLabel(s domain.CredentialState) string {
	switch s {
	case domain.StateHealthy:
		return "正常"
	case domain.StateLimited:
		return "范围受限（局部额度）"
	case domain.StateNotice:
		return "注意"
	case domain.StateWarning:
		return "告警"
	case domain.StateExhausted:
		return "耗尽"
	case domain.StateInvalid:
		return "失效"
	case domain.StateSuspect:
		return "疑似异常"
	case domain.StateStale:
		return "数据过期"
	case domain.StateUnknown:
		return "未知"
	default:
		if s == "" {
			return "未知"
		}
		return string(s)
	}
}

func evidenceLabel(e domain.EvidenceLevel) string {
	switch e {
	case domain.EvidenceConfirmed:
		return "确认"
	case domain.EvidenceSuspected:
		return "疑似"
	default:
		return "未知"
	}
}

func severityLabel(s domain.Severity) string {
	switch s {
	case domain.SeverityUrgent:
		return "紧急"
	case domain.SeverityWarn:
		return "警告"
	case domain.SeverityInfo:
		return "提示"
	default:
		if s == "" {
			return "提示"
		}
		return string(s)
	}
}

// location is the display time zone. Values are never rewritten; only the
// rendering of an instant changes.
func (r *Renderer) location() *time.Location {
	if r.loc == nil {
		return time.Local
	}
	return r.loc
}

func (r *Renderer) formatTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.In(r.location()).Format("2006-01-02 15:04")
}

// formatTimeHM returns the compact "15:04" representation in the configured zone.
func (r *Renderer) formatTimeHM(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.In(r.location()).Format("15:04")
}

// formatShort is the compact instant used inside a line. Like every other
// timestamp it is converted into the configured zone first: a wall clock with
// no zone is only readable if it is the reader's own wall clock.
func (r *Renderer) formatShort(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.In(r.location()).Format("01-02 15:04")
}

func percent(v *float64) string {
	if v == nil {
		return "未上报"
	}
	return fmt.Sprintf("%.1f%%", *v)
}

// resetText is a Renderer method rather than a package function precisely so
// it cannot be called without a time zone. The previous package-level version
// printed the upstream's own wall clock, which made a window reset appear
// eight hours away from the recovery time quoted in the same message.
func (r *Renderer) resetText(w domain.QuotaWindow) string {
	if w.ResetAt != nil {
		return r.formatShort(*w.ResetAt)
	}
	if w.ResetText != "" {
		return w.ResetText
	}
	return "未上报"
}

// windowLine renders one window with its exact evidence.
//
// It uses the display label and the readable scope: the internal Name/ScopeID
// (e.g. "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1")
// are dedup and persistence keys, not text for a person.
func (r *Renderer) windowLine(w domain.QuotaWindow) string {
	return fmt.Sprintf("[%s] %s 已用 %s（%s 刷新）", w.ScopeText(), w.DisplayLabel(), percent(w.UsedPercent), r.resetText(w))
}

// sourcesAndFetched aggregates provenance and freshness across a provider's
// snapshots.
//
// The returned time is the most recent *successful* observation, not the last
// attempt: a failed fetch's FetchedAt records when we failed, so showing it as
// the data time would present stale numbers as fresh.
func (r *Renderer) sourcesAndFetched(p domain.ProviderReport, fallback time.Time) (string, time.Time) {
	seen := map[string]bool{}
	var sources []string
	latest := time.Time{}
	for _, s := range p.Snapshots {
		if s.Source != "" && !seen[string(s.Source)] {
			seen[string(s.Source)] = true
			sources = append(sources, string(s.Source))
		}
		t := s.LastSuccessAt
		if t.IsZero() && s.OK {
			t = s.FetchedAt
		}
		if t.After(latest) {
			latest = t
		}
	}
	if len(sources) == 0 {
		sources = []string{"未知"}
	}
	if latest.IsZero() {
		latest = fallback
	}
	return strings.Join(sources, "+"), latest
}

// IsGroupIgnored tests whether a scopeGroup or window should be ignored based on ignoredGroups.
func (r *Renderer) IsGroupIgnored(w domain.QuotaWindow) bool {
	if len(r.ignoredGroups) == 0 {
		return false
	}
	if w.Scope.Normalized() != domain.ScopeGroup {
		return false
	}
	rawScopeID := strings.TrimSpace(w.ScopeID)
	humanizedID := strings.TrimSpace(domain.HumanizeIdentifier(rawScopeID))
	translated := strings.TrimSpace(humanizeScopeGroup(humanizedID))
	displayLabel := strings.TrimSpace(w.DisplayLabel())

	for _, ig := range r.ignoredGroups {
		ig = strings.TrimSpace(ig)
		if ig == "" {
			continue
		}
		if strings.EqualFold(rawScopeID, ig) ||
			strings.EqualFold(humanizedID, ig) ||
			strings.EqualFold(translated, ig) ||
			strings.EqualFold(displayLabel, ig) {
			return true
		}
		// Also check if humanizedID or translated contains the ignored group or vice-versa
		if strings.EqualFold(humanizedID, ig) || strings.EqualFold(translated, ig) {
			return true
		}
		// Match against lower-case contains for "Claude and GPT models" vs "Claude and GPT Models"
		lowerIG := strings.ToLower(ig)
		if lowerIG == "claude and gpt models" || lowerIG == "claude and gpt" || lowerIG == "claude / gpt 模型" || lowerIG == "claude 和 gpt 模型组" {
			if strings.Contains(strings.ToLower(rawScopeID), "claude%20and%20gpt") ||
				strings.Contains(strings.ToLower(rawScopeID), "claude and gpt") ||
				strings.Contains(strings.ToLower(humanizedID), "claude and gpt") ||
				strings.Contains(strings.ToLower(translated), "claude / gpt") ||
				strings.Contains(strings.ToLower(displayLabel), "claude 和 gpt") {
				return true
			}
		}
	}
	return false
}

// IsAlertIgnored tests whether an alert is for an ignored scope group.
func (r *Renderer) IsAlertIgnored(a domain.Alert) bool {
	if len(r.ignoredGroups) == 0 {
		return false
	}
	if a.Scope.Normalized() != domain.ScopeGroup {
		return false
	}
	wFake := domain.QuotaWindow{Scope: a.Scope, ScopeID: a.ScopeID}
	return r.IsGroupIgnored(wFake)
}

// FilterReport creates a filtered deep copy of a report where ignored groups and their windows are stripped.
func (r *Renderer) FilterReport(rep *domain.Report) *domain.Report {
	if rep == nil || len(r.ignoredGroups) == 0 {
		return rep
	}
	out := *rep
	out.Providers = make([]domain.ProviderReport, len(rep.Providers))
	for i, p := range rep.Providers {
		pCopy := p
		// Filter BestWindows
		var filteredBest []domain.QuotaWindow
		for _, w := range p.BestWindows {
			if !r.IsGroupIgnored(w) {
				filteredBest = append(filteredBest, w)
			}
		}
		pCopy.BestWindows = filteredBest

		// Filter Snapshots
		pCopy.Snapshots = make([]domain.QuotaSnapshot, len(p.Snapshots))
		for j, s := range p.Snapshots {
			sCopy := s
			var filteredWins []domain.QuotaWindow
			for _, w := range s.Windows {
				if !r.IsGroupIgnored(w) {
					filteredWins = append(filteredWins, w)
				}
			}
			sCopy.Windows = filteredWins
			pCopy.Snapshots[j] = sCopy
		}

		// Recompute Provider worst state and states if needed
		out.Providers[i] = pCopy
	}
	return &out
}

// FilterAlerts removes alerts associated with ignored scope groups.
func (r *Renderer) FilterAlerts(alerts []domain.Alert) []domain.Alert {
	if len(alerts) == 0 || len(r.ignoredGroups) == 0 {
		return alerts
	}
	var out []domain.Alert
	for _, a := range alerts {
		if !r.IsAlertIgnored(a) {
			out = append(out, a)
		}
	}
	return out
}

// FilterMessage returns a copy of Message with Report and Alerts filtered according to ignoredGroups.
func (r *Renderer) FilterMessage(msg domain.Message) domain.Message {
	if len(r.ignoredGroups) == 0 {
		return msg
	}
	out := msg
	out.Report = r.FilterReport(msg.Report)
	out.Alerts = r.FilterAlerts(msg.Alerts)
	return out
}
