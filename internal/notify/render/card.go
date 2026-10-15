package render

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// maxCardElements is Feishu's hard per-card component ceiling. We stay a
// little below it: the count includes nested panel elements and table rows, and
// exceeding it makes the whole card fail to send in strict JSON 2.0.
const maxCardElements = 200

// maxPanelRows bounds how many detail rows one collapsible panel contributes,
// so a provider with a hundred credentials cannot blow the component budget.
const maxPanelRows = 50

// maxChartBars bounds how many credential bars one card chart may hold. Beyond
// it the axis labels crowd and the chart stops being readable, so the caller
// falls back to the text lines, which already carry the exact numbers.
const maxChartBars = 8

// chartHeight is the chart's fixed height. A fixed value makes aspect_ratio
// moot and stops the chart growing with the card's width.
const chartHeight = "200px"

// Card builds the full Feishu interactive-card (schema 2.0) structure.
//
// Layout: header (title + colour + abnormal-channel text tags), a one-line
// conclusion, then a per-channel block — a status line, a linearProgress chart
// of the remaining share of every credential, and, for abnormal channels only,
// a collapsible detail table — then the footer (a read-only refresh button, an
// optional CPA link, and the data-time/caliber line).
//
// The display caliber is REMAINING (100 - used), matching the CPA console. The
// evaluation thresholds are still used-percentage; only the presentation
// changed.
func (r *Renderer) Card(msg domain.Message) map[string]any {
	return r.card(msg, true)
}

// CardSimple is the degraded card: no chart, no collapsible_panel, no table.
//
// It exists because strict JSON 2.0 rejects a card wholesale when one element
// is malformed, and the chart component's client support is the least certain
// thing in the card. The channel falls back to it automatically, and the
// channel-agnostic evidence survives in markdown.
func (r *Renderer) CardSimple(msg domain.Message) map[string]any {
	return r.card(msg, false)
}

func (r *Renderer) card(msg domain.Message, full bool) map[string]any {
	msg = r.FilterMessage(msg)
	charts := full && r.charts
	header := r.cardHeader(msg)
	// The subtitle is only on the full card: the degraded card is the fallback
	// when the full one is rejected, so it keeps the minimal header.
	if full {
		if s := r.headerSubtitle(msg); s != "" {
			header["subtitle"] = map[string]any{"tag": "plain_text", "content": s}
		}
	}
	return map[string]any{
		"schema": "2.0",
		// JSON 2.0's config is exactly {update_multi: true}. The 1.0-era
		// wide_screen_mode is not part of this schema and, in strict mode, an
		// unknown property is rejected rather than ignored.
		"config": cardConfig(full),
		"header": header,
		"body":   map[string]any{"elements": r.cardElements(msg, full, charts)},
	}
}

// cardConfig is the global card config. The full card uses the documented
// "compact" width (400px on desktop / iPad; phones are unaffected): the rows
// are short and the default 600px only added empty space. The fallback card
// keeps the bare config so it can never be rejected over this field.
func cardConfig(full bool) map[string]any {
	if !full {
		return map[string]any{"update_multi": true}
	}
	return map[string]any{"update_multi": true, "width_mode": "compact"}
}

// cardHeader is the title bar: title, colour by the worst severity, and no
// tag list. Every row already carries its own state tag, so header tags only
// repeated them and cost a line.
func (r *Renderer) cardHeader(msg domain.Message) map[string]any {
	title := r.titleFor(msg)
	// An alert card is titled by its kind (still overridable with
	// NOTIFY_TITLE_<KIND>), not by one alert's own evaluator title, so a
	// single alert and a batch read the same.
	if isAlertNotice(msg) {
		bare := msg
		bare.Title = ""
		title = r.titleFor(bare)
	}
	// "额度告急" overstates one exhausted account whose channel still has room.
	if isAlertNotice(msg) && title == defaultTitles[msg.Kind] && msg.Kind == string(domain.AlertQuotaExhausted) && r.urgentAlertsLeaveHeadroom(msg) {
		title = "单号额度用满"
	}
	// A CPA cooldown that runs to the upstream reset time is a long pause / waiting
	// for upstream reset, rather than a proven full account exhaustion.
	if isAlertNotice(msg) && title == defaultTitles[msg.Kind] && msg.Kind == string(domain.AlertRateLimited) {
		all := true
		for _, a := range msg.Alerts {
			if a.Kind == domain.AlertRateLimited && !quotaCooldown(a) {
				all = false
			}
		}
		if all {
			title = "限流冷却中 (时间较长)"
		}
	}
	template := r.cardColor(msg)
	if msg.Actionable != nil && msg.Actionable.HeaderColor != "" {
		template = msg.Actionable.HeaderColor
	}
	return map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": title},
		"template": template,
	}
}

// brandTagColor maps a provider to a non-status brand color text_tag.
// Red and green are never brand colours, so they cannot be mistaken for a
// state. Claude is orange by request; state tags always carry their text.
func brandTagColor(p domain.ProviderKind) string {
	switch p {
	case domain.ProviderClaude:
		// Anthropic's own orange, by request; state tags still carry text,
		// so an orange name never stands alone as a warning.
		return "orange"
	case domain.ProviderAntigravity:
		return "blue"
	case domain.ProviderCodex:
		return "turquoise"
	case domain.ProviderGeminiCLI:
		return "indigo"
	case domain.ProviderOllama:
		return "purple"
	default:
		return "neutral"
	}
}

