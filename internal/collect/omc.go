package collect

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/ollama"
	"github.com/Newoahil/CPA-Manager/internal/omc"
)

// OMCQuotaLister reads the quota OMC has already collected. It is satisfied by
// *omc.Client and by test fakes.
type OMCQuotaLister interface {
	ListQuotas(ctx context.Context) ([]omc.QuotaItem, error)
}

// credentialLister is the slice of *cpa.Client the OMC collector needs: the
// credential list supplies identity and aliases, and it is a management call,
// not an upstream quota query.
type credentialLister interface {
	ListCredentials(ctx context.Context) ([]domain.Credential, error)
}

// OMCCollector implements domain.Collector by reading quota from OMC and
// associating it with CPA's credential list by auth_index.
//
// CPA remains the identity authority: the credential list (already polled every
// 60s by the cooldown watcher) gives each account its stable opaque Key and
// alias, so switching the quota source never resets state or degrades an alias
// into a masked email.
type OMCCollector struct {
	creds credentialLister
	omc   OMCQuotaLister
	now   func() time.Time
}

// NewOMCCollector builds an OMC-backed collector.
func NewOMCCollector(creds credentialLister, quotas OMCQuotaLister) *OMCCollector {
	return &OMCCollector{creds: creds, omc: quotas, now: time.Now}
}

// Name implements domain.Collector. It is "omc" so a collector-level failure is
// attributed to the same CPA-owned providers the direct collector owns.
func (c *OMCCollector) Name() string { return "omc" }

// Collect lists CPA credentials, reads OMC's quota, and projects each supported
// credential onto a domain.QuotaSnapshot.
//
// A credential OMC does not report (or a provider OMC cannot refresh, such as
// gemini-cli) becomes a non-OK unsupported snapshot: no number is invented, and
// the existing engine keeps the previous evidence while marking it stale. Only
// a failure of the credential list (control plane) or of the OMC call itself is
// a batch error.
func (c *OMCCollector) Collect(ctx context.Context) ([]domain.QuotaSnapshot, error) {
	creds, err := c.creds.ListCredentials(ctx)
	if err != nil {
		return []domain.QuotaSnapshot{}, fmt.Errorf("%w: list credentials: %w", ErrControlPlane, err)
	}
	items, err := c.omc.ListQuotas(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("collect omc: %w", ctxErr)
		}
		return nil, fmt.Errorf("collect omc: %w", err)
	}
	byIndex := make(map[string]omc.QuotaItem, len(items))
	for _, item := range items {
		if item.AuthIndex == "" {
			continue
		}
		if _, seen := byIndex[item.AuthIndex]; !seen {
			byIndex[item.AuthIndex] = item
		}
	}

	now := c.now()
	out := make([]domain.QuotaSnapshot, 0, len(creds))
	for _, cred := range creds {
		if cred.AuthIndex == "" || cred.Disabled {
			continue
		}
		if !omcSupportedProvider(cred.Provider) {
			continue
		}
		item, ok := byIndex[cred.AuthIndex]
		if !ok {
			out = append(out, unreportedSnapshot(cred, now))
			continue
		}
		out = append(out, mapOMCItem(cred, item, now))
	}
	return out, nil
}

// omcSupportedProvider is the set of CPA-owned providers OMC normalizes. Other
// providers (Ollama) are collected elsewhere; OMC-only families are ignored.
func omcSupportedProvider(p domain.ProviderKind) bool {
	switch p {
	case domain.ProviderCodex, domain.ProviderClaude, domain.ProviderAntigravity, domain.ProviderGeminiCLI:
		return true
	default:
		return false
	}
}

// unreportedSnapshot is the honest "OMC has no reading for this credential"
// result: unsupported, never a fabricated 0% or healthy number.
func unreportedSnapshot(cred domain.Credential, now time.Time) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential:   cred,
		Source:       domain.SourceOMC,
		Confidence:   domain.ConfidenceUnknown,
		FetchedAt:    now,
		OK:           false,
		Failure:      domain.FailureUnsupported,
		FailureScope: domain.ScopeUnknown,
		Err:          "OMC 未上报该凭据额度",
	}
}

