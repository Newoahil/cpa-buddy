// Package domain holds the shared vocabulary of the CPA quota watcher.
//
// Every other package depends on these types and must not redefine them.
package domain

import (
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ProviderKind identifies an upstream family tracked by the watcher.
type ProviderKind string

const (
	ProviderCodex       ProviderKind = "codex"
	ProviderClaude      ProviderKind = "claude"
	ProviderAntigravity ProviderKind = "antigravity"
	ProviderGeminiCLI   ProviderKind = "gemini-cli"
	ProviderOllama      ProviderKind = "ollama"
)

// Source records where a quota number came from.
type Source string

const (
	// SourceCPA is the official CPA v8 management API.
	SourceCPA   Source = "cpa-v8"
	SourceCPAV0 Source = "cpa-v0"
	// SourceOMC is quota read from Oh My CPA's read-only MCP endpoint. OMC is
	// the only party actively refreshing upstream quota when it is enabled.
	SourceOMC Source = "omc"
	// SourceOllamaWeb is the unofficial ollama.com settings page scrape.
	SourceOllamaWeb Source = "ollama-web"
)

// Confidence separates authoritative numbers from derived ones.
//
// ConfidenceReported values came straight from an upstream response.
// ConfidenceEstimated values were derived locally and must be labelled as such
// in every user-facing surface. ConfidenceUnknown means we have no number at
// all: never render it as 0% or 100%.
type Confidence string

const (
	ConfidenceReported  Confidence = "reported"
	ConfidenceEstimated Confidence = "estimated"
	ConfidenceUnknown   Confidence = "unknown"
)

// EvidenceLevel grades how firmly a diagnosis is supported.
type EvidenceLevel string

const (
	EvidenceConfirmed EvidenceLevel = "confirmed"
	EvidenceSuspected EvidenceLevel = "suspected"
	EvidenceUnknown   EvidenceLevel = "unknown"
)

// Credential is one tracked account/credential of a provider.
//
// Key is the stable identity used for state transitions and dedup. It must be
// stable across restarts and must never contain a secret or a directly
// reusable upstream handle: it is persisted to the state file and rendered
// into JSON APIs, so it has to be an opaque derivation (e.g. a hash of the
// auth index), not the auth index itself.
type Credential struct {
	Key      string       `json:"key"`
	Provider ProviderKind `json:"provider"`
	Alias    string       `json:"alias"`
	ShortID  string       `json:"short_id"`
	// AuthIndex is the live CPA handle. It is an internal transport field:
	// never serialise it, never render it, never persist it.
	AuthIndex string `json:"-"`
	Status    string `json:"status,omitempty"`
	Disabled  bool   `json:"disabled"`
	// Unavailable mirrors the CPA runtime flag, not our own judgement.
	Unavailable bool `json:"unavailable"`
	// NextRetryAfter is CPA's own "usable again at" instant for the whole
	// credential. Nil when CPA omitted it or sent something unparseable.
	NextRetryAfter *time.Time `json:"next_retry_after,omitempty"`
	// Cooldowns are CPA's active cooldown entries for this credential. They
	// carry only whitelisted, non-free-text fields; CPA's status_message is
	// never read into any field of this type.
	Cooldowns []Cooldown `json:"cooldowns,omitempty"`
}

// Cooldown is one CPA cooldown entry (typically after an upstream HTTP 429).
type Cooldown struct {
	// Scope is CPA's own scope string ("model", "credential", ...).
	Scope string `json:"scope"`
	// ModelKey is the model the cooldown applies to when Scope is "model".
	ModelKey string `json:"model_key,omitempty"`
	// Reason is CPA's whitelisted reason identifier (e.g. "quota").
	Reason string `json:"reason,omitempty"`
	// RetryAt is when CPA expects to retry; nil when unknown.
	RetryAt *time.Time `json:"retry_at,omitempty"`
	// HTTPStatus is the upstream status that caused the cooldown. Only a value
	// in 400-599 is kept; 0 means unknown.
	HTTPStatus int `json:"http_status,omitempty"`
}

// Label renders a credential for humans without leaking email or secrets.
func (c Credential) Label() string {
	switch {
	case c.Alias != "" && c.ShortID != "":
		return c.Alias + " · " + c.ShortID
	case c.Alias != "":
		return c.Alias
	case c.ShortID != "":
		return string(c.Provider) + " · " + c.ShortID
	default:
		return string(c.Provider)
	}
}

// Name is the credential's human name without the masked short id: the alias
// when there is one, else Label(). It is for titles a reader sees.
func (c Credential) Name() string {
	if a := strings.TrimSpace(c.Alias); a != "" {
		return TrimProviderPrefix(a, c.Provider)
	}
	return c.Label()
}

// providerAliasPrefixes are the file-name prefixes CPA puts in front of an
// account alias (e.g. "claude-External"). The card already shows the brand,
// so the prefix only repeats it.
var providerAliasPrefixes = map[ProviderKind][]string{
	ProviderClaude:      {"claude-", "anthropic-"},
	ProviderCodex:       {"codex-"},
	ProviderAntigravity: {"antigravity-"},
	ProviderGeminiCLI:   {"gemini-cli-", "gemini-"},
	ProviderOllama:      {"ollama-"},
}

// TrimProviderPrefix drops the provider prefix from an alias for display. An
// alias that is only the prefix, or does not start with it, is kept as is.
func TrimProviderPrefix(alias string, p ProviderKind) string {
	lower := strings.ToLower(alias)
	for _, pre := range providerAliasPrefixes[p] {
		if strings.HasPrefix(lower, pre) && len(alias) > len(pre) {
			return alias[len(pre):]
		}
	}
	return alias
}

// QuotaScope is the explicitly reported applicability of a quota. Missing or
// unrecognised values mean unknown, never account. Names cannot establish scope.
type QuotaScope string

const (
	ScopeAccount QuotaScope = "account"
	ScopeModel   QuotaScope = "model"
	ScopeGroup   QuotaScope = "group"
	ScopeUnknown QuotaScope = "unknown"
)

func (s QuotaScope) Normalized() QuotaScope {
	switch s {
	case ScopeAccount, ScopeModel, ScopeGroup:
		return s
	default:
		return ScopeUnknown
	}
}

// QuotaWindow is one rolling usage window reported by an upstream.
//
// UsedPercent and ResetAt are pointers on purpose: nil means "not reported",
// which is different from zero.
type QuotaWindow struct {
	Scope QuotaScope `json:"scope"`
	// ScopeID is a safe model/group identifier, never an account ID or secret.
	// Account is local to the enclosing credential and needs no ScopeID.
	ScopeID string `json:"scope_id,omitempty"`
	Name    string `json:"name"`
	// Label is a purely presentational, human-readable name for this window.
	// It is assigned by the upstream parser and must never be used for dedup,
	// persistence keys or scope identity: Name and ScopeID keep those roles.
	// Renderers prefer Label so an internal path like
	// "antigravity/groups/Gemini%20Models#1/buckets/gemini-weekly/weekly#1"
	// never reaches a reader.
	Label       string     `json:"label,omitempty"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	ResetText   string     `json:"reset_text,omitempty"`
	// WindowSeconds is the rolling window length the upstream reported for
	// this window. Zero means not reported: the period is then unknown and
	// must not be guessed from the field's position in the payload.
	WindowSeconds int64 `json:"window_seconds,omitempty"`
	// LimitReached is an explicit upstream "you are at the limit" marker. It
	// is independent of UsedPercent and never used to synthesise one: a marker
	// without a number stays a marker.
	LimitReached bool `json:"limit_reached,omitempty"`
	// RemainingAmount is an upstream-reported remaining count, verbatim. Its
	// unit is whatever the window's own label names (e.g. a Gemini tokenType);
	// we never convert it or attach a unit the payload did not state.
	RemainingAmount string `json:"remaining_amount,omitempty"`
}

// ExtraUsage is pay-as-you-go usage reported alongside the quota windows.
//
// The amounts are the upstream's own "credits". The payload states no currency
// anywhere, so these are never converted, rounded into money or rendered with a
// currency symbol; they are shown in the unit the upstream used.
type ExtraUsage struct {
	Enabled      bool     `json:"enabled"`
	UsedCredits  *float64 `json:"used_credits,omitempty"`
	MonthlyLimit *float64 `json:"monthly_limit,omitempty"`
	// UsedPercent is the upstream's own utilization for this budget, not a
	// ratio we computed from the two amounts above.
	UsedPercent *float64 `json:"used_percent,omitempty"`
}

// Reportable is true when there is something concrete to show. A disabled or
// empty extra-usage block is not rendered at all rather than as zeros.
func (e *ExtraUsage) Reportable() bool {
	return e != nil && e.Enabled && (e.UsedCredits != nil || e.MonthlyLimit != nil || e.UsedPercent != nil)
}

// ScopeLabel is the machine-facing scope tuple ("group:some/internal/path").
// It is used for dedup keys and logs; it is NOT safe for user-facing text
// because ScopeID can be an internal, percent-encoded path. Use ScopeText.
func (w QuotaWindow) ScopeLabel() string {
	s := string(w.Scope.Normalized())
	if w.ScopeID != "" {
		s += ":" + w.ScopeID
	}
	return s
}

// DisplayLabel is the reader-facing window name: the parser-assigned Label
// when present, otherwise a best-effort humanisation of the internal Name.
func (w QuotaWindow) DisplayLabel() string {
	if s := strings.TrimSpace(w.Label); s != "" {
		return s
	}
	if s := HumanizeIdentifier(w.Name); s != "" {
		return s
	}
	return "未命名窗口"
}

// ScopeText describes applicability in reader-facing words.
func (w QuotaWindow) ScopeText() string { return ScopeText(w.Scope, w.ScopeID) }

// ScopeText renders an applicability tuple for humans. Unknown stays visibly
// unknown: it must never read as account-wide.
func ScopeText(scope QuotaScope, scopeID string) string {
	id := HumanizeIdentifier(scopeID)
	switch scope.Normalized() {
	case ScopeAccount:
		return "账号"
	case ScopeModel:
		if id != "" {
			return "模型 " + id
		}
		return "模型"
	case ScopeGroup:
		if id != "" {
			return "分组 " + id
		}
		return "分组"
	default:
		if id != "" {
			return "范围未知 " + id
		}
		return "范围未知"
	}
}

// occurrenceSuffix is the "#n" disambiguator the parsers append to duplicate
// identities. It carries no meaning for a reader.
var occurrenceSuffix = regexp.MustCompile(`#\d+$`)

// structuralSegments are internal path scaffolding: provider prefixes and
// container names that say nothing a reader needs.
var structuralSegments = map[string]bool{
	"codex": true, "claude": true, "antigravity": true,
	"gemini-cli": true, "ollama": true,
	"groups": true, "buckets": true, "models": true, "tokens": true,
	"limits": true, "additional": true, "weekly_scoped": true,
}

// HumanizeIdentifier turns an internal window/scope identifier into readable
// text: it percent-decodes each segment, drops the "#n" occurrence suffix and
// the structural path segments, and joins what is left.
//
// It is a display fallback only. Callers must not feed the result back into
// any key: the transformation is lossy on purpose.
func HumanizeIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = occurrenceSuffix.ReplaceAllString(p, "")
		if decoded, err := url.PathUnescape(p); err == nil {
			p = decoded
		}
		p = strings.TrimSpace(p)
		if p == "" || structuralSegments[strings.ToLower(p)] {
			continue
		}
		out = append(out, strings.ReplaceAll(p, "_", " "))
	}
	if len(out) == 0 {
		// Everything was structural: fall back to the last decoded segment so
		// we never render an empty name.
		last := occurrenceSuffix.ReplaceAllString(parts[len(parts)-1], "")
		if decoded, err := url.PathUnescape(last); err == nil {
			last = decoded
		}
		return strings.ReplaceAll(strings.TrimSpace(last), "_", " ")
	}
	return strings.Join(out, " · ")
}

