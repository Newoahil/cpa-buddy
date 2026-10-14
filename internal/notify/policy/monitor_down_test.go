package policy

import (
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/state"
)

// outageStub is a deterministic OutageSource: active for every provider while
// active is true.
type outageStub struct {
	active bool
	since  time.Time
}

func (o *outageStub) OutageSince(domain.ProviderKind, time.Time) (time.Time, bool) {
	if !o.active {
		return time.Time{}, false
	}
	return o.since, true
}

// collectorFailureReport is a channel whose whole collector failed: a
// provider-wide Error with no snapshots at all, the case the snapshot-only
// timing could not see.
func collectorFailureReport(p domain.ProviderKind, msg string) domain.Report {
	return domain.Report{
		GeneratedAt: t0,
		Providers: []domain.ProviderReport{{
			Provider: p, WorstState: domain.StateUnknown, Error: msg,
			States: map[string]domain.CredentialState{},
		}},
	}
}

func TestMonitorDownWholeCollectorFailureFromTrack(t *testing.T) {
	now := t0
	rep := collectorFailureReport(domain.ProviderCodex, "渠道采集失败")
	tracks := map[domain.ProviderKind]state.ChannelTrack{
		domain.ProviderCodex: {FirstSeen: now.Add(-2 * time.Hour), LastValid: now.Add(-40 * time.Minute)},
	}
	res := Evaluate(defaultCfg(), Input{Report: rep, ChannelTracks: tracks, Now: now})
	if !hasKind(res.Immediate, domain.AlertMonitorDown) {
		t.Fatalf("collector-wide failure not pushed: %v", kinds(res.Immediate))
	}
	if len(res.Opened) != 1 || res.Opened[0].Kind != string(domain.AlertMonitorDown) {
		t.Fatalf("monitor_down not opened: %+v", res.Opened)
	}
}

func TestMonitorDownWholeCollectorFailureBelowThreshold(t *testing.T) {
	now := t0
	rep := collectorFailureReport(domain.ProviderCodex, "渠道采集失败")
	tracks := map[domain.ProviderKind]state.ChannelTrack{
		domain.ProviderCodex: {FirstSeen: now.Add(-2 * time.Hour), LastValid: now.Add(-10 * time.Minute)},
	}
	res := Evaluate(defaultCfg(), Input{Report: rep, ChannelTracks: tracks, Now: now})
	if len(res.Immediate) != 0 || len(res.Opened) != 0 {
		t.Fatalf("below-threshold failure alerted: immediate=%v opened=%v", kinds(res.Immediate), res.Opened)
	}
}

func TestMonitorDownNeverSuccessfulFromFirstSeen(t *testing.T) {
	now := t0
	rep := collectorFailureReport(domain.ProviderOllama, "Ollama 采集失败")
	tracks := map[domain.ProviderKind]state.ChannelTrack{
		domain.ProviderOllama: {FirstSeen: now.Add(-45 * time.Minute)}, // never valid
	}
	res := Evaluate(defaultCfg(), Input{Report: rep, ChannelTracks: tracks, Now: now})
	if !hasKind(res.Immediate, domain.AlertMonitorDown) {
		t.Fatalf("never-successful channel not timed from its first attempt: %v", kinds(res.Immediate))
	}
}

func TestMonitorDownAllDisabledNotCounted(t *testing.T) {
	now := t0
	snap := failSnap("k1", domain.ProviderCodex, now.Add(-2*time.Hour))
	snap.Credential.Disabled = true
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateStale}, snap)
	tracks := map[domain.ProviderKind]state.ChannelTrack{
		domain.ProviderCodex: {FirstSeen: now.Add(-3 * time.Hour), LastValid: now.Add(-2 * time.Hour)},
	}
	res := Evaluate(defaultCfg(), Input{Report: rep, ChannelTracks: tracks, Now: now})
	if hasKind(res.Immediate, domain.AlertMonitorDown) || len(res.Opened) != 0 {
		t.Fatalf("all-disabled channel alerted: immediate=%v opened=%v", kinds(res.Immediate), res.Opened)
	}
}

func TestMonitorDownSuppressedDuringOMCOutage(t *testing.T) {
	now := t0
	cfg := defaultCfg()
	cfg.Outage = &outageStub{active: true, since: now.Add(-30 * time.Minute)}
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateStale},
		failSnap("k1", domain.ProviderCodex, now.Add(-40*time.Minute)))
	res := Evaluate(cfg, Input{Report: rep, Now: now})
	if hasKind(res.Immediate, domain.AlertMonitorDown) {
		t.Fatalf("omc outage must suppress monitor_down: %v", kinds(res.Immediate))
	}
}

func TestOMCOutagePendingOnlyAfterThreshold(t *testing.T) {
	now := t0
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateHealthy},
		acctSnap("k1", domain.ProviderCodex, 10, nil))

	short := defaultCfg()
	short.Outage = &outageStub{active: true, since: now.Add(-1 * time.Hour)}
	res := Evaluate(short, Input{Report: rep, Now: now})
	if len(res.Opened) != 0 || len(res.Immediate) != 0 {
		t.Fatalf("short outage must stay quiet: opened=%v immediate=%v", res.Opened, kinds(res.Immediate))
	}

	long := defaultCfg()
	long.Outage = &outageStub{active: true, since: now.Add(-3 * time.Hour)}
	res = Evaluate(long, Input{Report: rep, Now: now})
	if len(res.Opened) != 1 || res.Opened[0].Kind != string(domain.AlertOMCOutage) {
		t.Fatalf("sustained outage not opened: %+v", res.Opened)
	}
	if len(res.Immediate) != 0 {
		t.Fatalf("omc_outage must never push immediately: %v", kinds(res.Immediate))
	}
}

func TestOMCOutageResolvesOnRecovery(t *testing.T) {
	now := t0
	rep := report(domain.ProviderCodex, map[string]domain.CredentialState{"k1": domain.StateHealthy},
		acctSnap("k1", domain.ProviderCodex, 10, nil))
	cfg := defaultCfg()
	cfg.Outage = &outageStub{active: true, since: now.Add(-3 * time.Hour)}
	first := Evaluate(cfg, Input{Report: rep, Now: now})
	if len(first.Opened) != 1 {
		t.Fatalf("setup: %+v", first.Opened)
	}
	// Same report, outage cleared.
	cfg.Outage = &outageStub{active: false}
	second := Evaluate(cfg, Input{Report: rep, Prev: first.Items, Now: now.Add(time.Minute)})
	if len(second.Resolved) != 1 || second.Resolved[0].Kind != string(domain.AlertOMCOutage) {
		t.Fatalf("omc_outage not resolved: %+v", second.Items)
	}
}