// mapOMCItem projects one OMC quota onto the same semantics CPACollector
// produces, so alerts, history and evaluation are unchanged.
func mapOMCItem(cred domain.Credential, item omc.QuotaItem, now time.Time) domain.QuotaSnapshot {
	observed := observationTime(item.ObservedAtMS, now)
	snap := domain.QuotaSnapshot{
		Credential: cred,
		Source:     domain.SourceOMC,
		FetchedAt:  observed,
	}
	status := strings.ToLower(strings.TrimSpace(item.Status))
	if status == "error" {
		// OMC's own read failed. We do not know whether it was auth or
		// transport, and we never fabricate a verdict: a transport failure is
		// the non-committal kind. Numbers are dropped so the engine keeps the
		// previous evidence instead.
		snap.Confidence = domain.ConfidenceUnknown
		snap.Failure = domain.FailureTransport
		snap.FailureScope = domain.ScopeUnknown
		snap.Err = "OMC 采集该凭据额度失败"
		return snap
	}

	windows, plan := mapOMCWindows(cred.Provider, item)
	snap.Plan = plan
	if len(windows) == 0 {
		snap.Confidence = domain.ConfidenceUnknown
		snap.Failure = domain.FailureUnsupported
		snap.FailureScope = domain.ScopeUnknown
		snap.Err = unreportedReason(status)
		return snap
	}
	snap.Windows = windows
	snap.Confidence = domain.ConfidenceReported
	snap.OK = true
	// The observation time is when OMC actually read this, not when we fetched
	// its answer; a reader must never see a stale number dated now.
	snap.LastSuccessAt = observed
	if status == "stale" {
		// OMC says its reading aged out. The numbers were really observed
		// (hence observed), but every surface must present them as stale.
		snap.Stale = true
	}
	return snap
}

// observationTime converts OMC's epoch-millisecond observation into a UTC
// instant, falling back to the collection time only when OMC omitted it.
func observationTime(ms int64, now time.Time) time.Time {
	if ms <= 0 {
		return now
	}
	return time.UnixMilli(ms).UTC()
}

func unreportedReason(status string) string {
	switch status {
	case "unpublished":
		return "OMC 未上报额度窗口"
	case "idle", "loading":
		return "OMC 尚未完成该凭据的额度采集"
	default:
		return "OMC 未上报额度窗口"
	}
}

// mapOMCWindows converts OMC windows and picks a single plan label.
func mapOMCWindows(provider domain.ProviderKind, item omc.QuotaItem) ([]domain.QuotaWindow, string) {
	out := make([]domain.QuotaWindow, 0, len(item.Windows))
	seen := map[string]int{}
	for _, w := range item.Windows {
		dw := domain.QuotaWindow{
			Name:  omcWindowName(seen, provider, w),
			Label: strings.TrimSpace(w.Label),
		}
		dw.Scope, dw.ScopeID = omcScope(w)
		if w.UsedPercent != nil {
			v := *w.UsedPercent
			dw.UsedPercent = &v
		}
		if w.ResetAtMS != nil && *w.ResetAtMS > 0 {
			t := time.UnixMilli(*w.ResetAtMS).UTC()
			dw.ResetAt = &t
		}
		if w.PeriodHours != nil && *w.PeriodHours > 0 {
			// The domain stores the rolling-window length in seconds; OMC
			// reports hours. Round to the nearest second so a fractional hour
			// cannot silently truncate to zero.
			dw.WindowSeconds = int64(math.Round(*w.PeriodHours * 3600))
		}
		out = append(out, dw)
	}
	return out, planLabel(item.Plan)
}

// omcWindowName builds a stable, namespaced identity. OMC ids are unique per
// credential; a duplicate gets the same "#n" disambiguator the direct parser
// uses, so two same-id windows never collapse onto one history key.
func omcWindowName(seen map[string]int, provider domain.ProviderKind, w omc.Window) string {
	base := string(provider) + "/omc/"
	switch {
	case strings.TrimSpace(w.ID) != "":
		base += strings.TrimSpace(w.ID)
	case strings.TrimSpace(w.Kind) != "":
		base += strings.TrimSpace(w.Kind)
	default:
		base += "window"
	}
	seen[base]++
	if seen[base] == 1 {
		return base
	}
	return base + "#" + strconv.Itoa(seen[base])
}

