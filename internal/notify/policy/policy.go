// Package policy decides what a round of evaluation results should do to a
// human: push it now, open or close a pending item, or drop it
// (docs/alert-noise-plan.md §6.1).
//
// It sits AFTER the evaluation engine and never changes a verdict: it only
// classifies the alerts evaluate already produced, plus two channel-level
// conditions (channel_unavailable, monitor_down) that can be read from the
// report's per-account state and last-success time. The engine's rules are left
// untouched.
//
// In legacy mode the policy is a pass-through: every alert is immediate and no
// pending item is ever created, which reproduces the old behaviour exactly.
package policy

import (
	"hash/fnv"
	"sort"
	"strconv"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// Mode selects between the new actionable behaviour and the legacy pass-through.
type Mode string

const (
	Actionable Mode = "actionable"
	Legacy     Mode = "legacy"
)

// ModeOf normalises a configured value. Anything that is not the explicit
// legacy switch is actionable, so a zero-value config still gets the new,
// quieter behaviour once the value is wired through.
func ModeOf(raw string) Mode {
	if raw == string(Legacy) {
		return Legacy
	}
	return Actionable
}

// OutageSource reports an ongoing OMC fallback outage for a provider.
//
// This branch has no FallbackCollector (feat/omc-quota-source is not merged), so
// the interface is reserved only: a nil OutageSource means an OMC outage is
// never detected. When that feature lands, wire its InFallback signal here;
// until then omc_outage never reaches a card. The comment is deliberate so the
// gap is not mistaken for dead code.
type OutageSource interface {
	// OutageSince returns the start of an ongoing OMC fallback outage for
	// provider and whether one is active at now.
	OutageSince(provider domain.ProviderKind, now time.Time) (time.Time, bool)
}

// Config is the policy's immutable configuration.
type Config struct {
	Mode                    Mode
	ChannelUnavailableAfter time.Duration
	MonitorDownAfter        time.Duration
	OMCOutagePendingAfter   time.Duration
	Location                *time.Location
	Outage                  OutageSource
}

// Input is one round.
type Input struct {
	Report domain.Report
	Alerts []domain.Alert
	Prev   []state.PendingItem
	Now    time.Time
	// Quiet marks that immediate pushes are held back. The classification is
	// the same, but a newly opened item is left un-notified so the quiet-end
	// merge (or the next non-quiet cycle) sends it.
	Quiet bool
}

// Result is the policy's verdict for one round.
type Result struct {
	// Immediate are the alerts to push now (empty while quiet; the caller
	// stashes them instead).
	Immediate []domain.Alert
	// Items is the complete pending-item list after reconciliation.
	Items []state.PendingItem
	// Opened and Resolved describe this round's transitions.
	Opened   []state.PendingItem
	Resolved []state.PendingItem
	// Dropped are the evaluation alerts that were classified away.
	Dropped []domain.Alert
}

// Evaluate classifies one round.
func Evaluate(cfg Config, in Input) Result {
	if cfg.Mode == Legacy {
		return Result{
			Immediate: append([]domain.Alert(nil), in.Alerts...),
			Items:     append([]state.PendingItem(nil), in.Prev...),
		}
	}

	now := in.Now
	info := analyze(cfg, in.Report, now)

	// prevAll keys the previous items; resolved items are kept so a recurrence
	// can reopen them (and re-alert once).
	prevAll := map[string]state.PendingItem{}
	for _, it := range in.Prev {
		prevAll[it.ID] = it
	}

	desired, alertFor := signals(cfg, in, info)

	var (
		opened   []state.PendingItem
		resolved []state.PendingItem
		items    []state.PendingItem
		seen     = map[string]bool{}
	)

	for id, sig := range desired {
		seen[id] = true
		prev, ok := prevAll[id]
		switch {
		case ok && prev.Status == state.PendingOpen:
			it := prev
			it.Kind = sig.Kind
			it.Provider = sig.Provider
			it.CredKey = sig.CredKey
			it.Alias = sig.Alias
			it.Action = sig.Action
			it.LastSeen = now
			it.Status = state.PendingOpen
			if !in.Quiet && it.NotifiedAt.IsZero() {
				if a, has := alertFor[id]; has && a != nil {
					it.NotifiedAt = now
				}
			}
			items = append(items, it)
		default:
			// New, or a recurrence of a resolved item: fresh Since and no
			// previous notification, so it alerts exactly once.
			it := sig
			it.Since = now
			it.LastSeen = now
			it.Status = state.PendingOpen
			it.ResolvedAt = time.Time{}
			it.NotifiedAt = time.Time{}
			if !in.Quiet {
				if a, has := alertFor[id]; has && a != nil {
					it.NotifiedAt = now
				}
			}
			items = append(items, it)
			opened = append(opened, it)
		}
	}

	// Previous items with no current signal: resolve them when we have positive
	// evidence of recovery, otherwise keep them (evidence absent is not recovery).
	for _, prev := range in.Prev {
		if seen[prev.ID] {
			continue
		}
		if prev.Status == state.PendingResolved {
			items = append(items, prev)
			continue
		}
		if info.resolvedByEvidence(prev) {
			it := prev
			it.Status = state.PendingResolved
			it.ResolvedAt = now
			items = append(items, it)
			resolved = append(resolved, it)
			continue
		}
		items = append(items, prev)
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].Since.Before(items[j].Since) })

	res := Result{Items: items, Opened: opened, Resolved: resolved}
	// The classification is the same whether or not quiet hours are active; the
	// Quiet flag only decides whether NotifiedAt is stamped. The app stashes the
	// same Immediate list when quiet instead of sending it.
	for _, a := range in.Alerts {
		if a.Kind == domain.AlertBootstrap {
			res.Immediate = append(res.Immediate, a)
		}
	}
	// Newly opened items push once, in a stable order.
	sorted := append([]state.PendingItem(nil), opened...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, it := range sorted {
		if a, has := alertFor[it.ID]; has && a != nil {
			res.Immediate = append(res.Immediate, *a)
		}
	}

	// Everything the engine produced that was neither bootstrap nor an opened
	// item's alert is dropped.
	for _, a := range in.Alerts {
		if a.Kind == domain.AlertBootstrap {
			continue
		}
		res.Dropped = append(res.Dropped, a)
	}
	return res
}