// FailureKind classifies why a fetch failed.
//
// Attribution depends on this being decided by the collector, which sees the
// HTTP status and error shape, instead of being guessed later from error text.
// A transport timeout and an exhausted quota must never collapse into the same
// string match.
type FailureKind string

const (
	FailureNone FailureKind = ""
	// FailureAuth is confirmed credential rejection (upstream 401/invalid
	// grant/revoked token/expired session cookie).
	FailureAuth FailureKind = "auth"
	// FailureQuota is a confirmed quota rejection reported as an error.
	FailureQuota FailureKind = "quota"
	// FailureTransport is a timeout, connection or upstream-gateway failure.
	// It is never evidence about the credential itself.
	FailureTransport FailureKind = "transport"
	// FailureUnsupported means the provider has no quota source at all.
	FailureUnsupported FailureKind = "unsupported"
	// FailureParse means we reached the upstream but could not understand the
	// payload. It is a defect signal for us, not a verdict on the credential.
	FailureParse FailureKind = "parse"
	// FailureControlPlane is a failure of CPA itself (bad management key,
	// management API unreachable). It must be attributed to the control plane,
	// never to an individual credential.
	FailureControlPlane FailureKind = "control_plane"
)

// FailureReason is the reader-facing explanation of a structured failure kind.
//
// It is derived from the classification the collector already made, never from
// the raw upstream error text, so it can be shown without leaking a payload.
func FailureReason(k FailureKind) string {
	switch k {
	case FailureAuth:
		return "认证被上游拒绝"
	case FailureQuota:
		return "上游返回额度拒绝"
	case FailureTransport:
		return "网络或上游不可达"
	case FailureParse:
		return "上游响应无法解析"
	case FailureControlPlane:
		return "CPA 控制面故障"
	case FailureUnsupported:
		return "额度能力不可用"
	default:
		return "原因未确认"
	}
}

