// Package advice projects current quota readings into read-only model-config
// advice. No historical prediction or cross-credential window merging occurs.
package advice

import (
	"fmt"
	"sort"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

type Profile = config.ModelProfile
type ThresholdsFor func(domain.ProviderKind) config.Thresholds

type Verdict string

const (
	OK      Verdict = "ok"
	Caution Verdict = "caution"
	Avoid   Verdict = "avoid"
	Unknown Verdict = "unknown"
)

// These DTOs are the entire JSON surface; no domain credential or snapshot is
// embedded, so account identifiers and upstream errors cannot escape.
type Reason struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

type TightestWindow struct {
	Name        string     `json:"name"`
	UsedPercent *float64   `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at"`
}

type Item struct {
	Name              string              `json:"name"`
	Provider          domain.ProviderKind `json:"provider"`
	Verdict           Verdict             `json:"verdict"`
	Rank              *int                `json:"rank"`
	Reasons           []Reason            `json:"reasons"`
	TightestWindow    *TightestWindow     `json:"tightest_window"`
	DataAgeSeconds    *int64              `json:"data_age_seconds"`
	CredentialsTotal  int                 `json:"credentials_total"`
	CredentialsUsable int                 `json:"credentials_usable"`
}

type Result struct {
	Items []Item `json:"items"`
}

type candidate struct {
	verdict      Verdict
	reason       Reason
	window       *TightestWindow
	age          *int64
	usable       bool
	unknownScope bool
}

// Build reads only the passed report, profiles, thresholds and clock. An empty
// binding considers account scope only; explicit bindings include account,
// model and group scopes by exact Name or Label. Unknown scope is never ranked
// as available capacity or a limit, but a numeric match warrants caution.
// A blocked credential plus an unmeasured credential remains unknown: only
// when every enabled credential is blocked can absence of capacity be asserted.
func Build(report domain.Report, profiles []Profile, th ThresholdsFor, now time.Time) Result {
	result := Result{Items: make([]Item, 0, len(profiles))}
	for _, profile := range profiles {
		item := Item{Name: profile.Name, Provider: profile.Provider, Verdict: Unknown, Reasons: []Reason{}}
		best := candidate{verdict: Unknown, reason: Reason{"no_data", "暂无可用读数"}}
		bestSet := false
		blocked, unmeasured := 0, 0
		for _, provider := range report.Providers {
			if provider.Provider != profile.Provider {
				continue
			}
			for _, snap := range provider.Snapshots {
				if snap.Credential.Disabled {
					continue
				}
				item.CredentialsTotal++
				c := assess(snap, provider.States[snap.Credential.Key], profile, th(profile.Provider), now)
				if c.usable {
					item.CredentialsUsable++
				}
				switch c.verdict {
				case Avoid:
					blocked++
				case Unknown:
					unmeasured++
				}
				if !bestSet || betterCandidate(c, best) {
					best = c
					bestSet = true
				}
			}
			if provider.Error != "" && item.CredentialsTotal == 0 {
				best.reason = Reason{"collection_failed", "采集失败"}
			}
		}
		if blocked > 0 && unmeasured > 0 && best.verdict == Unknown {
			best.reason = Reason{"uncertain_capacity", fmt.Sprintf("%d 个凭证被阻断，%d 个凭证无有效读数", blocked, unmeasured)}
		}
		item.Verdict, item.TightestWindow, item.DataAgeSeconds = best.verdict, best.window, best.age
		item.Reasons = []Reason{best.reason}
		if best.unknownScope && best.reason.Code != "unknown_scope" {
			item.Reasons = append(item.Reasons, Reason{"unknown_scope", "窗口范围未知"})
		}
		result.Items = append(result.Items, item)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		a, b := result.Items[i], result.Items[j]
		if priority(a.Verdict) != priority(b.Verdict) {
			return priority(a.Verdict) < priority(b.Verdict)
		}
		if a.TightestWindow != nil && b.TightestWindow != nil &&
			*a.TightestWindow.UsedPercent != *b.TightestWindow.UsedPercent {
			return *a.TightestWindow.UsedPercent < *b.TightestWindow.UsedPercent
		}
		if (a.TightestWindow == nil) != (b.TightestWindow == nil) {
			return a.TightestWindow != nil
		}
		return a.Name < b.Name
	})
	rank := 0
	for i := range result.Items {
		if result.Items[i].Verdict == Unknown {
			continue
		}
		rank++
		r := rank
		result.Items[i].Rank = &r
	}
	return result
}

