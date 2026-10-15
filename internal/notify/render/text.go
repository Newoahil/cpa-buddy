package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// Text renders the plain-text form used by chat replies, the webhook and logs.
//
// It is deliberately short: a normal channel is one line, and only credentials
// that need action are expanded. Shortness is achieved by removing repetition
// and internal identifiers, never by dropping evidence — every percentage,
// reset time, source and confidence grade that was in the report is still here.
func (r *Renderer) Text(msg domain.Message) string {
	msg = r.FilterMessage(msg)
	var b strings.Builder
	title := r.titleFor(msg)
	b.WriteString("【" + title + "】")
	if msg.Report != nil && !msg.Report.GeneratedAt.IsZero() {
		b.WriteString(r.formatTime(msg.Report.GeneratedAt))
	}
	if msg.Freshness != "" {
		b.WriteString(" · " + msg.Freshness)
	}
	b.WriteByte('\n')

	if msg.Notice != "" {
		b.WriteString("⚠️ " + oneLine(msg.Notice) + "\n")
	}
	if msg.Report != nil && msg.Report.Degraded {
		b.WriteString(r.degradedLine(msg.Report) + "\n")
	}
	if msg.Body != "" {
		b.WriteString(msg.Body)
		b.WriteByte('\n')
	}
	if msg.Actionable != nil {
		b.WriteString(r.actionableText(msg.Actionable))
	}

	if msg.Report != nil && len(msg.Report.Providers) > 0 {
		b.WriteString(r.textProviders(msg.Report, msg.Detailed))
	}
	if len(msg.Alerts) > 0 {
		b.WriteString(r.textAlerts(msg.Alerts))
	}
	if strings.TrimSpace(b.String()) == strings.TrimSpace("【"+title+"】") {
		b.WriteString("暂无可展示的额度信息。\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// degradedLine is the reader-facing warning that this picture is incomplete.
//
// Notes are deduplicated: the evaluator groups identical wording, but a report
// assembled from several cycles can still repeat, and three copies of the same
// paragraph is what buried the one line that mattered.
func (r *Renderer) degradedLine(rep *domain.Report) string {
	line := "⚠️ 本次数据不完整（降级）"
	if notes := dedup(append([]string(nil), rep.Notes...)); len(notes) > 0 {
		line += "：" + strings.Join(notes, "；")
	}
	return line
}

func (r *Renderer) textProviders(rep *domain.Report, detailed bool) string {
	var b strings.Builder
	views, pad := r.summarizeAll(rep, detailed)
	if conclusion := r.conclusion(rep, views); conclusion != "" {
		b.WriteString("结论：" + conclusion + "\n")
	}
	b.WriteByte('\n')

	for _, v := range views {
		for _, line := range r.providerLines(v, pad, detailed) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// providerLines is the full block for one provider, shared by text and card so
// the two surfaces cannot drift apart.
func (r *Renderer) providerLines(v providerView, pad int, detailed bool) []string {
	lines := []string{v.headline(pad, detailed)}
	lines = append(lines, r.rowLines(v, detailed)...)
	lines = append(lines, r.extraLines(v)...)
	// The coverage caveat lives here rather than in the conclusion: it
	// qualifies these numbers, and a verdict crowded with disclaimers stops
	// being a verdict.
	if note := v.coverageNote(); note != "" {
		lines = append(lines, "  · "+note)
	}
	if v.failure != "" {
		lines = append(lines, "  · 渠道错误："+v.failure)
	}
	// Advice is attached only where the reader has something to do. For a
	// folded, healthy channel the conclusion line already said it, and
	// repeating it per provider is the noise this replaces.
	if v.advice != "" && (len(v.rows) > 0 || v.failure != "") {
		lines = append(lines, "  · 建议："+v.advice)
	}
	return lines
}

func (r *Renderer) textAlerts(alerts []domain.Alert) string {
	var b strings.Builder
	b.WriteString("\n告警：\n")
	for _, a := range alerts {
		fmt.Fprintf(&b, "[%s·%s] %s %s\n", severityLabel(a.Severity), evidenceLabel(a.Evidence), a.Credential.Name(), oneLine(a.Title))
		if a.Detail != "" {
			fmt.Fprintf(&b, "  %s\n", oneLine(a.Detail))
		}
		for _, f := range a.Facts {
			fmt.Fprintf(&b, "  - %s\n", oneLine(f))
		}
		if a.Advice != "" {
			fmt.Fprintf(&b, "  建议：%s\n", oneLine(a.Advice))
		}
	}
	if u := r.cpaPageURL(); u != "" {
		fmt.Fprintf(&b, "管理页：%s\n", u)
	}
	return b.String()
}

// summarizeAll projects every provider once and returns the name column width
// so the folded lines align in a monospaced client.
func (r *Renderer) summarizeAll(rep *domain.Report, detailed bool) ([]providerView, int) {
	views := make([]providerView, 0, len(rep.Providers))
	pad := 0
	for _, p := range rep.Providers {
		v := r.summarize(rep, p, detailed)
		if w := displayWidth(v.name); w > pad {
			pad = w
		}
		views = append(views, v)
	}
	return views, pad
}

// conclusion answers exactly one question: what should I do right now.
//
// It names who to stop using, who to switch to, and what needs fixing. Every
// qualification — coverage limits, evidence grade, scope caveats — belongs on
// the provider's own line instead, because a disclaimer inside the verdict
// makes the verdict unreadable without making it more true.
//
// remaining selects the display caliber of the quoted percentage: the Feishu
// card speaks the remaining share (matching the CPA console), while the
// channel-agnostic text keeps the used share that its own lines print. The
// verdict and the numbers below it must never disagree.
func (r *Renderer) conclusion(rep *domain.Report, views []providerView) string {
	return r.conclusionCaliber(rep, views, false)
}

// cardConclusion is the conclusion in the card's remaining caliber.
func (r *Renderer) cardConclusion(rep *domain.Report, views []providerView) string {
	return r.conclusionCaliber(rep, views, true)
}

func (r *Renderer) conclusionCaliber(rep *domain.Report, views []providerView, remaining bool) string {
	var avoid []string
	var preferred, fallback []providerView
	var blocked []string

	// tightest renders the verdict's headline number in the caller's caliber,
	// so the "省着点用（最紧 X）" clause matches the channel line below it.
	tightest := func(used float64) string {
		if remaining {
			return "最紧剩余 " + remainingPct(used)
		}
		return "最紧 " + fmt.Sprintf("%.1f%%", used)
	}

	for _, v := range views {
		switch v.grade() {
		case unusable:
			clause := v.name + " 先别用了"
			var why []string
			if len(v.breakdown) > 0 {
				why = append(why, strings.Join(v.breakdown, " · "))
			}
			if v.recoverAt != nil {
				why = append(why, "最早 "+r.formatShort(*v.recoverAt)+" 恢复")
			}
			if len(why) > 0 {
				clause += "（" + strings.Join(why, "，") + "）"
			}
			avoid = append(avoid, clause)
		case ample:
			preferred = append(preferred, v)
		case tight:
			fallback = append(fallback, v)
		default:
			blocked = append(blocked, v.name+"："+undecidableReason(v))
		}
	}

	// Rank the channels we can recommend. Account-wide evidence outranks
	// scope-limited evidence at equal headroom, because it proves more; within
	// the same evidence strength the roomiest wins.
	rank := func(list []providerView) []string {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].scopedOnly != list[j].scopedOnly {
				return !list[i].scopedOnly
			}
			return *list[i].worst < *list[j].worst
		})
		out := make([]string, 0, len(list))
		for _, v := range list {
			out = append(out, v.name)
		}
		return out
	}
	useNames := rank(preferred)
	rank(fallback)

	var clauses []string
	clauses = append(clauses, dedup(avoid)...)
	// A channel past halfway is still usable, but it is an instruction, not a
	// status: say what to do with it and keep the exact number.
	for _, v := range fallback {
		if r.formal() {
			clauses = append(clauses, v.name+" 用量偏高，请控制（"+tightest(*v.worst)+"）")
		} else {
			clauses = append(clauses, v.name+" 省着点用（"+tightest(*v.worst)+"）")
		}
	}

	switch {
	case len(useNames) > 0 && len(clauses) > 0:
		clauses = append(clauses, "改用 "+strings.Join(useNames, " 或 "))
	case len(useNames) > 0:
		if r.formal() {
			clauses = append(clauses, "优先使用 "+strings.Join(useNames, "，其次 "))
		} else {
			clauses = append(clauses, "优先用 "+strings.Join(useNames, "，其次 "))
		}
	case len(clauses) == 0:
		// Nothing gradeable and nothing to act on: say so plainly and name what
		// is missing, instead of emitting an ambiguous hedge.
		reason := "缺少可用的额度数据"
		if len(blocked) > 0 {
			reason = strings.Join(dedup(blocked), "；")
		}
		return "暂时给不出建议（" + reason + "）"
	}
	return strings.Join(clauses, "，")
}

// undecidableReason names the specific thing that blocks a verdict, so
// "暂时给不出建议" is always accompanied by what would unblock it.
func undecidableReason(v providerView) string {
	switch {
	case v.total == 0:
		return "没有凭证"
	case v.unknownScope:
		return "窗口未申明适用范围"
	case v.worst == nil:
		return "未上报用量"
	case v.worstStale:
		return "只有过期读数"
	default:
		return "数据不足"
	}
}