// cardElements assembles the body under the component budget.
func (r *Renderer) cardElements(msg domain.Message, full, charts bool) []any {
	// The actionable-mode summary carries its own view model; its absence is
	// what keeps every legacy card pixel-identical.
	if msg.Actionable != nil {
		return r.actionableSummaryElements(msg, full)
	}
	// Every card — full or fallback, query or alert — is built from the same
	// blocks; the fallback only drops the collapsible panel.
	if isAlertNotice(msg) {
		return r.alertNoticeElements(msg, full)
	}
	// A failed collection with no data at all still says so in the same
	// error block, rather than a blank or stale-looking card.
	if msg.Notice != "" && (msg.Report == nil || len(msg.Report.Providers) == 0) {
		return r.failedElements(msg, nil, false)
	}
	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		return r.blockElements(msg, full)
	}
	_ = charts
	var head []any
	if k := r.kicker(msg); k != "" {
		head = append(head, md(k))
	}
	if msg.Notice != "" {
		head = append(head, md("⚠️ **"+oneLine(msg.Notice)+"**"))
	}
	if msg.Report != nil && msg.Report.Degraded {
		head = append(head, md("⚠️ **"+r.degradedLine(msg.Report)+"**"))
	}
	if len(head) > 0 {
		head = append(head, hr())
	}

	// Alert evidence stays on the card even when a report is present: an alert
	// is often the reason the notification fired, and its facts carry the
	// timestamps and provenance the compact channel lines folded away.
	var alerts []any
	if len(msg.Alerts) > 0 {
		alerts = append(alerts, hr())
		if msg.Report == nil {
			// Alert-only notifications have no channel section, so the title
			// goes here; the header already names the kind.
			alerts = append(alerts, md("**告警明细**"))
		}
		alerts = append(alerts, r.cardAlerts(msg.Alerts)...)
	}

	tail := []any{hr()}
	if buttons := r.cardButtons(msg); buttons != nil {
		tail = append(tail, buttons)
	}
	tail = append(tail, r.cardFooter(msg))

	var blocks []any
	hasChannels := msg.Report != nil && len(msg.Report.Providers) > 0
	if hasChannels {
		views, _ := r.summarizeAll(msg.Report, msg.Detailed)
		// One chart for the whole card, not one per channel: a per-channel
		// 2:1 chart eats hundreds of vertical pixels each and pushes the text
		// to the edge. A single fixed-height chart carries every credential,
		// and only when there are few enough bars to stay readable.
		if charts {
			if ch, ok := r.cardChart(views); ok {
				blocks = append(blocks, ch)
			}
		}
		budget := maxCardElements - countElements(head) - countElements(alerts) - countElements(tail) - countElements(blocks)
		if budget < 1 {
			budget = 1
		}
		truncated := false
		for i, v := range views {
			if i > 0 {
				blocks = append(blocks, hr())
				budget--
			}
			block, cost := r.channelBlock(v, msg.Detailed, full)
			if cost > budget {
				// Not enough room for the full block: keep the status line,
				// which is the part a reader scans, and say so.
				block, cost = []any{md(r.cardChannelLine(v))}, 1
				truncated = true
			}
			if cost > budget {
				truncated = true
				break
			}
			blocks = append(blocks, block...)
			budget -= cost
		}
		if truncated {
			blocks = append(blocks, md("（部分渠道因组件数量上限未展开，可用 @我 <渠道名> 查看）"))
		}
	}

	out := make([]any, 0, len(head)+len(blocks)+len(alerts)+len(tail))
	out = append(out, head...)
	out = append(out, blocks...)
	out = append(out, alerts...)
	out = append(out, tail...)
	if !hasChannels && len(msg.Alerts) == 0 && len(head) == 0 {
		out = append([]any{md("暂无可展示的额度信息。")}, out...)
	}
	return out
}

// channelBlock is one channel's elements plus its component cost.
func (r *Renderer) channelBlock(v providerView, detailed, full bool) ([]any, int) {
	out := []any{md(r.cardChannelLine(v))}
	cost := 1
	if full {
		if sample, rest, tightest, ok := v.isomorphicHealthyGroup(); ok {
			out = append(out, md(r.formatSampleCredential(sample)))
			cost++
			panel, rows := r.isomorphicHealthyPanel(v, rest, tightest, detailed)
			out = append(out, panel)
			cost += 1 + rows
			return out, cost
		}
		if len(v.rows) > 0 {
			for _, normalLine := range r.normalSiblingLines(v) {
				out = append(out, md(normalLine))
				cost++
			}
			panel, rows := r.channelPanel(v, detailed)
			out = append(out, panel)
			cost += 1 + rows
			return out, cost
		}
		// All credentials are normal/limited, but not isomorphic (or single account):
		// Render each credential as a compact single line with its window remainders.
		for _, row := range v.allRows {
			out = append(out, md(r.formatCompactNormalCredential(row)))
			cost++
		}
		return out, cost
	}
	if !full {
		// Degraded card: the same evidence as markdown, no panel/table, in the
		// card's remaining caliber.
		for _, line := range r.cardEvidenceLines(v, detailed) {
			out = append(out, md(line))
			cost++
		}
	}
	return out, cost
}

// isomorphicHealthyGroup checks if a provider has >= 2 credentials, all normal/limited,
// and all sharing an identical non-empty set of window names.
func (v providerView) isomorphicHealthyGroup() (sample credRow, rest []credRow, tightestRem string, ok bool) {
	if v.abnormal > 0 || v.failure != "" || len(v.allRows) < 2 {
		return credRow{}, nil, "", false
	}
	sig := v.allRows[0].WindowSignature()
	if sig == "" {
		return credRow{}, nil, "", false
	}
	for _, cr := range v.allRows {
		if needsDetail(cr.state) || cr.WindowSignature() != sig {
			return credRow{}, nil, "", false
		}
	}
	var maxUsed *float64
	for _, cr := range v.allRows[1:] {
		for _, w := range cr.windows {
			if w.UsedPercent != nil {
				if maxUsed == nil || *w.UsedPercent > *maxUsed {
					val := *w.UsedPercent
					maxUsed = &val
				}
			}
		}
	}
	remText := "100%"
	if maxUsed != nil {
		remText = trimPercent(remainingOf(*maxUsed))
	} else if v.worst != nil {
		remText = trimPercent(remainingOf(*v.worst))
	}
	return v.allRows[0], v.allRows[1:], remText, true
}