// QuotaSnapshot is one collection attempt for one credential.
//
// OK=true requires at least one usable quota window. A response we could parse
// but that carried no quota numbers is a failure, not a healthy reading:
// otherwise an upstream redesign would look like a permanently healthy account.
type QuotaSnapshot struct {
	Credential Credential    `json:"credential"`
	Windows    []QuotaWindow `json:"windows,omitempty"`
	Plan       string        `json:"plan,omitempty"`
	Balance    string        `json:"balance,omitempty"`
	// ExtraUsage is the credential's pay-as-you-go budget when the upstream
	// reports one in the same response. It is real spend, so it is shown even
	// for an otherwise-folded healthy channel.
	ExtraUsage *ExtraUsage `json:"extra_usage,omitempty"`
	Source     Source      `json:"source"`
	Confidence Confidence  `json:"confidence"`
	FetchedAt  time.Time   `json:"fetched_at"`
	OK         bool        `json:"ok"`
	Failure    FailureKind `json:"failure,omitempty"`
	// FailureScope is required to attribute a quota rejection to the whole
	// credential. Unscoped rejections cannot prove account exhaustion.
	FailureScope   QuotaScope `json:"failure_scope,omitempty"`
	FailureScopeID string     `json:"failure_scope_id,omitempty"`
	Err            string     `json:"err,omitempty"`
	// Stale marks windows carried over from an older successful read so the
	// UI and cards can never present them as current.
	Stale bool `json:"stale,omitempty"`
	// LastSuccessAt is when these numbers were actually observed.
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	// Code is a short, displayable error code derived from an upstream HTTP
	// status (e.g. "401") or a known CPA error code (e.g. "CPA 200621"). It is
	// never response body text: it is a status number or a sanitised error
	// identifier, so it cannot carry a token or a secret. Empty means no code
	// was available and nothing is shown.
	Code string `json:"code,omitempty"`
}

