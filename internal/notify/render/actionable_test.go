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

func getBodyElements(t *testing.T, card map[string]any) []map[string]any {
	t.Helper()
	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatalf("card missing body: %v", card)
	}
	rawElements, ok := body["elements"].([]any)
	if !ok {
		t.Fatalf("body missing elements: %v", body)
	}
	out := make([]map[string]any, len(rawElements))
	for i, e := range rawElements {
		elemMap, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("element %d not map: %v", i, e)
		}
		out[i] = elemMap
	}
	return out
}

func TestActionableSummaryCard(t *testing.T) {
	utc := time.UTC
	r := rendererAt(utc)
	base := time.Date(2026, 10, 14, 16, 0, 0, 0, utc)
	since := base.Add(-30 * time.Minute)
	reset := base.Add(2 * time.Hour)
	used := 85.0
	rem := 15.0

	rep := &domain.Report{
		GeneratedAt: base,
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
				Alias: "claude-a", Since: since, Action: "重新登录后更新 cookie",
			}},
			Tight: []domain.TightChannel{{Provider: domain.ProviderClaude, BestRemaining: &rem, ResetAt: &reset}},
		},
	}
	card := r.Card(msg)
	body := dumpCard(t, card)

	// Structural assertions
	if card["schema"] != "2.0" {
		t.Errorf("schema = %v, want 2.0", card["schema"])
	}
	header, _ := card["header"].(map[string]any)
	if header["template"] != "orange" {
		t.Errorf("header template = %v, want orange", header["template"])
	}

	elements := getBodyElements(t, card)
	if len(elements) < 3 {
		t.Fatalf("expected at least 3 elements, got %d", len(elements))
	}

	// 1. Conclusion element
	concl, _ := elements[0]["content"].(string)
	if !strings.Contains(concl, "**结论：** Claude 紧张，有 1 项待处理") {
		t.Errorf("element[0] conclusion mismatch: %q", concl)
	}

	// 2. Pending heading & row
	pendingHead, _ := elements[1]["content"].(string)
	if !strings.Contains(pendingHead, "**待处理（1）**") {
		t.Errorf("element[1] expected pending heading, got: %q", pendingHead)
	}
	pendingContent, _ := elements[2]["content"].(string)
	if !strings.Contains(pendingContent, "color='red'>凭证失效</text_tag>") {
		t.Errorf("pending row missing red tag: %q", pendingContent)
	}
	if !strings.Contains(pendingContent, "Claude a") {
		t.Errorf("pending row missing provider/alias: %q", pendingContent)
	}
	if !strings.Contains(pendingContent, "持续 30 分钟") {
		t.Errorf("pending row missing duration: %q", pendingContent)
	}
	if !strings.Contains(pendingContent, "重新登录后更新 cookie") {
		t.Errorf("pending row missing action: %q", pendingContent)
	}

	// 3. Tight row
	for _, want := range []string{"紧张渠道", "最好的账号剩余 15.0%", "18:00（2 小时后）重置"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary card missing %q:\n%s", want, body)
		}
	}

	// 4. Detail panel & buttons
	if !strings.Contains(body, "各账号明细 · 1 个号") {
		t.Errorf("summary card missing collapsible detail panel:\n%s", body)
	}

	// 5. Banned legacy elements
	for _, banned := range []string{"短暂限流", "上次汇总以来", "rateLimitBlock"} {
		if strings.Contains(body, banned) {
			t.Errorf("actionable summary must not contain %q", banned)
		}
	}
}