func priority(v Verdict) int {
	switch v {
	case OK:
		return 0
	case Caution:
		return 1
	case Avoid:
		return 2
	default:
		return 3
	}
}

func betterCandidate(a, b candidate) bool {
	// Unknown ranks below avoid in the output, but must win the aggregate
	// when all other credentials are blocked.
	score := func(v Verdict) int {
		switch v {
		case OK:
			return 0
		case Caution:
			return 1
		case Unknown:
			return 2
		default:
			return 3
		}
	}
	if score(a.verdict) != score(b.verdict) {
		return score(a.verdict) < score(b.verdict)
	}
	if a.window != nil && b.window != nil {
		return *a.window.UsedPercent < *b.window.UsedPercent
	}
	return a.window != nil && b.window == nil
}

func assess(s domain.QuotaSnapshot, state domain.CredentialState, profile Profile, threshold config.Thresholds, now time.Time) candidate {
	c := candidate{verdict: Unknown, reason: Reason{"no_data", "暂无可用读数"}}
	if !s.FetchedAt.IsZero() {
		seconds := int64(now.Sub(s.FetchedAt).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		c.age = &seconds
	}
	if s.Credential.NextRetryAfter != nil && s.Credential.NextRetryAfter.After(now) {
		c.verdict, c.reason = Avoid, Reason{"cooldown", "凭证仍在冷却"}
		return c
	}
	for _, cd := range s.Credential.Cooldowns {
		if cd.RetryAt != nil && cd.RetryAt.After(now) {
			c.verdict, c.reason = Avoid, Reason{"cooldown", "凭证仍在冷却"}
			return c
		}
	}
	if s.Stale {
		c.reason = Reason{"stale", "读数已过期"}
		return c
	}
	if !s.OK {
		c.reason = Reason{"collection_failed", "采集失败"}
		return c
	}

	unknownScope := false
	limit := false
	for _, w := range s.Windows {
		if !matches(profile.Windows, w) {
			continue
		}
		if w.Scope.Normalized() == domain.ScopeUnknown {
			unknownScope = unknownScope || w.UsedPercent != nil
			continue
		}
		if w.LimitReached {
			limit = true
		}
		if w.UsedPercent != nil && (c.window == nil || *w.UsedPercent > *c.window.UsedPercent) {
			used := *w.UsedPercent
			var reset *time.Time
			if w.ResetAt != nil {
				value := *w.ResetAt
				reset = &value
			}
			c.window = &TightestWindow{Name: w.DisplayLabel(), UsedPercent: &used, ResetAt: reset}
		}
	}
	switch {
	case limit:
		c.verdict, c.reason = Avoid, Reason{"limit_reached", "绑定窗口已达上限"}
	case c.window != nil && *c.window.UsedPercent >= threshold.Urgent:
		c.verdict, c.reason = Avoid, Reason{"urgent", "绑定窗口已达紧急阈值"}
	case c.window != nil && *c.window.UsedPercent >= threshold.Notice:
		c.verdict, c.reason = Caution, Reason{"notice", "绑定窗口接近用尽"}
	case state == domain.StateLimited && c.window != nil:
		c.verdict, c.reason = Caution, Reason{"limited", "凭证容量范围受限"}
	case unknownScope:
		c.verdict, c.reason = Caution, Reason{"unknown_scope", "窗口范围未知"}
	case c.window != nil:
		c.verdict, c.reason = OK, Reason{"available", "当前有可用额度"}
	}
	c.usable = c.window != nil && c.verdict != Avoid
	c.unknownScope = unknownScope
	return c
}

func matches(binding []string, w domain.QuotaWindow) bool {
	if len(binding) == 0 {
		return w.Scope.Normalized() == domain.ScopeAccount
	}
	for _, name := range binding {
		if name == w.Name || name == w.Label {
			return true
		}
	}
	return false
}
