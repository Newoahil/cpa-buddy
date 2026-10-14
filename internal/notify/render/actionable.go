package render

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

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
			line := "  · " + pendingLabel(p.Kind) + " " + r.displayName(p.Provider)
			if p.Alias != "" || p.ShortID != "" {
				line += " " + shortCredentialName(domain.Credential{Alias: p.Alias, ShortID: p.ShortID, Provider: p.Provider})
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
				line += " " + r.formatShort(*t.ResetAt) + " 重置"
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// This file is the single home of the ALERT_MODE=actionable card rendering.
//
// The classification lives in internal/notify/policy and reaches render only as
// domain.ActionableCard, a plain view model. Everything visual about the new
// summary and the quiet-hours merge is here, so @designer can polish it without
// touching the policy or the evaluation engine. The immediate alert cards for
// channel_unavailable and monitor_down reuse the existing alert-notice block
// system; only their phrase/colour mapping is added next to it.

// actionableSummaryElements renders the new scheduled digest:
//
//	结论
//	待处理（N）：类型 · 账号 · 开始时间 · 建议动作
//	紧张渠道：渠道 · 最好的账号剩余 X% · 几点重置
//	▸ 各账号明细（折叠，沿用现有面板）
func (r *Renderer) actionableSummaryElements(msg domain.Message, full bool) []any {
	card := msg.Actionable
	out := []any{}
	if card.Conclusion != "" {
		// The closing "** " keeps the emphasis boundary intact (see kicker).
		out = append(out, md("**结论：** "+card.Conclusion))
	}

	if len(card.Pending) > 0 {
		out = append(out, md("**待处理（"+strconv.Itoa(len(card.Pending))+"）**"))
		for _, p := range card.Pending {
			out = append(out, md(r.pendingRow(p)))
		}
	}

	if len(card.Tight) > 0 {
		out = append(out, md("**紧张渠道**"))
		for _, t := range card.Tight {
			out = append(out, md(r.tightRow(t)))
		}
	}

	// The per-account detail panel and the CPA button are reused verbatim: the
	// new card keeps the same drill-down a reader already knows.
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

// pendingRow renders one open pending item as a single line.
func (r *Renderer) pendingRow(p domain.PendingRow) string {
	line := inlineTag(pendingTagColor(p.Kind), pendingLabel(p.Kind)) + " " + r.displayName(p.Provider)
	if p.Alias != "" || p.ShortID != "" {
		line += " " + shortCredentialName(domain.Credential{Alias: p.Alias, ShortID: p.ShortID, Provider: p.Provider})
	}
	if !p.Since.IsZero() {
		line += " · 开始 " + r.formatShort(p.Since)
	}
	if p.Action != "" {
		line += "\n" + grey(p.Action)
	}
	return line
}

// tightRow renders one tight channel: channel, the best account's remaining
// share, and when it refreshes.
func (r *Renderer) tightRow(t domain.TightChannel) string {
	line := r.brandName(t.Provider)
	if t.BestRemaining != nil {
		line += " · 最好的账号剩余 " + fmt.Sprintf("%.1f%%", *t.BestRemaining)
	}
	if t.ResetAt != nil {
		line += " · " + r.formatShort(*t.ResetAt) + " 重置"
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
	case domain.AlertMonitorDown, domain.AlertOMCOutage:
		return "orange"
	default:
		return "neutral"
	}
}

// actionableAlertPhrase is the tag for the new channel-level immediate alerts.
// It reports ok=false for kinds the legacy table already handles.
func actionableAlertPhrase(k domain.AlertKind) (color, phrase string, ok bool) {
	switch k {
	case domain.AlertChannelUnavailable:
		return "red", "渠道不可用", true
	case domain.AlertMonitorDown:
		return "orange", "监控停摆", true
	default:
		return "", "", false
	}
}
