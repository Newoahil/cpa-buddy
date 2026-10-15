package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// nowFunc is the clock hook used by duration and relative-time calculations.
// It defaults to time.Now and can be overridden in tests.
var nowFunc = time.Now

// actionableText is the plain-text projection of the actionable summary, used
// by the webhook and logs. It mirrors the card's sections without any markdown.
func (r *Renderer) actionableText(card *domain.ActionableCard) string {
	var b strings.Builder
	if card.Conclusion != "" {
		b.WriteString("结论：" + card.Conclusion + "\n")
	}
	if len(card.Pending) > 0 {
		b.WriteString("待处理（" + strconv.Itoa(len(card.Pending)) + "）：\n")
		for _, p := range card.Pending {
			line := "  · [" + pendingLabel(p.Kind) + "] " + r.displayName(p.Provider)
			if p.Alias != "" || p.ShortID != "" {
				line += " " + shortCredentialName(domain.Credential{Alias: p.Alias, ShortID: p.ShortID, Provider: p.Provider})
			}
			if !p.Since.IsZero() {
				if dur := r.formatDurationSince(p.Since, nowFunc()); dur != "" {
					line += " · " + dur
				}
			}
			if p.Action != "" {
				line += "：" + p.Action
			}
			b.WriteString(line + "\n")
		}
	}
	if len(card.Tight) > 0 {
		b.WriteString("紧张渠道：\n")
		for _, t := range card.Tight {
			line := "  · " + r.displayName(t.Provider)
			if t.BestRemaining != nil {
				line += fmt.Sprintf(" 最好的账号剩余 %.1f%%", *t.BestRemaining)
			}
			if t.ResetAt != nil {
				line += " · " + r.formatResetTime(*t.ResetAt, nowFunc())
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// actionableSummaryElements renders actionable cards:
//
// 1. Immediate alerts (channel_unavailable, credential_invalid, monitor_down):
//   - Advice first: tell the user what to do immediately
//   - Timing: start time, duration, and expected recovery
//   - Account detail panel & CPA buttons
//
// 2. Quiet hours merge digest (quiet_digest):
//   - Merged verdict: night-time summary
//   - Pending items list (reusing pendingRow)
//   - Tight channels (if any)
//
// 3. Scheduled digest (actionable_summary):
//   - One-line conclusion
//   - Pending items (only if present): [Tag] Provider Account · Duration \n Advice
//   - Tight channels: Provider · Best Account Remaining % · Reset time (Absolute + Relative)
//   - Account detail panel & CPA buttons
func (r *Renderer) actionableSummaryElements(msg domain.Message, full bool) []any {
	card := msg.Actionable
	base := r.messageBaseTime(msg)
	out := []any{}

	// Case 1: Immediate alert cards
	if isImmediateAlertKind(msg.Kind) {
		// Advice first: clear guidance without numbers crowding the view
		if card.Conclusion != "" {
			out = append(out, md("**建议动作：** "+card.Conclusion))
		} else if len(card.Pending) > 0 && card.Pending[0].Action != "" {
			out = append(out, md("**建议动作：** "+card.Pending[0].Action))
		}

		// Timing: start time, duration, and expected recovery time
		if len(card.Pending) > 0 {
			p := card.Pending[0]
			var timeParts []string
			if !p.Since.IsZero() {
				dur := r.formatDurationSince(p.Since, base)
				if dur != "" {
					timeParts = append(timeParts, fmt.Sprintf("**开始时间：** %s（%s）", r.formatShort(p.Since), dur))
				} else {
					timeParts = append(timeParts, fmt.Sprintf("**开始时间：** %s", r.formatShort(p.Since)))
				}
			}
			if len(card.Tight) > 0 && card.Tight[0].ResetAt != nil {
				timeParts = append(timeParts, fmt.Sprintf("**预计恢复：** %s", r.formatResetTime(*card.Tight[0].ResetAt, base)))
			} else if p.Kind == domain.AlertCredential {
				timeParts = append(timeParts, "**预计恢复：** 需人工处理（更新凭证后恢复）")
			} else if p.Kind == domain.AlertChannelUnavailable {
				timeParts = append(timeParts, "**预计恢复：** 无自动恢复时间（等待配额重置或切换账号）")
			} else if p.Kind == domain.AlertMonitorDown {
				timeParts = append(timeParts, "**预计恢复：** 采集链路恢复后自动解除")
			}
			if len(timeParts) > 0 {
				out = append(out, md(strings.Join(timeParts, "\n")))
			}
		}

		if len(card.Pending) > 1 {
			out = append(out, md("**相关待处理（"+strconv.Itoa(len(card.Pending))+"）**"))
			for _, p := range card.Pending {
				out = append(out, md(r.pendingRowAt(p, base)))
			}
		}

		if full && msg.Report != nil && len(msg.Report.Providers) > 0 {
			views, _ := r.summarizeAll(msg.Report, false)
			if panel := r.detailPanel(views, false, fmt.Sprintf("各账号明细 · %d 个号", totalAccounts(views))); panel != nil {
				out = append(out, panel)
			}
		}
		if buttons := r.cardButtons(msg); buttons != nil {
			out = append(out, buttons)
		}
		return append(out, r.cardFooter(msg))
	}

	// Case 2 & 3: Quiet hours merge & Scheduled digest
	isQuietDigest := msg.Kind == string(domain.AlertQuietDigest) || strings.Contains(msg.Title, "夜间") || strings.Contains(msg.Title, "免打扰")

	if card.Conclusion != "" {
		out = append(out, md("**结论：** "+card.Conclusion))
	} else if isQuietDigest && len(card.Pending) > 0 {
		out = append(out, md(fmt.Sprintf("**夜间共积压 %d 项待处理：**", len(card.Pending))))
	}

	if len(card.Pending) > 0 {
		heading := "**待处理（" + strconv.Itoa(len(card.Pending)) + "）**"
		if isQuietDigest {
			heading = "**待处理列表（" + strconv.Itoa(len(card.Pending)) + "）**"
		}
		out = append(out, md(heading))
		for _, p := range card.Pending {
			out = append(out, md(r.pendingRowAt(p, base)))
		}
	}

	if len(card.Tight) > 0 {
		out = append(out, md("**紧张渠道**"))
		for _, t := range card.Tight {
			out = append(out, md(r.tightRowAt(t, base)))
		}
	}

	if full && msg.Report != nil && len(msg.Report.Providers) > 0 {
		views, _ := r.summarizeAll(msg.Report, false)
		if panel := r.detailPanel(views, false, fmt.Sprintf("各账号明细 · %d 个号", totalAccounts(views))); panel != nil {
			out = append(out, panel)
		}
	}
	if buttons := r.cardButtons(msg); buttons != nil {
		out = append(out, buttons)
	}
	return append(out, r.cardFooter(msg))
}

// pendingRow renders one open pending item as a single line with an advice sub-line.
func (r *Renderer) pendingRow(p domain.PendingRow) string {
	return r.pendingRowAt(p, nowFunc())
}

func (r *Renderer) pendingRowAt(p domain.PendingRow, base time.Time) string {
	line := inlineTag(pendingTagColor(p.Kind), pendingLabel(p.Kind)) + " " + r.displayName(p.Provider)
	if p.Alias != "" || p.ShortID != "" {
		line += " " + shortCredentialName(domain.Credential{Alias: p.Alias, ShortID: p.ShortID, Provider: p.Provider})
	}
	if !p.Since.IsZero() {
		if dur := r.formatDurationSince(p.Since, base); dur != "" {
			line += " · " + dur
		}
	}
	if p.Action != "" {
		act := p.Action
		if !strings.HasPrefix(act, "建议") && !strings.HasPrefix(act, "请") {
			act = "建议：" + act
		}
		line += "\n" + grey(act)
	}
	return line
}

// tightRow renders one tight channel: channel, the best account's remaining
// share, and when it refreshes using absolute + relative time.
func (r *Renderer) tightRow(t domain.TightChannel) string {
	return r.tightRowAt(t, nowFunc())
}

func (r *Renderer) tightRowAt(t domain.TightChannel, base time.Time) string {
	line := r.brandName(t.Provider)
	if t.BestRemaining != nil {
		line += " · 最好的账号剩余 " + fmt.Sprintf("%.1f%%", *t.BestRemaining)
	}
	if t.ResetAt != nil {
		line += " · " + r.formatResetTime(*t.ResetAt, base)
	}
	return line
}

// pendingLabel is the short human name of a pending item kind.
func pendingLabel(k domain.AlertKind) string {
	switch k {
	case domain.AlertCredential:
		return "凭证失效"
	case domain.AlertChannelUnavailable:
		return "渠道不可用"
	case domain.AlertMonitorDown:
		return "监控停摆"
	case domain.AlertOMCOutage:
		return "OMC 中断"
	case domain.AlertQuietDigest:
		return "免打扰合并"
	default:
		return string(k)
	}
}

// pendingTagColor colours a pending item's tag. Colour and text always appear
// together, so meaning never depends on colour alone.
func pendingTagColor(k domain.AlertKind) string {
	switch k {
	case domain.AlertCredential, domain.AlertChannelUnavailable:
		return "red"
	case domain.AlertMonitorDown, domain.AlertOMCOutage, domain.AlertQuietDigest:
		return "orange"
	default:
		return "neutral"
	}
}

// actionableAlertPhrase is the tag for the new channel-level immediate alerts.
// It reports ok=false for kinds the legacy table already handles.
func actionableAlertPhrase(k domain.AlertKind) (color, phrase string, ok bool) {
	switch k {
	case domain.AlertCredential:
		return "red", "凭证失效", true
	case domain.AlertChannelUnavailable:
		return "red", "渠道不可用", true
	case domain.AlertMonitorDown:
		return "orange", "监控停摆", true
	case domain.AlertQuietDigest:
		return "orange", "免打扰结束", true
	default:
		return "", "", false
	}
}

func isImmediateAlertKind(kind string) bool {
	switch domain.AlertKind(kind) {
	case domain.AlertChannelUnavailable, domain.AlertCredential, domain.AlertMonitorDown:
		return true
	default:
		return false
	}
}

func (r *Renderer) messageBaseTime(msg domain.Message) time.Time {
	if msg.Report != nil && !msg.Report.GeneratedAt.IsZero() {
		return msg.Report.GeneratedAt
	}
	return nowFunc()
}

// formatDuration formats a duration into plain Chinese.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	m := int(math.Round(d.Minutes()))
	if m < 60 {
		return strconv.Itoa(m) + " 分钟"
	}
	h := m / 60
	remM := m % 60
	if h < 24 {
		if remM == 0 {
			return strconv.Itoa(h) + " 小时"
		}
		return fmt.Sprintf("%d 小时 %d 分钟", h, remM)
	}
	days := h / 24
	remH := h % 24
	if remH == 0 {
		return strconv.Itoa(days) + " 天"
	}
	return fmt.Sprintf("%d 天 %d 小时", days, remH)
}

// formatDurationSince returns "持续 X 分钟" or "刚刚".
func (r *Renderer) formatDurationSince(since time.Time, base time.Time) string {
	if since.IsZero() {
		return ""
	}
	diff := base.Sub(since)
	if diff <= 0 {
		return "刚刚"
	}
	return "持续 " + formatDuration(diff)
}

// formatResetTime formats ResetAt into "13:20（2 小时后）重置" or "10-15 02:00（8 小时后）重置".
func (r *Renderer) formatResetTime(resetAt time.Time, base time.Time) string {
	loc := r.location()
	resetInLoc := resetAt.In(loc)
	baseInLoc := base.In(loc)

	abs := resetInLoc.Format("15:04")
	if resetInLoc.Year() != baseInLoc.Year() || resetInLoc.YearDay() != baseInLoc.YearDay() {
		abs = resetInLoc.Format("01-02 15:04")
	}

	diff := resetAt.Sub(base)
	if diff <= 0 {
		return abs + "（已到期）重置"
	}
	return abs + "（" + formatDuration(diff) + "后）重置"
}
