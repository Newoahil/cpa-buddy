package policy

import (
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// TightChannels returns the channels that count as tight: the best usable
// account's tightest account-level window is below threshold remaining
// (docs/alert-noise-plan.md §3.1 condition 2).
//
// "Best usable account" is the one with the most headroom, measured by its own
// tightest account-level window. Model/group-only channels are not judged here,
// because the rule is stated on account-level windows.
func TightChannels(rep domain.Report, threshold float64) []domain.TightChannel {
	var out []domain.TightChannel
	for _, p := range rep.Providers {
		var best *float64
		var bestReset *time.Time
		for _, s := range p.Snapshots {
			if s.Credential.Disabled {
				continue
			}
			if !usableState(p.States[s.Credential.Key]) {
				continue
			}
			var minRem *float64
			var minReset *time.Time
			for _, w := range s.Windows {
				if w.Scope.Normalized() != domain.ScopeAccount || w.UsedPercent == nil {
					continue
				}
				rem := 100 - *w.UsedPercent
				if rem < 0 {
					rem = 0
				}
				if minRem == nil || rem < *minRem {
					val := rem
					minRem, minReset = &val, w.ResetAt
				}
			}
			if minRem == nil {
				continue
			}
			if best == nil || *minRem > *best {
				best, bestReset = minRem, minReset
			}
		}
		if best != nil && *best < threshold {
			out = append(out, domain.TightChannel{
				Provider:      p.Provider,
				BestRemaining: best,
				ResetAt:       bestReset,
			})
		}
	}
	return out
}

// usableState reports an account that can still take traffic. It mirrors the
// renderer's usableState so the summary decision and the card agree.
func usableState(s domain.CredentialState) bool {
	switch s {
	case domain.StateHealthy, domain.StateLimited, domain.StateNotice, domain.StateWarning:
		return true
	}
	return false
}