// signals builds the desired pending items from the report, plus the immediate
// alert to attach to each on first open (nil for items that never push).
func signals(cfg Config, in Input, info reportInfo) (desired map[string]state.PendingItem, alertFor map[string]*domain.Alert) {
	desired = map[string]state.PendingItem{}
	alertFor = map[string]*domain.Alert{}
	now := in.Now

	// credential_invalid: one per broken, enabled account.
	for _, p := range info.providers {
		for _, s := range p.snapshots {
			if s.Credential.Disabled {
				continue
			}
			if p.states[s.Credential.Key] != domain.StateInvalid {
				continue
			}
			id := ItemID(domain.AlertCredential, p.provider, s.Credential.Key, "")
			it := state.PendingItem{
				ID: id, Kind: string(domain.AlertCredential), Provider: p.provider,
				CredKey: s.Credential.Key, Alias: s.Credential.Alias,
				Action: reauthAdvice(s.Credential.Provider),
			}
			desired[id] = it
			if a := credentialAlert(in.Alerts, s.Credential, now); a != nil {
				alertFor[id] = a
			}
		}
	}

	// channel_unavailable: the whole enabled channel is unusable, and its
	// earliest recovery lies beyond the threshold.
	for _, p := range info.providers {
		if !p.channelUnavailable {
			continue
		}
		id := ItemID(domain.AlertChannelUnavailable, p.provider, "", "")
		it := state.PendingItem{
			ID: id, Kind: string(domain.AlertChannelUnavailable), Provider: p.provider,
			Action: "检查该渠道账号额度，必要时等待恢复或更换账号",
		}
		desired[id] = it
		a := channelAlert(p, now)
		alertFor[id] = &a
	}

	// monitor_down: no valid data for longer than the threshold.
	for _, p := range info.providers {
		if !p.monitorDown {
			continue
		}
		id := ItemID(domain.AlertMonitorDown, p.provider, "", "")
		it := state.PendingItem{
			ID: id, Kind: string(domain.AlertMonitorDown), Provider: p.provider,
			Action: "检查采集链路与上游可达性",
		}
		desired[id] = it
		a := monitorAlert(p, now)
		alertFor[id] = &a
	}

	// omc_outage: reserved. Never immediate; pending only.
	if cfg.Outage != nil {
		for _, p := range info.providers {
			since, active := cfg.Outage.OutageSince(p.provider, now)
			if !active || now.Sub(since) <= cfg.OMCOutagePendingAfter {
				continue
			}
			id := ItemID(domain.AlertOMCOutage, p.provider, "", "")
			desired[id] = state.PendingItem{
				ID: id, Kind: string(domain.AlertOMCOutage), Provider: p.provider,
				Action: "确认 CPA 直连可用；OMC 恢复后会自动关闭",
			}
		}
	}
	return desired, alertFor
}

