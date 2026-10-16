package advice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func pct(v float64) *float64 { return &v }

func snapshot(key string, p domain.ProviderKind, windows ...domain.QuotaWindow) domain.QuotaSnapshot {
	return domain.QuotaSnapshot{
		Credential: domain.Credential{Key: key, Provider: p, Alias: "private-alias", AuthIndex: "private-auth"},
		Windows:    windows, OK: true, FetchedAt: time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC),
	}
}

func window(name string, scope domain.QuotaScope, value *float64) domain.QuotaWindow {
	return domain.QuotaWindow{Name: name, Scope: scope, UsedPercent: value}
}

func TestVerdicts(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 1, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	cases := []struct {
		name   string
		snap   domain.QuotaSnapshot
		state  domain.CredentialState
		bind   []string
		want   Verdict
		usable int
	}{
		{"ok", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(20))), "", nil, OK, 1},
		{"notice", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(90))), "", nil, Caution, 1},
		{"urgent", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(100))), "", nil, Avoid, 0},
		{"limited", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(10))), domain.StateLimited, nil, Caution, 1},
		{"limit marker", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, nil))
			s.Windows[0].LimitReached = true
			return s
		}(), "", nil, Avoid, 0},
		{"cooldown", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, nil))
			s.Credential.Cooldowns = []domain.Cooldown{{RetryAt: &future}}
			return s
		}(), "", nil, Avoid, 0},
		{"next retry", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(10)))
			s.Credential.NextRetryAfter = &future
			return s
		}(), "", nil, Avoid, 0},
		{"stale", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(100)))
			s.Stale = true
			return s
		}(), "", nil, Unknown, 0},
		{"stale limit marker", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, nil))
			s.Windows[0].LimitReached = true
			s.Stale = true
			return s
		}(), "", nil, Unknown, 0},
		{"failed", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(20)))
			s.OK = false
			return s
		}(), "", nil, Unknown, 0},
		{"missing number", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeAccount, nil)), "", nil, Unknown, 0},
		{"unknown scope ignored by default", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeUnknown, pct(20))), "", nil, Unknown, 0},
		{"unknown scope bound", snapshot("k", domain.ProviderCodex, window("a", domain.ScopeUnknown, pct(20))), "", []string{"a"}, Caution, 0},
		{"unknown scope cannot block", func() domain.QuotaSnapshot {
			s := snapshot("k", domain.ProviderCodex, window("a", domain.ScopeUnknown, pct(100)))
			s.Windows[0].LimitReached = true
			return s
		}(), "", []string{"a"}, Caution, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderCodex, Snapshots: []domain.QuotaSnapshot{tc.snap}, States: map[string]domain.CredentialState{"k": tc.state}}}}
			got := Build(report, []Profile{{Name: "p", Provider: domain.ProviderCodex, Windows: tc.bind}}, config.Config{DefaultThresholds: config.Thresholds{Notice: 90, Urgent: 100}}.ThresholdsFor, now).Items[0]
			if got.Verdict != tc.want || got.CredentialsUsable != tc.usable || got.CredentialsTotal != 1 {
				t.Fatalf("got %+v, want %s usable %d", got, tc.want, tc.usable)
			}
			if got.DataAgeSeconds == nil || *got.DataAgeSeconds != 60 {
				t.Fatalf("age = %+v", got.DataAgeSeconds)
			}
		})
	}
}

