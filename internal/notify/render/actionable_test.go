package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func dumpCard(t *testing.T, card map[string]any) string {
	t.Helper()
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rendererAt(loc *time.Location) *Renderer {
	return New(config.ToneCasual).WithLocation(loc)
}

func TestActionableSummaryCard(t *testing.T) {
	utc := time.UTC
	r := rendererAt(utc)
	used := 85.0
	rem := 15.0
	reset := time.Date(2026, 10, 14, 18, 0, 0, 0, utc)
	rep := &domain.Report{
		GeneratedAt: reset,
		Providers: []domain.ProviderReport{{
			Provider: domain.ProviderClaude, Total: 1,
			States: map[string]domain.CredentialState{"a": domain.StateNotice},
			Snapshots: []domain.QuotaSnapshot{{
				Credential: domain.Credential{Key: "a", Provider: domain.ProviderClaude, Alias: "claude-a", ShortID: "s"},
				OK:         true,
				Windows:    []domain.QuotaWindow{{Scope: domain.ScopeAccount, Name: "5h", UsedPercent: &used}},
			}},
		}},
	}
	msg := domain.Message{
		Kind:   string(domain.AlertActionableSummary),
		Report: rep,
		Actionable: &domain.ActionableCard{
			HeaderColor: "orange",
			Conclusion:  "Claude 紧张，有 1 项待处理",
			Pending: []domain.PendingRow{{
				Kind: domain.AlertCredential, Provider: domain.ProviderClaude,
				Alias: "claude-a", Since: reset, Action: "重新登录后更新 cookie",
			}},
			Tight: []domain.TightChannel{{Provider: domain.ProviderClaude, BestRemaining: &rem, ResetAt: &reset}},
		},
	}
	card := r.Card(msg)
	body := dumpCard(t, card)

	for _, want := range []string{"结论", "待处理（1）", "紧张渠道", "最好的账号剩余 15.0%", "各账号明细", "重新登录后更新 cookie"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary card missing %q:\n%s", want, body)
		}
	}
	for _, banned := range []string{"短暂限流", "上次汇总以来", "rateLimitBlock"} {
		if strings.Contains(body, banned) {
			t.Errorf("actionable summary must not contain %q", banned)
		}
	}
	header, _ := card["header"].(map[string]any)
	if header["template"] != "orange" {
		t.Errorf("header template = %v, want orange", header["template"])
	}
}

func TestActionableSummaryColorFromView(t *testing.T) {
	r := rendererAt(time.UTC)
	msg := domain.Message{Kind: string(domain.AlertActionableSummary), Actionable: &domain.ActionableCard{HeaderColor: "red"}}
	card := r.Card(msg)
	header, _ := card["header"].(map[string]any)
	if header["template"] != "red" {
		t.Errorf("header template = %v, want red", header["template"])
	}
}

func TestActionableSummarySkipsPanelWithoutReport(t *testing.T) {
	r := rendererAt(time.UTC)
	msg := domain.Message{Kind: string(domain.AlertActionableSummary), Actionable: &domain.ActionableCard{Conclusion: "无待处理"}}
	card := r.Card(msg)
	body := dumpCard(t, card)
	if strings.Contains(body, "各账号明细") {
		t.Errorf("no report means no detail panel:\n%s", body)
	}
}

func TestNewImmediateKindsGetPhrases(t *testing.T) {
	r := rendererAt(time.UTC)
	cases := []struct {
		kind  domain.AlertKind
		want  string
		color string
	}{
		{domain.AlertChannelUnavailable, "渠道不可用", "red"},
		{domain.AlertMonitorDown, "监控停摆", "orange"},
	}
	for _, tc := range cases {
		msg := domain.Message{Kind: string(tc.kind), Alerts: []domain.Alert{{
			Kind:       tc.kind,
			Severity:   domain.SeverityUrgent,
			Credential: domain.Credential{Provider: domain.ProviderClaude},
			Title:      "x",
		}}}
		card := r.Card(msg)
		body := dumpCard(t, card)
		if !strings.Contains(body, tc.want) {
			t.Errorf("kind %s missing phrase %q:\n%s", tc.kind, tc.want, body)
		}
	}
}

func TestQuietDigestHasTitle(t *testing.T) {
	r := rendererAt(time.UTC)
	msg := domain.Message{Kind: string(domain.AlertQuietDigest), Alerts: []domain.Alert{{
		Kind:       domain.AlertChannelUnavailable,
		Severity:   domain.SeverityUrgent,
		Credential: domain.Credential{Provider: domain.ProviderClaude},
	}}}
	card := r.Card(msg)
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	if title["content"] != "免打扰结束" {
		t.Errorf("quiet digest title = %v, want 免打扰结束", title["content"])
	}
}