// formatSampleCredential renders the first healthy credential as a sample with 4-tier hierarchy.
func (r *Renderer) formatSampleCredential(row credRow) string {
	var lines []string
	head := "  · `" + row.label + "`"
	if row.plan != "" {
		head += " " + inlineTag("neutral", row.plan+" · 样本")
	} else {
		head += " " + inlineTag("neutral", "样本")
	}
	head += " " + inlineTag(stateTagColor(row.state), shortStateLabel(row.state))
	lines = append(lines, head)
	lines = append(lines, r.groupAndWindowLines(row, true)...)
	return strings.Join(lines, "\n")
}

// isomorphicHealthyPanel builds the collapsible_panel for the remaining isomorphic normal accounts.
func (r *Renderer) isomorphicHealthyPanel(v providerView, rest []credRow, tightestRem string, detailed bool) (map[string]any, int) {
	title := fmt.Sprintf("另 %d 个号结构相同，均正常（最紧 %s）", len(rest), tightestRem)
	var lines []string
	for _, row := range rest {
		lines = append(lines, r.formatCompactNormalCredential(row))
	}
	if len(lines) > maxPanelRows {
		lines = lines[:maxPanelRows]
	}
	inner := []any{md(strings.Join(lines, "\n"))}
	panel := map[string]any{
		"tag":      "collapsible_panel",
		"expanded": false,
		"header": map[string]any{
			"title":          map[string]any{"tag": "plain_text", "content": title},
			"vertical_align": "center",
		},
		"elements": inner,
	}
	return panel, len(inner)
}

// normalSiblingLines returns compact one-line summaries for normal credentials in a channel that also has abnormal credentials.
func (r *Renderer) normalSiblingLines(v providerView) []string {
	if v.abnormal == 0 && len(v.rows) == 0 {
		return nil
	}
	var out []string
	for _, row := range v.allRows {
		if !needsDetail(row.state) {
			out = append(out, r.formatCompactNormalCredential(row))
		}
	}
	return out
}

// knownScopeGroupTranslations maps known upstream group/scope display names to Chinese.
var knownScopeGroupTranslations = map[string]string{
	"gemini models":         "Gemini 模型",
	"gemini model":          "Gemini 模型",
	"claude and gpt models": "Claude / GPT 模型",
	"claude and gpt model":  "Claude / GPT 模型",
	"claude and gpt":        "Claude / GPT 模型",
}

// humanizeScopeGroup translates known group names to Chinese, preserving unknown names verbatim.
func humanizeScopeGroup(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if tr, ok := knownScopeGroupTranslations[strings.ToLower(raw)]; ok {
		return tr
	}
	return raw
}

// shortNormalizedWindowName extracts only the normalized window period/name
// (e.g. "5小时", "周", "7天", "Fable 5") without repeating group names or upstream raw bucket text.
func shortNormalizedWindowName(w domain.QuotaWindow) string {
	lbl := strings.TrimSpace(w.DisplayLabel())
	lower := strings.ToLower(w.Name + " " + lbl)
	switch {
	case strings.Contains(lower, "five_hour") || strings.Contains(lower, "5h") || strings.Contains(lower, "5小时") || strings.Contains(lower, "five hour"):
		return "5小时"
	case strings.Contains(lower, "seven_day") || strings.Contains(lower, "7d") || strings.Contains(lower, "7天"):
		return "7天"
	case strings.Contains(lower, "fable"):
		return "Fable 5"
	case strings.Contains(lower, "weekly") || strings.Contains(lower, "周"):
		return "周"
	case strings.Contains(lower, "daily") || strings.Contains(lower, "日"):
		return "日"
	case strings.Contains(lower, "monthly") || strings.Contains(lower, "月"):
		return "月"
	case strings.Contains(lower, "hourly") || strings.Contains(lower, "小时"):
		return "小时"
	default:
		// Fallback: strip "账号 · " and group prefixes if any
		lbl = strings.TrimPrefix(lbl, "账号 · ")
		if parts := strings.Split(lbl, " · "); len(parts) > 1 {
			return parts[len(parts)-1]
		}
		return lbl
	}
}

// shortWindowName produces a concise window label for compact single-line normal accounts:
// e.g. "5h", "7d", "周", "Fable 5", "Gemini 5h".
func shortWindowName(w domain.QuotaWindow) string {
	lbl := w.DisplayLabel()
	lower := strings.ToLower(w.Name + " " + lbl + " " + w.ScopeID)
	prefix := ""
	if w.Scope.Normalized() == domain.ScopeGroup || w.Scope.Normalized() == domain.ScopeModel {
		id := domain.HumanizeIdentifier(w.ScopeID)
		idLower := strings.ToLower(id)
		if strings.Contains(idLower, "gemini") {
			// Gemini is the only Antigravity group shown (Claude/GPT is
			// ignored by default), so its windows read plainly "5h" / "7d".
			prefix = ""
		} else if strings.Contains(idLower, "claude") || strings.Contains(idLower, "gpt") {
			prefix = "Claude/GPT "
		} else if id != "" && !strings.Contains(idLower, "fable") {
			prefix = id + " "
		}
	}
	switch {
	case strings.Contains(lower, "five_hour") || strings.Contains(lower, "5h") || strings.Contains(lower, "5小时") || strings.Contains(lower, "five hour"):
		return prefix + "5h"
	case strings.Contains(lower, "seven_day") || strings.Contains(lower, "7d") || strings.Contains(lower, "7天"):
		return prefix + "7d"
	case strings.Contains(lower, "fable"):
		return "Fable 5"
	case strings.Contains(lower, "weekly") || strings.Contains(lower, "周") || strings.Contains(lower, "week"):
		// One vocabulary everywhere: a weekly window is "7d", like Claude's.
		return prefix + "7d"
	default:
		lbl = strings.TrimPrefix(lbl, "账号 · ")
		return prefix + lbl
	}
}

