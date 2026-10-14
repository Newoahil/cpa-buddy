package policy

import (
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// providerInfo is the channel-level reading the policy needs.
type providerInfo struct {
	provider  domain.ProviderKind
	snapshots []domain.QuotaSnapshot
	states    map[string]domain.CredentialState
	// channelUnavailable is true when every enabled account is unusable and the
	// earliest recovery lies beyond the threshold.
	channelUnavailable bool
	// earliestRecovery is the soonest account recovery; nil means no account
	// auto-recovers (or the reset is unknown).
	earliestRecovery *time.Time
	// monitorDown is true when no enabled account produced valid data for
	// longer than the threshold.
	monitorDown   bool
	hasValid      bool
	latestSuccess time.Time
}

// reportInfo is the per-round projection used for reconciliation.
type reportInfo struct {
	providers   []providerInfo
	present     map[domain.ProviderKind]bool
	unavailable map[domain.ProviderKind]bool
	hasValid    map[domain.ProviderKind]bool
	hasKey      map[string]bool
	invalidKeys map[string]bool
}

// analyze reads the report once and derives the channel-level conditions.
func analyze(cfg Config, rep domain.Report, now time.Time) reportInfo {
	info := reportInfo{
		present:     map[domain.ProviderKind]bool{},
		unavailable: map[domain.ProviderKind]bool{},
		hasValid:    map[domain.ProviderKind]bool{},
		hasKey:      map[string]bool{},
		invalidKeys: map[string]bool{},
	}
	for _, p := range rep.Providers {
		info.present[p.Provider] = true
		pi := providerInfo{provider: p.Provider, snapshots: p.Snapshots, states: p.States}

		enabled, unavail, nonAuthUnavail := 0, 0, 0
		var earliest *time.Time
		authOnly := true
		sawFailure := false
		for _, s := range p.Snapshots {
			info.hasKey[s.Credential.Key] = true
			st := p.States[s.Credential.Key]
			if st == domain.StateInvalid && !s.Credential.Disabled {
				info.invalidKeys[s.Credential.Key] = true
			}
			if s.LastSuccessAt.After(pi.latestSuccess) {
				pi.latestSuccess = s.LastSuccessAt
			}
			if s.Credential.Disabled {
				continue
			}
			enabled++
			if s.OK {
				pi.hasValid = true
			} else {
				sawFailure = true
				if st != domain.StateInvalid {
					authOnly = false
				}
			}
			// An exhausted account reports OK=true with a maxed window, so the
			// availability check must run for every enabled snapshot.
			if u, rec := accountUnavailable(s, st, now); u {
				unavail++
				if st != domain.StateInvalid {
					nonAuthUnavail++
				}
				if rec != nil {
					earliest = earlierTime(earliest, rec)
				}
			}
		}

		// A channel whose only unavailable accounts are auth-broken is covered
		// by the per-credential credential_invalid alert, not this one.
		if enabled > 0 && unavail == enabled && nonAuthUnavail > 0 {
			pi.channelUnavailable = earliest == nil || earliest.After(now.Add(cfg.ChannelUnavailableAfter))
			pi.earliestRecovery = earliest
		}

		// monitor_down: no valid data, at least one non-auth failure, and the
		// last good reading is old enough. A channel that is auth-broken is a
		// credential problem, not a collection outage.
		if enabled > 0 && !pi.hasValid && sawFailure && !authOnly &&
			!pi.latestSuccess.IsZero() && now.Sub(pi.latestSuccess) > cfg.MonitorDownAfter {
			pi.monitorDown = true
		}
		// An OMC fallback outage is not a monitor outage.
		if pi.monitorDown && cfg.Outage != nil {
			if _, active := cfg.Outage.OutageSince(p.Provider, now); active {
				pi.monitorDown = false
			}
		}

		info.providers = append(info.providers, pi)
		info.unavailable[p.Provider] = pi.channelUnavailable
		info.hasValid[p.Provider] = pi.hasValid
	}
	return info
}

// resolvedByEvidence reports positive evidence that an open item's condition no
// longer holds. Absent evidence (provider or credential missing from the
// report) is not recovery, so the item stays open.
func (info reportInfo) resolvedByEvidence(it state.PendingItem) bool {
	switch domain.AlertKind(it.Kind) {
	case domain.AlertCredential:
		if !info.present[it.Provider] || !info.hasKey[it.CredKey] {
			return false
		}
		return !info.invalidKeys[it.CredKey]
	case domain.AlertChannelUnavailable:
		if !info.present[it.Provider] {
			return false
		}
		return !info.unavailable[it.Provider]
	case domain.AlertMonitorDown:
		if !info.present[it.Provider] {
			return false
		}
		return info.hasValid[it.Provider]
	default:
		// omc_outage and unknown kinds: no source in this branch, never resolve
		// on absence.
		return false
	}
}

// accountUnavailable decides whether one account is unusable, and when it comes
// back. A model- or group-scoped window running out does NOT make the account
// unusable. nil recovery means "no automatic recovery is known".
func accountUnavailable(s domain.QuotaSnapshot, st domain.CredentialState, now time.Time) (bool, *time.Time) {
	if st == domain.StateInvalid {
		return true, nil
	}
	var recoverAt *time.Time
	for _, cd := range s.Credential.Cooldowns {
		if cd.Scope == "model" {
			continue
		}
		if cd.RetryAt != nil && cd.RetryAt.After(now) {
			recoverAt = laterTime(recoverAt, cd.RetryAt)
		}
	}
	if s.Credential.NextRetryAfter != nil && s.Credential.NextRetryAfter.After(now) {
		recoverAt = laterTime(recoverAt, s.Credential.NextRetryAfter)
	}
	if recoverAt != nil {
		return true, recoverAt
	}

	blocked := false
	var reset *time.Time
	for _, w := range s.Windows {
		if w.Scope.Normalized() != domain.ScopeAccount {
			continue
		}
		if w.LimitReached || (w.UsedPercent != nil && *w.UsedPercent >= 100) {
			blocked = true
			if w.ResetAt != nil {
				reset = laterTime(reset, w.ResetAt)
			}
		}
	}
	if blocked {
		return true, reset
	}
	if st == domain.StateExhausted {
		return true, nil
	}
	return false, nil
}

func earlierTime(a, b *time.Time) *time.Time {
	switch {
	case b == nil:
		return a
	case a == nil:
		return b
	case b.Before(*a):
		return b
	default:
		return a
	}
}

func laterTime(a, b *time.Time) *time.Time {
	switch {
	case b == nil:
		return a
	case a == nil:
		return b
	case b.After(*a):
		return b
	default:
		return a
	}
}