// WorstUsedPercent returns the highest reported usage across windows.
// ok is false when no window reported a number.
func (s QuotaSnapshot) WorstUsedPercent() (value float64, window string, ok bool) {
	for _, w := range s.Windows {
		if w.UsedPercent == nil {
			continue
		}
		if !ok || *w.UsedPercent > value {
			value, window, ok = *w.UsedPercent, w.Name, true
		}
	}
	return value, window, ok
}

// CredentialState is the evaluated health of one credential.
type CredentialState string

const (
	StateHealthy CredentialState = "healthy"
	// StateLimited means only scoped capacity is known or a local scope is
	// constrained. It is not account health, account exhaustion or a parse error.
	StateLimited   CredentialState = "limited"
	StateNotice    CredentialState = "notice"
	StateWarning   CredentialState = "warning"
	StateExhausted CredentialState = "exhausted"
	// StateInvalid means the credential is confirmed broken (auth error).
	StateInvalid CredentialState = "invalid"
	// StateSuspect means repeated failures without a confirmed cause.
	StateSuspect CredentialState = "suspect"
	// StateStale means quota could not be read for several cycles.
	StateStale CredentialState = "stale"
	// StateUnknown means we never got a usable reading.
	StateUnknown CredentialState = "unknown"
)

// StateClass groups credential states into the three buckets a reader can act
// on. A binary healthy/unhealthy count is misleading: a provider whose only
// measurements are model- or group-scoped is never "healthy", yet it is also
// not broken, and counting it as unhealthy makes a channel with plenty of
// headroom look completely down.
type StateClass string