// omcScope maps OMC's own scope vocabulary onto domain scopes. OMC's
// "standard" is account-wide; "model" and "group" carry an identifier; a
// "code_review" limit is a named group. Anything unrecognised stays unknown.
func omcScope(w omc.Window) (domain.QuotaScope, string) {
	scopeID := strings.TrimSpace(w.Model)
	if scopeID == "" {
		scopeID = strings.TrimSpace(w.ID)
	}
	switch strings.ToLower(strings.TrimSpace(w.Scope)) {
	case "standard":
		return domain.ScopeAccount, ""
	case "model":
		return domain.ScopeModel, url.PathEscape(scopeID)
	case "group":
		return domain.ScopeGroup, url.PathEscape(scopeID)
	case "code_review":
		return domain.ScopeGroup, "code_review"
	default:
		return domain.ScopeUnknown, ""
	}
}

// planLabel prefers OMC's human plan label (e.g. "Pro 200") and falls back to
// its provider key. It never guesses a plan the payload does not carry.
func planLabel(p *omc.Plan) string {
	if p == nil {
		return ""
	}
	if label := strings.TrimSpace(p.PlanLabel); label != "" {
		return label
	}
	return strings.TrimSpace(p.PlanType)
}

// FallbackCollector wraps an OMC collector with the direct CPA collector.
//
// Every cycle tries OMC first. A run of failures shorter than the threshold is
// surfaced as an error (so the report is degraded and the existing evidence
// ages), but once the threshold is reached the cycle transparently queries CPA
// directly. Recovery is automatic: as soon as OMC answers again the fallback is
// left and the direct path is no longer used.
type FallbackCollector struct {
	primary   domain.Collector
	fallback  domain.Collector
	threshold int
	log       *slog.Logger

	mu sync.Mutex
	// failures is the run of consecutive primary failures. It resets on any
	// successful primary read.
	failures int
	// outageStart is when the current failure run began, i.e. the first failure
	// of the run. It is zero when no outage is ongoing.
	outageStart time.Time
	// providers are the providers this primary speaks for. An OMC outage only
	// concerns them; other collectors' providers must never report one.
	providers map[domain.ProviderKind]bool

	now func() time.Time
}

// NewFallbackCollector builds the wrapper. A threshold below 1 is treated as 1,
// so misconfiguration cannot disable the fallback entirely.
func NewFallbackCollector(primary, fallback domain.Collector, threshold int, log *slog.Logger) *FallbackCollector {
	if log == nil {
		log = slog.Default()
	}
	if threshold < 1 {
		threshold = 1
	}
	return &FallbackCollector{primary: primary, fallback: fallback, threshold: threshold, log: log, now: time.Now}
}

// Name implements domain.Collector: the primary's name, so provider
// attribution stays the same whether or not the fallback is active.
func (f *FallbackCollector) Name() string { return f.primary.Name() }

// SetOutageProviders declares the providers this source speaks for, so an OMC
// outage is attributed only to those channels and never to Ollama or another
// independent collector.
func (f *FallbackCollector) SetOutageProviders(providers ...domain.ProviderKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.providers == nil {
		f.providers = map[domain.ProviderKind]bool{}
	}
	for _, p := range providers {
		f.providers[p] = true
	}
}

// OutageSince reports the start of an ongoing OMC outage for provider and
// whether one is active. It implements the policy's OutageSource: a run of
// consecutive failures (whether or not it has crossed the fallback threshold)
// is an interruption, and a provider this source does not speak for is never
// outaged.
func (f *FallbackCollector) OutageSince(provider domain.ProviderKind, _ time.Time) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures <= 0 || !f.providers[provider] {
		return time.Time{}, false
	}
	return f.outageStart, true
}