// windowRank orders windows the same way everywhere: 5h, then 7d, then the
// rest (model-specific windows such as Fable 5).
func windowRank(w domain.QuotaWindow) int {
	name := shortWindowName(w)
	switch {
	case strings.HasSuffix(name, "5h"):
		return 0
	case strings.HasSuffix(name, "7d"):
		return 1
	default:
		return 2
	}
}

// orderedWindows keeps each model group together, in the order the groups
// first appear, and puts 5h before 7d inside every group.
func orderedWindows(in []domain.QuotaWindow) []domain.QuotaWindow {
	out := append([]domain.QuotaWindow(nil), in...)
	group := map[string]int{}
	for _, w := range out {
		key := string(w.Scope.Normalized()) + ":" + w.ScopeID
		if _, ok := group[key]; !ok {
			group[key] = len(group)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		gi := group[string(out[i].Scope.Normalized())+":"+out[i].ScopeID]
		gj := group[string(out[j].Scope.Normalized())+":"+out[j].ScopeID]
		if gi != gj {
			return gi < gj
		}
		return windowRank(out[i]) < windowRank(out[j])
	})
	return out
}

// formatCompactNormalCredential renders a normal credential on a single line with all window remainders:
// e.g. "  · `antigravity-leacanva92 · ****d79a`（Gemini 周 剩 95.3% ｜ ...）"
func (r *Renderer) formatCompactNormalCredential(row credRow) string {
	var winParts []string
	for _, w := range row.windows {
		wName := shortWindowName(w)
		if w.UsedPercent != nil {
			winParts = append(winParts, fmt.Sprintf("%s 剩 %s", wName, remainingPct(*w.UsedPercent)))
		} else {
			winParts = append(winParts, fmt.Sprintf("%s 额度可用", wName))
		}
	}
	head := "  · `" + row.label + "`"
	if row.plan != "" {
		head += " " + inlineTag("neutral", row.plan)
	}
	if len(winParts) > 0 {
		return head + "（" + strings.Join(winParts, " ｜ ") + "）"
	}
	return head + "（" + shortStateLabel(row.state) + "）"
}

// groupAndWindowLines renders Tier 3 (model/group scope) and Tier 4 (windows) for a credential.
func (r *Renderer) groupAndWindowLines(row credRow, detailed bool) []string {
	windows := row.windows
	folded := 0
	if !detailed && !needsDetail(row.state) {
		if w, ok := tightestWindow(windows); ok {
			folded = len(windows) - 1
			windows = []domain.QuotaWindow{w}
		}
	}
	// Group windows by Scope/ScopeID if any are group/model scoped
	type scopeGroup struct {
		title   string
		windows []domain.QuotaWindow
	}
	var groups []scopeGroup
	groupIdx := map[string]int{}
	hasScoped := false
	for _, w := range windows {
		sc := w.Scope.Normalized()
		if sc == domain.ScopeGroup || sc == domain.ScopeModel {
			hasScoped = true
		}
		key := string(sc) + ":" + w.ScopeID
		idx, exists := groupIdx[key]
		if !exists {
			title := ""
			if sc == domain.ScopeGroup || sc == domain.ScopeModel {
				humanizedID := humanizeScopeGroup(domain.HumanizeIdentifier(w.ScopeID))
				if sc == domain.ScopeGroup {
					title = "分组 " + humanizedID
				} else {
					title = "模型 " + humanizedID
				}
			}
			idx = len(groups)
			groupIdx[key] = idx
			groups = append(groups, scopeGroup{title: title})
		}
		groups[idx].windows = append(groups[idx].windows, w)
	}

	var out []string
	if hasScoped {
		for _, g := range groups {
			if g.title != "" {
				out = append(out, "    ▸ "+g.title)
			}
			for _, w := range g.windows {
				line := "      · " + r.tier4WindowCell(w) + "  " + r.resetText(w) + " 刷新"
				if w.LimitReached && (w.UsedPercent == nil || *w.UsedPercent < 100) {
					line += "  [上游标记已达上限]"
				}
				if row.stale {
					line += " ［旧值 · 最后成功 " + r.lastSuccessText(row.lastOK) + "］"
				}
				out = append(out, line)
			}
		}
	} else {
		for i, w := range windows {
			line := "    · " + r.remainingCell(w) + "  " + r.resetText(w) + " 刷新"
			if w.LimitReached && (w.UsedPercent == nil || *w.UsedPercent < 100) {
				line += "  [上游标记已达上限]"
			}
			if row.stale && i == 0 {
				line += " ［旧值 · 最后成功 " + r.lastSuccessText(row.lastOK) + "］"
			}
			if i == 0 && folded > 0 {
				line += "  · 另 " + strconv.Itoa(folded) + " 个窗口"
			}
			out = append(out, line)
		}
	}
	return out
}

// tier4WindowCell renders Tier 4 window name with only normalized period name and percentage,
// e.g. "5小时 88.6%" or "周 92.0%".
func (r *Renderer) tier4WindowCell(w domain.QuotaWindow) string {
	wName := shortNormalizedWindowName(w)
	cell := wName + " " + remainingPctOrUnknown(w.UsedPercent)
	if w.RemainingAmount != "" {
		cell += "  剩余 " + w.RemainingAmount
	}
	return cell
}