const (
	// ClassNormal has account-wide evidence of headroom.
	ClassNormal StateClass = "normal"
	// ClassLimited has real numbers but constrained coverage or pressure:
	// only scoped capacity is known, or usage crossed a notice/warning bar.
	ClassLimited StateClass = "limited"
	// ClassAbnormal needs attention: exhausted, invalid, suspect, stale or
	// never measured.
	ClassAbnormal StateClass = "abnormal"
)

// ClassOf maps a projected state onto its display bucket.
func ClassOf(s CredentialState) StateClass {
	switch s {
	case StateHealthy:
		return ClassNormal
	case StateLimited, StateNotice, StateWarning:
		return ClassLimited
	default:
		return ClassAbnormal
	}
}

// Severity drives notification urgency.
type Severity string

const (
	SeverityInfo   Severity = "info"
	SeverityWarn   Severity = "warn"
	SeverityUrgent Severity = "urgent"
)

// AlertKind enumerates the notification triggers agreed for the MVP.
type AlertKind string

const (
	AlertQuotaThreshold AlertKind = "quota_threshold"
	AlertQuotaExhausted AlertKind = "quota_exhausted"
	AlertCredential     AlertKind = "credential_invalid"
	AlertSuspect        AlertKind = "suspect_anomaly"
	AlertStale          AlertKind = "stale_data"
	AlertRecovered      AlertKind = "recovered"
	AlertQuotaReset     AlertKind = "quota_reset"
	AlertBootstrap      AlertKind = "bootstrap_summary"
)

// AlertRateLimited / AlertRateLimitCleared come from the cooldown watcher
// (internal/cooldown), which reads CPA's own cooldown data. They never pass
// through the evaluation engine.
const (
	AlertRateLimited      AlertKind = "rate_limited"
	AlertRateLimitCleared AlertKind = "rate_limit_cleared"
)