func TestActionableSummaryCardSimple(t *testing.T) {
	utc := time.UTC
	r := rendererAt(utc)
	base := time.Date(2026, 10, 14, 16, 0, 0, 0, utc)
	reset := base.Add(2 * time.Hour)
	rem := 18.0

	msg := domain.Message{
		Kind: string(domain.AlertActionableSummary),
		Actionable: &domain.ActionableCard{
			HeaderColor: "orange",
			Conclusion:  "Claude 紧张",
			Tight:       []domain.TightChannel{{Provider: domain.ProviderClaude, BestRemaining: &rem, ResetAt: &reset}},
		},
	}
	simpleCard := r.CardSimple(msg)
	body := dumpCard(t, simpleCard)

	// Structural assertions for degraded card
	cfg, _ := simpleCard["config"].(map[string]any)
	if _, hasWidth := cfg["width_mode"]; hasWidth {
		t.Errorf("CardSimple must omit width_mode: %v", cfg)
	}
	if strings.Contains(body, "collapsible_panel") {
		t.Errorf("CardSimple must drop collapsible_panel:\n%s", body)
	}
	if strings.Contains(body, "chart") {
		t.Errorf("CardSimple must drop chart:\n%s", body)
	}
	if !strings.Contains(body, "Claude 紧张") || !strings.Contains(body, "最好的账号剩余 18.0%") {
		t.Errorf("CardSimple missing critical content:\n%s", body)
	}
}

func TestActionableSummaryEmptyNormal(t *testing.T) {
	r := rendererAt(time.UTC)
	msg := domain.Message{
		Kind: string(domain.AlertActionableSummary),
		Actionable: &domain.ActionableCard{
			HeaderColor: "blue",
			Conclusion:  "各渠道额度充裕，无需处理",
		},
	}
	card := r.Card(msg)
	body := dumpCard(t, card)

	if !strings.Contains(body, "各渠道额度充裕，无需处理") {
		t.Errorf("missing conclusion in empty summary: %s", body)
	}
	if strings.Contains(body, "待处理") {
		t.Errorf("empty summary should not display pending block: %s", body)
	}
	if strings.Contains(body, "紧张渠道") {
		t.Errorf("empty summary should not display tight block: %s", body)
	}
}

func TestImmediateAlertCards(t *testing.T) {
	utc := time.UTC
	r := rendererAt(utc)
	base := time.Date(2026, 10, 14, 16, 0, 0, 0, utc)
	since := base.Add(-45 * time.Minute)
	reset := base.Add(2 * time.Hour)

	cases := []struct {
		name        string
		kind        domain.AlertKind
		title       string
		action      string
		color       string
		wantAdvice  string
		wantStart   string
		wantRecover string
	}{
		{
			name:        "channel_unavailable",
			kind:        domain.AlertChannelUnavailable,
			title:       "Claude 渠道全部不可用",
			action:      "检查该渠道账号额度，必要时等待恢复或更换账号",
			color:       "red",
			wantAdvice:  "**建议动作：** 检查该渠道账号额度，必要时等待恢复或更换账号",
			wantStart:   "**开始时间：** 10-14 15:15（持续 45 分钟）",
			wantRecover: "**预计恢复：** 18:00（2 小时后）重置",
		},
		{
			name:        "credential_invalid",
			kind:        domain.AlertCredential,
			title:       "Claude 凭据失效",
			action:      "前往 CPA 重新完成 OAuth 登录",
			color:       "red",
			wantAdvice:  "**建议动作：** 前往 CPA 重新完成 OAuth 登录",
			wantStart:   "**开始时间：** 10-14 15:15（持续 45 分钟）",
			wantRecover: "**预计恢复：** 需人工处理（更新凭证后恢复）",
		},
		{
			name:        "monitor_down",
			kind:        domain.AlertMonitorDown,
			title:       "Codex 监控停摆",
			action:      "检查采集链路与上游可达性",
			color:       "orange",
			wantAdvice:  "**建议动作：** 检查采集链路与上游可达性",
			wantStart:   "**开始时间：** 10-14 15:15（持续 45 分钟）",
			wantRecover: "**预计恢复：** 采集链路恢复后自动解除",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cardObj := &domain.ActionableCard{
				HeaderColor: tc.color,
				Pending: []domain.PendingRow{{
					Kind:     tc.kind,
					Provider: domain.ProviderClaude,
					Alias:    "claude-1",
					Since:    since,
					Action:   tc.action,
				}},
			}
			if tc.kind == domain.AlertChannelUnavailable {
				cardObj.Tight = []domain.TightChannel{{Provider: domain.ProviderClaude, ResetAt: &reset}}
			}
			msg := domain.Message{
				Title:      tc.title,
				Kind:       string(tc.kind),
				Report:     &domain.Report{GeneratedAt: base},
				Actionable: cardObj,
			}

			card := r.Card(msg)
			header, _ := card["header"].(map[string]any)
			if header["template"] != tc.color {
				t.Errorf("header template = %v, want %s", header["template"], tc.color)
			}
			titleElem, _ := header["title"].(map[string]any)
			if titleElem["content"] != tc.title {
				t.Errorf("header title = %v, want %s", titleElem["content"], tc.title)
			}

			elements := getBodyElements(t, card)
			// Advice must be the first element
			adviceElem, _ := elements[0]["content"].(string)
			if !strings.Contains(adviceElem, tc.wantAdvice) {
				t.Errorf("first element must be advice:\ngot: %q\nwant: %q", adviceElem, tc.wantAdvice)
			}

			// Time element must follow advice
			timeElem, _ := elements[1]["content"].(string)
			if !strings.Contains(timeElem, tc.wantStart) {
				t.Errorf("time element missing start:\ngot: %q\nwant: %q", timeElem, tc.wantStart)
			}
			if !strings.Contains(timeElem, tc.wantRecover) {
				t.Errorf("time element missing recovery:\ngot: %q\nwant: %q", timeElem, tc.wantRecover)
			}
		})
	}
}