// ItemID is the stable pending-item identity: hash(kind, credential key or
// provider, scope). It is exported so the app can look up the item an
// accumulated quiet-hours alert belongs to.
func ItemID(kind domain.AlertKind, provider domain.ProviderKind, credKey, scope string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(provider))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(credKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(scope))
	return string(kind) + ":" + strconv.FormatUint(h.Sum64(), 16)
}

// AlertItemID returns the pending-item ID an alert belongs to, or "" when the
// alert does not map to a pending item. It mirrors the identity the signals
// use so the quiet queue can be filtered against the open items.
func AlertItemID(a domain.Alert) string {
	switch a.Kind {
	case domain.AlertCredential:
		return ItemID(domain.AlertCredential, a.Credential.Provider, a.Credential.Key, "")
	case domain.AlertChannelUnavailable:
		return ItemID(domain.AlertChannelUnavailable, a.Credential.Provider, "", "")
	case domain.AlertMonitorDown:
		return ItemID(domain.AlertMonitorDown, a.Credential.Provider, "", "")
	case domain.AlertOMCOutage:
		return ItemID(domain.AlertOMCOutage, a.Credential.Provider, "", "")
	default:
		return ""
	}
}

func credentialAlert(alerts []domain.Alert, cred domain.Credential, now time.Time) *domain.Alert {
	for _, a := range alerts {
		if a.Kind == domain.AlertCredential && a.Credential.Key == cred.Key {
			cp := a
			return &cp
		}
	}
	advice := reauthAdvice(cred.Provider)
	a := domain.Alert{
		Kind:       domain.AlertCredential,
		Severity:   domain.SeverityUrgent,
		Evidence:   domain.EvidenceConfirmed,
		Credential: cred,
		Title:      "凭证失效",
		Detail:     cred.Label() + " 出现明确的认证失败证据，已判定为失效。",
		Advice:     advice,
		OccurredAt: now,
	}
	return &a
}

func channelAlert(p providerInfo, now time.Time) domain.Alert {
	facts := []string{}
	if p.earliestRecovery != nil {
		facts = append(facts, "最早恢复: "+p.earliestRecovery.Format(time.RFC3339))
	} else {
		facts = append(facts, "无自动恢复时间")
	}
	return domain.Alert{
		Kind:       domain.AlertChannelUnavailable,
		Severity:   domain.SeverityUrgent,
		Evidence:   domain.EvidenceConfirmed,
		Credential: domain.Credential{Provider: p.provider},
		Title:      providerName(p.provider) + " 渠道不可用",
		Detail:     "该渠道已启用账号全部不可用，最早恢复时间晚于阈值。",
		Facts:      facts,
		Advice:     "检查该渠道账号额度，必要时等待恢复或更换账号",
		OccurredAt: now,
	}
}

func monitorAlert(p providerInfo, now time.Time) domain.Alert {
	facts := []string{"最后有效数据: " + displayTime(p.latestSuccess, nil)}
	return domain.Alert{
		Kind:       domain.AlertMonitorDown,
		Severity:   domain.SeverityWarn,
		Evidence:   domain.EvidenceConfirmed,
		Credential: domain.Credential{Provider: p.provider},
		Title:      providerName(p.provider) + " 监控停摆",
		Detail:     "该渠道连续超过阈值没有取到有效数据。",
		Facts:      facts,
		Advice:     "检查采集链路与上游可达性",
		OccurredAt: now,
	}
}

func reauthAdvice(p domain.ProviderKind) string {
	if p == domain.ProviderOllama {
		return "更新 Ollama 的 __Secure-session cookie"
	}
	return "前往 CPA 重新完成 OAuth 登录"
}

func providerName(p domain.ProviderKind) string {
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

func displayTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "从未成功"
	}
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04")
}