// cardEvidenceLines is the panel's evidence as markdown lines per
// credential. It never drops a fact the table would have shown: the window name,
// exact remaining share, applicability, reset time, upstream limit marker,
// failure reason, error code, and last success all survive.
func (r *Renderer) cardEvidenceLines(v providerView, detailed bool) []string {
	var out []string
	for _, row := range v.rows {
		head := "  · `" + row.label + "`"
		if row.plan != "" {
			head += " " + inlineTag("neutral", row.plan)
		}
		head += "  " + shortStateLabel(row.state)
		if row.failure != domain.FailureNone && row.failure != "" {
			if row.code != "" {
				head += " · " + row.code + " · " + domain.FailureReason(row.failure)
			} else {
				head += "  依据：" + domain.FailureReason(row.failure)
			}
		} else if row.code != "" {
			head += " · " + row.code
		}
		if len(row.windows) == 0 {
			out = append(out, head+"  最后成功 "+r.lastSuccessText(row.lastOK))
			continue
		}
		if !needsDetail(row.state) && !detailed {
			out = append(out, r.formatCompactNormalCredential(row))
			continue
		}
		// Abnormal credential or detailed query: expand all windows (Tier 3 & Tier 4)
		// Also keep first window summary on the header line for compact readability and existing test assertions
		w0 := row.windows[0]
		if !detailed {
			// The headline window is the 5h one when present, unless a non-5h
			// window is binding; it is the same pick the channel big number
			// uses, so the row and its channel cannot disagree.
			if hw, ok := headlineWindow(row.windows); ok {
				w0 = hw
			}
		}
		first := head + "  " + r.remainingCell(w0) + "  " + r.resetText(w0) + " 刷新"
		if w0.LimitReached && (w0.UsedPercent == nil || *w0.UsedPercent < 100) {
			first += "  [上游标记已达上限]"
		}
		if row.stale {
			first += " ［旧值 · 最后成功 " + r.lastSuccessText(row.lastOK) + "］"
		}
		out = append(out, first)
		if len(row.windows) > 1 {
			out = append(out, r.groupAndWindowLines(row, true)...)
		}
	}
	return out
}

// cardChannelLine is one channel's headline on the card: counts, a status
// text_tag, and the tightest remaining share.
// The channel name is styled with its non-status brand color via <font color='...'>**name**</font>
// without any redundant brand tag.
func (r *Renderer) cardChannelLine(v providerView) string {
	var parts []string
	parts = append(parts, strconv.Itoa(v.total)+" 个号")
	parts = append(parts, v.breakdown...)
	if v.worstState != "" {
		parts = append(parts, inlineTag(stateTagColor(v.worstState), shortStateLabel(v.worstState)))
	}
	switch {
	case v.worst != nil && v.worstStale:
		parts = append(parts, "最紧剩余 "+remainingPct(*v.worst)+"（旧值）")
	case v.worst != nil:
		parts = append(parts, "最紧剩余 "+remainingPct(*v.worst))
	default:
		parts = append(parts, "用量未上报")
	}
	if v.unknownScope {
		// Unknown applicability is preserved, never rounded to account scope.
		parts = append(parts, "含范围未知窗口")
	}
	if len(v.plans) > 0 {
		parts = append(parts, "套餐 "+strings.Join(v.plans, "/"))
	}
	if v.caution != "" {
		parts = append(parts, v.caution)
	}
	if note := v.coverageNote(); note != "" {
		parts = append(parts, note)
	}
	if v.failure != "" {
		parts = append(parts, "渠道错误："+v.failure)
	}
	bColor := brandTagColor(v.provider)
	brandedName := fmt.Sprintf("**<font color='%s'>%s</font>**", bColor, v.name)
	line := brandedName + " · " + strings.Join(parts, " · ")
	if v.advice != "" && (len(v.rows) > 0 || v.failure != "") {
		line += " · 建议：" + toRemainingCaliber(v.advice)
	}
	return line
}

// barKind groups one channel's credential bars for the card chart. Channels
// are ordered abnormal-first so the actionable group sits at the top.
type barKind struct {
	name  string
	bars  []credBar
	class domain.StateClass
}

// shortCredentialName is the compact chart label: the alias, else the short id,
// else the provider. It deliberately omits the "alias · shortid" form, which is
// truncated on the chart axis and stops identifying the account.
func shortCredentialName(c domain.Credential) string {
	switch {
	case strings.TrimSpace(c.Alias) != "":
		// The brand is next to it on every card: drop the "claude-" style prefix.
		return domain.TrimProviderPrefix(strings.TrimSpace(c.Alias), c.Provider)
	case strings.TrimSpace(c.ShortID) != "":
		return strings.TrimSpace(c.ShortID)
	default:
		return string(c.Provider)
	}
}