func TestQuietDigestMergedCard(t *testing.T) {
	utc := time.UTC
	r := rendererAt(utc)
	base := time.Date(2026, 10, 15, 8, 0, 0, 0, utc)
	since1 := base.Add(-4 * time.Hour)
	since2 := base.Add(-1 * time.Hour)

	msg := domain.Message{
		Title:  "夜间 2 项待处理",
		Kind:   string(domain.AlertQuietDigest),
		Report: &domain.Report{GeneratedAt: base},
		Actionable: &domain.ActionableCard{
			HeaderColor: "red",
			Conclusion:  "夜间共发生 2 项异常，请及时处理",
			Pending: []domain.PendingRow{
				{
					Kind:     domain.AlertCredential,
					Provider: domain.ProviderClaude,
					Alias:    "claude-pro",
					Since:    since1,
					Action:   "重新登录后更新 cookie",
				},
				{
					Kind:     domain.AlertChannelUnavailable,
					Provider: domain.ProviderCodex,
					Since:    since2,
					Action:   "检查该渠道账号额度",
				},
			},
		},
	}

	card := r.Card(msg)
	header, _ := card["header"].(map[string]any)
	titleElem, _ := header["title"].(map[string]any)
	if titleElem["content"] != "夜间 2 项待处理" {
		t.Errorf("header title = %v, want 夜间 2 项待处理", titleElem["content"])
	}
	if header["template"] != "red" {
		t.Errorf("header template = %v, want red", header["template"])
	}

	body := dumpCard(t, card)
	if !strings.Contains(body, "夜间共发生 2 项异常，请及时处理") {
		t.Errorf("missing merged conclusion: %s", body)
	}
	if !strings.Contains(body, "待处理列表（2）") {
		t.Errorf("missing pending list heading: %s", body)
	}
	if !strings.Contains(body, "持续 4 小时") || !strings.Contains(body, "持续 1 小时") {
		t.Errorf("missing duration in quiet digest rows: %s", body)
	}
}