// Actionable-mode kinds. These are produced by the push policy
// (internal/notify/policy) rather than by the evaluation engine, except that
// AlertCredential doubles as the credential_invalid pending kind.
//
// AlertChannelUnavailable and AlertMonitorDown are channel-level: their
// Credential carries only the provider, because the condition is about the
// whole channel, not one account.
const (
	// AlertChannelUnavailable: every enabled account of a channel is unusable
	// and the earliest recovery lies beyond CHANNEL_UNAVAILABLE_AFTER.
	AlertChannelUnavailable AlertKind = "channel_unavailable"
	// AlertMonitorDown: a channel has produced no valid data for longer than
	// MONITOR_DOWN_AFTER. It replaces the immediate suspect/stale alerts.
	AlertMonitorDown AlertKind = "monitor_down"
	// AlertOMCOutage is reserved for the OMC fallback source (feat/omc-quota-source).
	// This branch has no FallbackCollector, so it is only ever used as a pending
	// item kind, never pushed immediately.
	AlertOMCOutage AlertKind = "omc_outage"
	// AlertQuietDigest is the merged message sent when quiet hours end, carrying
	// the still-unresolved events accumulated during quiet hours.
	AlertQuietDigest AlertKind = "quiet_digest"
	// AlertActionableSummary is the new scheduled digest card in actionable mode.
	AlertActionableSummary AlertKind = "actionable_summary"
)

// Fact line prefixes emitted by the cooldown watcher on AlertRateLimited and
// AlertRateLimitCleared alerts. Renderers parse these; the value follows the
// prefix after one space.
const (
	FactPrefixStatus   = "状态码:"
	FactPrefixRecovery = "预计恢复:"
	FactPrefixDuration = "持续:"
)

// CPABackoffCap is the longest cooldown CPA applies on its own after an
// upstream error. A cooldown whose recovery lies further away follows the
// upstream's quota reset, i.e. the account (or model) is used up. Shorter
// cooldowns are routine and recover by themselves.
const CPABackoffCap = 30 * time.Minute