// cardChart renders ONE card-level chart of every credential's remaining share.
//
// One chart, not one per channel: a per-channel 2:1 chart occupies hundreds of
// vertical pixels and pushes the text off-screen. A fixed height keeps it
// bounded; the labels are short so they are not truncated; and the bars are
// deliberately NOT coloured per credential — the rainbow conveyed nothing. The
// values still carry a status tag so the colour a reader does see maps to the
// state, and the value/text can never disagree. ok=false means there are too
// many bars to be readable and the caller should fall back to text.
func (r *Renderer) cardChart(views []providerView) (map[string]any, bool) {
	var kinds []barKind
	total := 0
	for _, v := range views {
		if len(v.bars) == 0 {
			continue
		}
		kinds = append(kinds, barKind{
			name:  r.displayName(v.provider),
			bars:  v.bars,
			class: domain.ClassOf(v.worstState),
		})
		total += len(v.bars)
	}
	if total == 0 || total > maxChartBars {
		return nil, false
	}
	// Abnormal channels first so the grouped bars put the actionable group at
	// the top of the chart.
	sort.SliceStable(kinds, func(i, j int) bool {
		return chartClassRank(kinds[i].class) < chartClassRank(kinds[j].class)
	})

	values := make([]any, 0, total)
	for _, k := range kinds {
		for _, b := range k.bars {
			if b.used == nil {
				continue
			}
			rem := remainingFraction(*b.used)
			// No group-separator rows: a 0-value separator renders as an empty
			// bar, which is indistinguishable from the "no data / 0% left" case
			// this chart must not fake. The credential alias already names the
			// channel in this deployment (codex-…, claude-…), so the bars stay
			// self-describing without a spacer.
			values = append(values, map[string]any{
				"type":  b.name,
				"value": rem,
				"text":  fmt.Sprintf("%.0f%%", rem*100),
			})
		}
	}
	if len(values) == 0 {
		return nil, false
	}
	return map[string]any{
		"tag": "chart",
		// A fixed height replaces aspect_ratio: a 2:1 chart grows with the card
		// width, so on a wide screen a single chart was ~350px tall.
		"height": chartHeight,
		"chart_spec": map[string]any{
			"type":      "linearProgress",
			"direction": "horizontal",
			// seriesField is intentionally absent: with no series every bar
			// shares one colour, instead of VChart assigning each credential a
			// meaningless hue.
			"data":   map[string]any{"values": values},
			"xField": "value",
			"yField": "type",
			"axes": []any{
				map[string]any{"orient": "left", "domainLine": map[string]any{"visible": false}},
			},
		},
	}, true
}

func chartClassRank(c domain.StateClass) int {
	switch c {
	case domain.ClassAbnormal:
		return 0
	case domain.ClassLimited:
		return 1
	default:
		return 2
	}
}

// channelPanel is the abnormal-channel detail, folded away unless the reader
// asked for a single channel. It returns the panel and the number of inner
// components it contributes, for the budget.
//
// The panel deliberately contains ONLY markdown. Feishu's collapsible_panel
// rejects several element types inside it — `table` among them, despite the
// official docs only naming `form` — and a single unsupported child fails the
// whole card in strict JSON 2.0. Folding matters more than table layout, so the
// same per-credential evidence is rendered as compact markdown lines instead.
func (r *Renderer) channelPanel(v providerView, detailed bool) (map[string]any, int) {
	lines := r.cardEvidenceLines(v, detailed)
	if len(lines) > maxPanelRows {
		lines = lines[:maxPanelRows]
	}
	title := v.name + " 异常明细（" + strconv.Itoa(len(v.rows)) + " 个号）"
	if detailed {
		title = v.name + " 全部窗口（" + strconv.Itoa(len(v.rows)) + " 个号）"
	}
	inner := []any{md(strings.Join(lines, "\n"))}
	if v.advice != "" {
		inner = append(inner, md("处置建议："+toRemainingCaliber(v.advice)))
	}
	panel := map[string]any{
		"tag":      "collapsible_panel",
		"expanded": detailed,
		"header": map[string]any{
			"title":          map[string]any{"tag": "plain_text", "content": title},
			"vertical_align": "center",
		},
		"elements": inner,
	}
	return panel, len(inner)
}

// remainingCell is one window's table cell: readable name, exact remaining
// share, applicability, and an upstream remaining count if any.
func (r *Renderer) remainingCell(w domain.QuotaWindow) string {
	cell := w.DisplayLabel() + " " + remainingPctOrUnknown(w.UsedPercent)
	if w.RemainingAmount != "" {
		cell += "  剩余 " + w.RemainingAmount
	}
	return cell
}

// remainingPctOrUnknown keeps a missing reading visibly missing: nil is never
// printed as 0% or 100%.
func remainingPctOrUnknown(v *float64) string {
	if v == nil {
		return "未上报"
	}
	return remainingPct(*v)
}

// cardButtons is the action row. There is no manual refresh: collection is
// scheduler-driven. The only button left is the CPA link, shown when a
// credential is actually broken and the URL is configured. When there is
// nothing to show it returns nil, so no empty column_set is emitted.
func (r *Renderer) cardButtons(msg domain.Message) map[string]any {
	u := r.cpaPageURL()
	if u == "" || !hasInvalidCredential(msg) {
		return nil
	}
	// horizontal_spacing is omitted: the only safe values are px sizes, and the
	// default (8px) is what we want anyway. "default" appears in some official
	// sample code but is absent from the field's documented enumeration, and
	// strict mode cannot gamble on it.
	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "none",
		"columns": []any{columnElement(map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": "去 CPA"},
			"type": "default",
			"behaviors": []any{
				map[string]any{"type": "open_url", "default_url": u},
			},
		})},
	}
}

// alertButtons is the action row of an alert card: only the CPA link, when a
// credential is broken. A refresh would replace the alert with a full query
// card and lose what the alert was about.
func (r *Renderer) alertButtons(msg domain.Message) map[string]any {
	u := r.cpaPageURL()
	if u == "" || !hasInvalidCredential(msg) {
		return nil
	}
	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "none",
		"columns": []any{columnElement(map[string]any{
			"tag":       "button",
			"text":      map[string]any{"tag": "plain_text", "content": "去 CPA"},
			"type":      "default",
			"behaviors": []any{map[string]any{"type": "open_url", "default_url": u}},
		})},
	}
}

func columnElement(el map[string]any) map[string]any {
	return map[string]any{"tag": "column", "width": "auto", "elements": []any{el}}
}

// cardFooter is the data time, in small grey text.
func (r *Renderer) cardFooter(msg domain.Message) map[string]any {
	line := "数据 " + r.reportTime(msg)
	if msg.Freshness != "" {
		line += " · " + msg.Freshness
	}
	return map[string]any{"tag": "markdown", "content": grey(line), "text_size": "notation"}
}