func TestMixedUnknownScopeReasonAndProviderThreshold(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 1, 0, 0, time.UTC)
	s := snapshot("k", domain.ProviderCodex, window("account", domain.ScopeAccount, pct(20)), window("mystery", domain.ScopeUnknown, pct(100)))
	report := domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderCodex, Snapshots: []domain.QuotaSnapshot{s}}}}
	cfg := config.Config{DefaultThresholds: config.Thresholds{Notice: 90, Urgent: 100}, ProviderThresholds: map[domain.ProviderKind]config.Thresholds{domain.ProviderCodex: {Notice: 10, Urgent: 80}}}
	item := Build(report, []Profile{{Name: "mixed", Provider: domain.ProviderCodex, Windows: []string{"account", "mystery"}}}, cfg.ThresholdsFor, now).Items[0]
	if item.Verdict != Caution || item.TightestWindow == nil || *item.TightestWindow.UsedPercent != 20 || item.CredentialsUsable != 1 {
		t.Fatalf("mixed scope = %+v", item)
	}
	found := false
	for _, reason := range item.Reasons {
		found = found || reason.Code == "unknown_scope"
	}
	if !found {
		t.Fatalf("missing scope reason: %+v", item.Reasons)
	}
}

func TestBlockedAndUnmeasuredCredentialsRemainUnknown(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 1, 0, 0, time.UTC)
	blocked := snapshot("blocked", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(100)))
	missing := snapshot("missing", domain.ProviderCodex, window("a", domain.ScopeAccount, nil))
	report := domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderCodex, Snapshots: []domain.QuotaSnapshot{blocked, missing}}}}
	cfg := config.Config{DefaultThresholds: config.Thresholds{Notice: 90, Urgent: 100}}
	got := Build(report, []Profile{{Name: "p", Provider: domain.ProviderCodex}}, cfg.ThresholdsFor, now).Items[0]
	if got.Verdict != Unknown || got.Rank != nil || got.CredentialsUsable != 0 || got.CredentialsTotal != 2 ||
		len(got.Reasons) != 1 || got.Reasons[0].Code != "uncertain_capacity" ||
		!strings.Contains(got.Reasons[0].Text, "1 个凭证被阻断，1 个凭证无有效读数") {
		t.Fatalf("mixed blocked/unmeasured = %+v", got)
	}
	report.Providers[0].Snapshots[1].Windows[0].UsedPercent = pct(20)
	got = Build(report, []Profile{{Name: "p", Provider: domain.ProviderCodex}}, cfg.ThresholdsFor, now).Items[0]
	if got.Verdict != OK || got.CredentialsUsable != 1 {
		t.Fatalf("best usable = %+v", got)
	}
	report.Providers[0].Snapshots[1].Windows[0].UsedPercent = pct(100)
	got = Build(report, []Profile{{Name: "p", Provider: domain.ProviderCodex}}, cfg.ThresholdsFor, now).Items[0]
	if got.Verdict != Avoid {
		t.Fatalf("all blocked = %+v", got)
	}
}

func TestSortingAcrossVerdictsAndResetCopy(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 1, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	a := snapshot("a", domain.ProviderCodex, window("a", domain.ScopeAccount, pct(80)))
	a.Windows[0].ResetAt = &reset
	b := snapshot("b", domain.ProviderClaude, window("b", domain.ScopeAccount, pct(90)))
	c := snapshot("c", domain.ProviderOllama, window("c", domain.ScopeAccount, pct(100)))
	d := snapshot("d", domain.ProviderGeminiCLI, window("d", domain.ScopeAccount, nil))
	report := domain.Report{Providers: []domain.ProviderReport{
		{Provider: domain.ProviderCodex, Snapshots: []domain.QuotaSnapshot{a}},
		{Provider: domain.ProviderClaude, Snapshots: []domain.QuotaSnapshot{b}},
		{Provider: domain.ProviderOllama, Snapshots: []domain.QuotaSnapshot{c}},
		{Provider: domain.ProviderGeminiCLI, Snapshots: []domain.QuotaSnapshot{d}},
	}}
	profiles := []Profile{
		{Name: "unknown", Provider: domain.ProviderGeminiCLI},
		{Name: "avoid", Provider: domain.ProviderOllama},
		{Name: "caution", Provider: domain.ProviderClaude},
		{Name: "ok", Provider: domain.ProviderCodex},
	}
	cfg := config.Config{DefaultThresholds: config.Thresholds{Notice: 90, Urgent: 100}}
	items := Build(report, profiles, cfg.ThresholdsFor, now).Items
	for i, want := range []struct {
		name    string
		verdict Verdict
		rank    int
	}{{"ok", OK, 1}, {"caution", Caution, 2}, {"avoid", Avoid, 3}, {"unknown", Unknown, 0}} {
		if items[i].Name != want.name || items[i].Verdict != want.verdict ||
			(want.rank == 0 && items[i].Rank != nil) ||
			(want.rank != 0 && (items[i].Rank == nil || *items[i].Rank != want.rank)) {
			t.Fatalf("items[%d] = %+v, want %+v", i, items[i], want)
		}
	}
	ok := items[0]
	if ok.TightestWindow == nil || ok.TightestWindow.ResetAt == nil || !ok.TightestWindow.ResetAt.Equal(reset) {
		t.Fatalf("reset = %+v", ok.TightestWindow)
	}
	reset = reset.Add(time.Hour)
	if ok.TightestWindow.ResetAt.Equal(reset) {
		t.Fatal("output aliases input reset")
	}
}