func TestFormatResetTime(t *testing.T) {
	utc := time.UTC
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	r := rendererAt(shanghai)

	base := time.Date(2026, 10, 14, 11, 20, 0, 0, shanghai) // 11:20

	// Same day, 2 hours later -> 13:20（2 小时后）重置
	reset1 := base.Add(2 * time.Hour)
	got1 := r.formatResetTime(reset1, base)
	if got1 != "13:20（2 小时后）重置" {
		t.Errorf("same day reset = %q, want 13:20（2 小时后）重置", got1)
	}

	// Next day, 15 hours later -> 10-15 02:20（15 小时后）重置
	reset2 := base.Add(15 * time.Hour)
	got2 := r.formatResetTime(reset2, base)
	if got2 != "10-15 02:20（15 小时后）重置" {
		t.Errorf("next day reset = %q, want 10-15 02:20（15 小时后）重置", got2)
	}

	// Expired
	reset3 := base.Add(-10 * time.Minute)
	got3 := r.formatResetTime(reset3, base)
	if got3 != "11:10（已到期）重置" {
		t.Errorf("expired reset = %q, want 11:10（已到期）重置", got3)
	}
	_ = utc
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{20 * time.Second, "不到 1 分钟"},
		{45 * time.Minute, "45 分钟"},
		{2 * time.Hour, "2 小时"},
		{2*time.Hour + 15*time.Minute, "2 小时 15 分钟"},
		{24 * time.Hour, "1 天"},
		{26 * time.Hour, "1 天 2 小时"},
	}
	for _, tc := range cases {
		got := formatDuration(tc.d)
		if got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestPendingLabelAndTagColor(t *testing.T) {
	cases := []struct {
		kind      domain.AlertKind
		wantLabel string
		wantColor string
	}{
		{domain.AlertCredential, "凭证失效", "red"},
		{domain.AlertChannelUnavailable, "渠道不可用", "red"},
		{domain.AlertMonitorDown, "监控停摆", "orange"},
		{domain.AlertOMCOutage, "OMC 中断", "orange"},
		{domain.AlertQuietDigest, "免打扰合并", "orange"},
	}
	for _, tc := range cases {
		if got := pendingLabel(tc.kind); got != tc.wantLabel {
			t.Errorf("pendingLabel(%s) = %q, want %q", tc.kind, got, tc.wantLabel)
		}
		if got := pendingTagColor(tc.kind); got != tc.wantColor {
			t.Errorf("pendingTagColor(%s) = %q, want %q", tc.kind, got, tc.wantColor)
		}
	}
}

func TestActionableText(t *testing.T) {
	r := rendererAt(time.UTC)
	base := time.Date(2026, 10, 14, 16, 0, 0, 0, time.UTC)
	reset := base.Add(2 * time.Hour)
	rem := 15.0

	// Set clock hook for text test
	oldNow := nowFunc
	nowFunc = func() time.Time { return base }
	t.Cleanup(func() { nowFunc = oldNow })

	card := &domain.ActionableCard{
		Conclusion: "Claude 紧张，有 1 项待处理",
		Pending: []domain.PendingRow{{
			Kind:     domain.AlertCredential,
			Provider: domain.ProviderClaude,
			Alias:    "main",
			Since:    base.Add(-30 * time.Minute),
			Action:   "重新登录后更新 cookie",
		}},
		Tight: []domain.TightChannel{{
			Provider:      domain.ProviderClaude,
			BestRemaining: &rem,
			ResetAt:       &reset,
		}},
	}

	text := r.actionableText(card)
	if !strings.Contains(text, "结论：Claude 紧张，有 1 项待处理") {
		t.Errorf("missing text conclusion: %s", text)
	}
	if !strings.Contains(text, "[凭证失效] Claude main · 持续 30 分钟：重新登录后更新 cookie") {
		t.Errorf("missing text pending row: %s", text)
	}
	if !strings.Contains(text, "Claude 最好的账号剩余 15.0% · 18:00（2 小时后）重置") {
		t.Errorf("missing text tight row: %s", text)
	}
	// Plain text must not contain Feishu tags
	for _, tag := range []string{"<text_tag", "<font", "**"} {
		if strings.Contains(text, tag) {
			t.Errorf("actionableText must not contain card formatting tag %q:\n%s", tag, text)
		}
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