// hasInvalidCredential reports whether any credential is confirmed broken, the
// condition for surfacing the CPA link.
func hasInvalidCredential(msg domain.Message) bool {
	for _, a := range msg.Alerts {
		if a.Kind == domain.AlertCredential {
			return true
		}
	}
	if msg.Report == nil {
		return false
	}
	for _, p := range msg.Report.Providers {
		for _, st := range p.States {
			if st == domain.StateInvalid {
				return true
			}
		}
	}
	return false
}

// providerWorst is the most severe state of a provider, from the evaluator's
// States map with WorstState as the fallback. Empty means no judgement was
// made and must not be treated as abnormal.
func providerWorst(p domain.ProviderReport) domain.CredentialState {
	worst := p.WorstState
	for _, st := range p.States {
		if worst == "" || breakdownRank(st) > breakdownRank(worst) {
			worst = st
		}
	}
	return worst
}

// stateTagColor maps a credential state onto a text_tag colour. Colour and
// text always appear together, so meaning never depends on colour alone.
func stateTagColor(s domain.CredentialState) string {
	switch s {
	case domain.StateHealthy:
		return "green"
	case domain.StateNotice, domain.StateWarning:
		return "orange"
	case domain.StateExhausted, domain.StateInvalid:
		return "red"
	case domain.StateLimited:
		return "blue"
	case domain.StateSuspect:
		return "orange"
	default:
		// Stale / unknown / unset: no usable data.
		return "neutral"
	}
}

func inlineTag(color, text string) string {
	return fmt.Sprintf("<text_tag color='%s'>%s</text_tag>", color, text)
}

// usedPercentPattern matches the used-percentage wording that reaches us from
// the evaluator's advice and alert text (internal/evaluate builds those strings
// and is out of this package's scope to change). Rewriting them here is what
// lets every user-visible surface speak the remaining caliber without touching
// the threshold logic that produced them.
var usedPercentPattern = regexp.MustCompile(`已用\s*([0-9]+(?:\.[0-9]+)?)\s*%`)

// toRemainingCaliber rewrites evaluator-authored prose from the used caliber
// into the remaining caliber, so one card never mixes "已用 94%" with
// "剩余 6%". Only a number followed by % is touched; credit amounts and every
// other number are left verbatim.
func toRemainingCaliber(s string) string {
	if s == "" {
		return s
	}
	s = usedPercentPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := usedPercentPattern.FindStringSubmatch(m)
		used, err := strconv.ParseFloat(sub[1], 64)
		if err != nil {
			return m
		}
		return "剩余 " + trimPercent(remainingOf(used))
	})
	// Wording that only makes sense on the used scale is restated, not dropped:
	// a threshold is a floor on remaining, so "below the line" says the same
	// thing a reader can act on.
	s = strings.ReplaceAll(s, "达到通知阈值", "低于提醒线")
	s = strings.ReplaceAll(s, "越过阈值", "剩余已低于提醒线")
	return s
}

// trimPercent prints a percentage without a trailing ".0", matching the
// evaluator's own pct formatting.
func trimPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}

// remainingOf is the display caliber: remaining = 100 - used, never negative.
func remainingOf(used float64) float64 {
	rem := 100 - used
	if rem < 0 {
		rem = 0
	}
	return rem
}

func remainingPct(used float64) string {
	return fmt.Sprintf("%.1f%%", remainingOf(used))
}

// remainingFraction is the chart's 0–1 value, clamped so a negative or
// over-100 upstream figure cannot produce an invalid bar.
func remainingFraction(used float64) float64 {
	rem := remainingOf(used) / 100
	if rem > 1 {
		return 1
	}
	return rem
}

// countElements counts every component a card contributes, including nested
// panel elements, column elements and table rows, because that is what Feishu
// counts against its ceiling.
func countElements(els []any) int {
	n := 0
	for _, e := range els {
		n++
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if sub, ok := m["elements"].([]any); ok {
			n += countElements(sub)
		}
		if cols, ok := m["columns"].([]any); ok {
			for _, c := range cols {
				if cm, ok := c.(map[string]any); ok {
					if se, ok := cm["elements"].([]any); ok {
						n += countElements(se)
					}
				}
			}
		}
		if rows, ok := m["rows"].([]any); ok {
			n += len(rows)
		}
	}
	return n
}

// kicker is the first-screen conclusion: who to use more, who to ease off, who
// must re-authenticate.
func (r *Renderer) kicker(msg domain.Message) string {
	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		views, _ := r.summarizeAll(msg.Report, msg.Detailed)
		if c := r.cardConclusion(msg.Report, views); c != "" {
			// The closing ** is followed by a space on purpose: a closing
			// delimiter adjacent to any non-whitespace character (a colon, a
			// letter) is not recognised as an emphasis boundary, which is why
			// "**结论：**Claude" printed the asterisks literally.
			return "**结论：** " + c
		}
	}
	// Alert-only notifications still lead with the single most actionable line.
	var reauth, ease []string
	for _, a := range msg.Alerts {
		switch a.Kind {
		case domain.AlertCredential:
			reauth = append(reauth, r.displayName(a.Credential.Provider))
		case domain.AlertQuotaExhausted:
			credName := shortCredentialName(a.Credential)
			if a.Scope == domain.ScopeAccount {
				ease = append(ease, credName+"（账号）")
			} else {
				ease = append(ease, credName+"（仅 "+domain.ScopeText(a.Scope, a.ScopeID)+"）")
			}
		}
	}
	var clauses []string
	if len(ease) > 0 {
		clauses = append(clauses, strings.Join(dedup(ease), "、")+" 的所示范围额度耗尽，请查看各 scope 说明")
	}
	if len(reauth) > 0 {
		clauses = append(clauses, strings.Join(dedup(reauth), "、")+" 需要重新登录")
	}
	if len(clauses) == 0 {
		return ""
	}
	return "**结论：** " + strings.Join(clauses, "；")
}