// OutageStart returns the start of the current outage, or the zero time when
// none is ongoing. The app persists it so the clock survives a restart.
func (f *FallbackCollector) OutageStart() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures <= 0 {
		return time.Time{}
	}
	return f.outageStart
}

// RestoreOutage seeds the outage start from persisted state. It only ever moves
// the start earlier, so a live run that began after a restart is bridged back to
// the original outage instead of restarting the clock. It is a no-op when no
// outage is active.
func (f *FallbackCollector) RestoreOutage(start time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures <= 0 || start.IsZero() {
		return
	}
	if f.outageStart.IsZero() || start.Before(f.outageStart) {
		f.outageStart = start
	}
}

// InFallback reports whether the last attempt reached the failure threshold.
// The app uses it to keep the fast poll from accelerating direct CPA queries.
func (f *FallbackCollector) InFallback() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures >= f.threshold
}

// Collect runs the primary collector and falls back to the secondary once the
// failure threshold is reached.
func (f *FallbackCollector) Collect(ctx context.Context) ([]domain.QuotaSnapshot, error) {
	snaps, err := f.primary.Collect(ctx)
	if err == nil {
		if f.reset() {
			f.log.Info("omc quota source recovered; leaving cpa fallback", "source", f.primary.Name())
		}
		return snaps, nil
	}
	// A cancelled caller context is not an OMC problem; do not count it and do
	// not issue a second (direct) collection on a context we already know is
	// cancelled.
	if ctx.Err() != nil {
		return nil, err
	}
	failures, active := f.recordFailure()
	if !active {
		f.log.Warn("omc quota read failed", "consecutive_failures", failures, "fallback_after", f.threshold)
		return nil, err
	}
	f.log.Error("omc quota read failed; falling back to querying cpa directly",
		"consecutive_failures", failures, "fallback_after", f.threshold)
	return f.fallback.Collect(ctx)
}

// recordFailure increments the counter and reports whether the threshold is met.
// The first failure of a run stamps the outage start.
func (f *FallbackCollector) recordFailure() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == 0 {
		f.outageStart = f.now()
	}
	f.failures++
	return f.failures, f.failures >= f.threshold
}

// reset clears the counter and the outage start, and reports whether the
// previous state was fallback (so "recovered" is logged exactly once, on the
// transition).
func (f *FallbackCollector) reset() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	was := f.failures >= f.threshold
	f.failures = 0
	f.outageStart = time.Time{}
	return was
}

// cpaProviders are the channels the CPA/OMC source speaks for. They share one
// primary collector, so an OMC outage is attributed to all of them and never to
// an unrelated collector such as Ollama.
var cpaProviders = []domain.ProviderKind{
	domain.ProviderCodex, domain.ProviderClaude, domain.ProviderAntigravity, domain.ProviderGeminiCLI,
}

// Build assembles the collectors for the configured quota source.
//
// When QUOTA_SOURCE=omc it returns a single OMC-backed collector wrapped in a
// fallback to the direct CPA collector, plus that wrapper so the composition
// root can install its InFallback predicate and OMC outage source on the app.
// Otherwise it returns the unchanged direct CPA collector and a nil fallback.
func Build(cfg config.Config, client *cpa.Client, log *slog.Logger) ([]domain.Collector, *FallbackCollector) {
	var fallback *FallbackCollector
	out := make([]domain.Collector, 0, 2)
	if cfg.QuotaSource == config.QuotaSourceOMC {
		primary := NewOMCCollector(client, omc.New(cfg.OMCMCPURL, cfg.OMCMCPKey, cfg.CPATimeout))
		fallback = NewFallbackCollector(primary, NewCPACollector(client), cfg.OMCFallbackAfter, log)
		fallback.SetOutageProviders(cpaProviders...)
		out = append(out, fallback)
	} else {
		out = append(out, NewCPACollector(client))
	}
	if len(cfg.OllamaAccounts) > 0 {
		out = append(out, NewOllamaCollector(ollama.New(cfg.OllamaTimeout), cfg.OllamaAccounts))
	}
	return out, fallback
}