// Alert is one emitted notification event.
type Alert struct {
	Scope      QuotaScope    `json:"scope,omitempty"`
	ScopeID    string        `json:"scope_id,omitempty"`
	Kind       AlertKind     `json:"kind"`
	Severity   Severity      `json:"severity"`
	Evidence   EvidenceLevel `json:"evidence"`
	Credential Credential    `json:"credential"`
	Title      string        `json:"title"`
	Detail     string        `json:"detail"`
	// Facts are short evidence lines shown verbatim to the reader.
	Facts []string `json:"facts,omitempty"`
	// Advice is the fixed remediation mapping; it never executes anything.
	Advice     string    `json:"advice,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Direction is the capacity advice for a provider.
type Direction string

const (
	DirectionUseMore Direction = "use_more"
	DirectionEaseOff Direction = "ease_off"
	DirectionSteady  Direction = "steady"
	DirectionReauth  Direction = "reauth"
)

// Recommendation is provider-level advice; it never names agent configs.
type Recommendation struct {
	Provider  ProviderKind `json:"provider"`
	Direction Direction    `json:"direction"`
	Reason    string       `json:"reason"`
}

// ProviderReport aggregates one provider for the summary card and status page.
//
// Healthy/Total are credential counts. We deliberately do not average usage
// percentages across accounts, because separate accounts are not additive.
//
// Healthy is retained for compatibility but must not be the headline number:
// a credential whose only measurements are model/group scoped projects to
// StateLimited and is therefore never counted healthy, which made providers
// with plenty of headroom render as "0/3". Normal/Limited/Abnormal is the
// display counting; it always sums to Total.
type ProviderReport struct {
	Provider   ProviderKind    `json:"provider"`
	Healthy    int             `json:"healthy"`
	Total      int             `json:"total"`
	Normal     int             `json:"normal"`
	Limited    int             `json:"limited"`
	Abnormal   int             `json:"abnormal"`
	WorstState CredentialState `json:"worst_state"`
	// BestWindows describes the safest single credential, not a cross-account
	// merge: mixing windows from different accounts can invent a headroom that
	// no real credential has.
	BestWindows []QuotaWindow              `json:"best_windows,omitempty"`
	Snapshots   []QuotaSnapshot            `json:"snapshots,omitempty"`
	States      map[string]CredentialState `json:"states,omitempty"`
	// Error is a provider-wide collection failure (e.g. the credential list
	// could not be read). Individual credential failures stay in Snapshots.
	Error string `json:"error,omitempty"`
}

// Report is the full evaluated picture at one point in time.
type Report struct {
	GeneratedAt     time.Time        `json:"generated_at"`
	Providers       []ProviderReport `json:"providers"`
	Recommendations []Recommendation `json:"recommendations"`
	// Holiday describes the calendar context used for recommendations.
	Holiday HolidayContext `json:"holiday"`
	// Degraded means part of this picture is missing or stale, so the report
	// must not be presented as a complete healthy view.
	Degraded bool `json:"degraded,omitempty"`
	// Notes carry degradation reasons in reader-facing wording.
	Notes []string `json:"notes,omitempty"`
	// RateLimits are the short rate-limit cooldowns (recovered before they
	// were worth an alert) seen since the previous digest. Only the scheduled
	// digest fills it.
	RateLimits []RateLimitTally `json:"rate_limits,omitempty"`
}

// RateLimitTally summarises short, un-alerted cooldown episodes of one
// credential since the last digest.
type RateLimitTally struct {
	Credential Credential    `json:"credential"`
	Count      int           `json:"count"`
	Longest    time.Duration `json:"longest"`
}

// HolidayContext is the calendar input to capacity advice.
type HolidayContext struct {
	Date           string `json:"date"`
	IsWorkday      bool   `json:"is_workday"`
	IsHoliday      bool   `json:"is_holiday"`
	Label          string `json:"label,omitempty"`
	DaysToNextWork int    `json:"days_to_next_workday"`
	// HolidayRunLength counts consecutive non-working days starting today.
	HolidayRunLength int `json:"holiday_run_length"`
}

// Message is a channel-agnostic outbound notification.
type Message struct {
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Alerts []Alert `json:"alerts,omitempty"`
	Report *Report `json:"report,omitempty"`
	Kind   string  `json:"kind"`
	// Freshness is a short, reader-facing note about how current the data is
	// ("实时", "缓存 · 约 3 分钟前"). The channel decides it, because only the
	// channel knows whether this particular answer was refreshed live; render
	// prints it verbatim and never infers it.
	Freshness string `json:"freshness,omitempty"`
	// Notice is a prominent warning shown above the body. It is reserved for
	// a genuine problem (no new data and the cache is past its useful life),
	// never for the routine "this answer came from cache" case.
	Notice string `json:"notice,omitempty"`
	// Detailed requests the per-window expansion used by a single-channel
	// query. The default compact form folds normal channels into one line.
	Detailed bool `json:"detailed,omitempty"`
	// Actionable, when set, selects the ALERT_MODE=actionable card view model.
	// Its absence means the message renders exactly as before (legacy). It is
	// the only place the new cards diverge, so the renderer can be polished
	// without touching the policy that produced the classification.
	Actionable *ActionableCard `json:"actionable,omitempty"`
}

// ActionableCard is the view model for the ALERT_MODE=actionable cards. It is a
// pure projection: the policy decides what belongs here, the renderer decides
// how it looks. Keeping it free of rendering concerns is what lets @designer
// change the card without touching the classification.
type ActionableCard struct {
	// HeaderColor is the Feishu header template ("red"/"orange"/"blue"/...).
	HeaderColor string
	// Conclusion is the one-line verdict shown under the title.
	Conclusion string
	// Pending lists the open pending items (docs/alert-noise-plan.md §4).
	Pending []PendingRow
	// Tight lists the channels whose best usable account is running low; it is
	// the reason a summary can be worth sending with no pending items.
	Tight []TightChannel
}

// PendingRow is one open pending item as a card shows it.
type PendingRow struct {
	Kind     AlertKind
	Provider ProviderKind
	Alias    string
	ShortID  string
	Since    time.Time
	Action   string
}

// TightChannel is one channel whose best usable account has a low remaining
// share in some account-level window.
type TightChannel struct {
	Provider ProviderKind
	// BestRemaining is the remaining percentage of the best usable account's
	// tightest account-level window. Nil means no account-level reading exists.
	BestRemaining *float64
	// ResetAt is when that binding window refreshes.
	ResetAt *time.Time
}