// reportTime is the generation time of the report the card was built from.
func (r *Renderer) reportTime(msg domain.Message) string {
	if msg.Report == nil {
		// An alert sent before any collection (e.g. from the cooldown
		// watcher) is dated by when it was observed.
		var latest time.Time
		for _, a := range msg.Alerts {
			if a.OccurredAt.After(latest) {
				latest = a.OccurredAt
			}
		}
		if latest.IsZero() {
			return "未知"
		}
		return r.formatTime(latest)
	}
	return r.formatTime(msg.Report.GeneratedAt)
}

func (r *Renderer) cardAlerts(alerts []domain.Alert) []any {
	out := []any{}
	var normal []domain.Alert
	for _, a := range alerts {
		if a.Severity == domain.SeverityInfo && a.Kind != domain.AlertQuotaThreshold && a.Scope == "" {
			normal = append(normal, a)
			continue
		}
		out = append(out, md(r.alertTextBlock(a)))
	}
	for _, a := range normal {
		out = append(out, md(fmt.Sprintf("-%s：%s（%s）", shortCredentialName(a.Credential), oneLine(a.Title), evidenceLabel(a.Evidence))))
	}
	return out
}

func (r *Renderer) alertTextBlock(a domain.Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s·%s] %s**\n", severityLabel(a.Severity), evidenceLabel(a.Evidence), shortCredentialName(a.Credential))
	if a.Title != "" {
		fmt.Fprintf(&b, "%s\n", toRemainingCaliber(oneLine(a.Title)))
	}
	if a.Detail != "" {
		fmt.Fprintf(&b, "%s\n", toRemainingCaliber(oneLine(a.Detail)))
	}
	for _, f := range a.Facts {
		fmt.Fprintf(&b, "- 证据：%s\n", toRemainingCaliber(oneLine(f)))
	}
	if a.Advice != "" {
		fmt.Fprintf(&b, "处置建议：%s\n", toRemainingCaliber(oneLine(a.Advice)))
	}
	if u := r.cpaPageURL(); u != "" {
		fmt.Fprintf(&b, "管理页：%s\n", u)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cardColor picks a header colour from the most severe signal present.
func (r *Renderer) cardColor(msg domain.Message) string {
	// A failed collection is an error, whatever the old numbers say.
	if !isAlertNotice(msg) && collectFailed(msg) {
		return "red"
	}
	// An alert card is coloured by the worst alert kind, not by its severity:
	// a threshold alert carries "接近上限" even at Info severity, and stale or
	// unknown evidence is neutral. The kind is the semantic, severity is only
	// how loudly it was raised.
	if isAlertNotice(msg) {
		return r.alertHeaderColor(msg)
	}
	worst := domain.SeverityInfo
	for _, a := range msg.Alerts {
		if severityRank(a.Severity) > severityRank(worst) {
			worst = a.Severity
		}
	}
	// A channel is judged by its usable accounts: one exhausted account next
	// to a sibling with headroom is a warning, not an emergency. Red is kept
	// for a channel with nothing left.
	if msg.Report != nil {
		views, _ := r.summarizeAll(msg.Report, false)
		for _, v := range views {
			sev := domain.SeverityInfo
			switch {
			case channelUnusable(v):
				sev = domain.SeverityUrgent
			case v.abnormal > 0:
				sev = domain.SeverityWarn
			default:
				switch channelState(v) {
				case domain.StateWarning, domain.StateNotice, domain.StateSuspect:
					sev = domain.SeverityWarn
				}
			}
			if severityRank(sev) > severityRank(worst) {
				worst = sev
			}
		}
	}
	switch worst {
	case domain.SeverityUrgent:
		return "red"
	case domain.SeverityWarn:
		return "orange"
	default:
		return "blue"
	}
}

// alertHeaderColor maps an alert notice onto a header colour by the worst alert
// tag it carries, so the header agrees with the rows and does not depend on the
// alert's severity:
//
//	exhausted / credential                         red
//	threshold / suspect / rate-limited             orange
//	reset / recovered / rate-limit-cleared         green
//	stale / unknown                                blue (neutral)
//
// The exhausted/credential red is downgraded to orange when the channel still
// has a usable account: the account is out, the channel is not.
func (r *Renderer) alertHeaderColor(msg domain.Message) string {
	worst := tagColorRank("")
	for _, a := range msg.Alerts {
		color, _ := alertPhrase(a)
		if rank := tagColorRank(color); rank > worst {
			worst = rank
		}
	}
	if worst == tagColorRank("red") && r.urgentAlertsLeaveHeadroom(msg) {
		worst = tagColorRank("orange")
	}
	switch worst {
	case tagColorRank("red"):
		return "red"
	case tagColorRank("orange"):
		return "orange"
	case tagColorRank("green"):
		return "green"
	default:
		// "blue" is the neutral header template in Feishu; stale or unknown
		// evidence must not look like a recovery (green) or a problem (orange).
		return "blue"
	}
}

// tagColorRank orders the alert tag colours by alarm: red over orange over
// green, with neutral (stale/unknown) last.
func tagColorRank(c string) int {
	switch c {
	case "red":
		return 3
	case "orange":
		return 2
	case "green":
		return 1
	default:
		return 0
	}
}

func severityRank(s domain.Severity) int {
	switch s {
	case domain.SeverityUrgent:
		return 3
	case domain.SeverityWarn:
		return 2
	default:
		return 1
	}
}

func md(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": content}
}

func div(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func hr() map[string]any { return map[string]any{"tag": "hr"} }

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