func TestBindingsBestCredentialAndRanking(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 1, 0, 0, time.UTC)
	one := snapshot("one", domain.ProviderCodex, window("5h", domain.ScopeAccount, pct(95)), window("model", domain.ScopeModel, pct(100)))
	two := snapshot("two", domain.ProviderCodex, window("5h", domain.ScopeAccount, pct(40)), window("model", domain.ScopeModel, pct(75)))
	two.Windows[1].Label = "model display"
	disabled := snapshot("off", domain.ProviderCodex, window("5h", domain.ScopeAccount, pct(1)))
	disabled.Credential.Disabled = true
	report := domain.Report{Providers: []domain.ProviderReport{{Provider: domain.ProviderCodex, Snapshots: []domain.QuotaSnapshot{one, two, disabled}}}}
	profiles := []Profile{
		{Name: "z", Provider: domain.ProviderCodex},
		{Name: "a", Provider: domain.ProviderCodex, Windows: []string{"model display"}},
		{Name: "missing", Provider: domain.ProviderClaude},
		{Name: "c", Provider: domain.ProviderCodex, Windows: []string{"model"}},
	}
	before, _ := json.Marshal(struct {
		R domain.Report
		P []Profile
	}{report, profiles})
	cfg := config.Config{DefaultThresholds: config.Thresholds{Notice: 90, Urgent: 100}}
	got := Build(report, profiles, cfg.ThresholdsFor, now)
	again := Build(report, profiles, cfg.ThresholdsFor, now)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("non-deterministic result")
	}
	after, _ := json.Marshal(struct {
		R domain.Report
		P []Profile
	}{report, profiles})
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
	if len(got.Items) != 4 {
		t.Fatalf("items = %+v", got.Items)
	}
	if got.Items[0].Name != "z" || got.Items[0].Rank == nil || *got.Items[0].Rank != 1 || got.Items[0].CredentialsTotal != 2 || got.Items[0].CredentialsUsable != 2 || got.Items[0].TightestWindow == nil || *got.Items[0].TightestWindow.UsedPercent != 40 {
		t.Fatalf("best credential/default scope = %+v", got.Items[0])
	}
	if got.Items[1].Name != "a" || got.Items[1].Verdict != OK || *got.Items[1].Rank != 2 {
		t.Fatalf("label binding = %+v", got.Items[1])
	}
	if got.Items[2].Name != "c" || got.Items[2].Verdict != OK || *got.Items[2].Rank != 3 {
		t.Fatalf("name binding/tie = %+v", got.Items[2])
	}
	if got.Items[3].Name != "missing" || got.Items[3].Rank != nil {
		t.Fatalf("unknown = %+v", got.Items[3])
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-alias", "private-auth", "auth_index", `"key"`, `"alias"`, "email"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("leaked %s in %s", forbidden, data)
		}
	}
}
