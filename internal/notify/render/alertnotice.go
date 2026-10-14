package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// An alert notice says what changed and nothing else: one block per changed
// account, in the same block system as the query card, with the window, its
// remaining share and when it refreshes (or the error code). The overview of
// every channel belongs to queries and the scheduled digest.

// maxAlertLines bounds the blocks on one notice; the rest are in the panel.
const maxAlertLines = 10

// isAlertNotice reports a notification driven by state changes. The first-run
// bootstrap is excluded: it is the initial inventory and keeps the overview.
func isAlertNotice(msg domain.Message) bool {
	if len(msg.Alerts) == 0 {
		return false
	}
	for _, a := range msg.Alerts {
		if a.Kind == domain.AlertBootstrap {
			return false
		}
	}
	return true
}

// alertGroup collapses the alerts for one credential and one kind into a single
// row: an account can cross several model scopes at once, and five rows saying
// the same thing bury the one that matters. It is keyed by Credential.Key (never
// the display alias), kind, and distinct quota window identity.
type alertGroup struct {
	representative domain.Alert
	// models counts the alerts in a group that has no account-scope member, so
	// the row can say "含 N 个模型" instead of naming one scope and hiding the
	// rest.
	models int
}

// alertWindowIdentity extracts the window identity from an alert's structured facts.
// Alerts for genuinely different quota windows (e.g. 5h vs 7d) have different window
// identities, so they are not deduped together. Alerts with no window fact (e.g.
// cooldown episodes, status recovery) return "" and collapse by credential and kind.
func alertWindowIdentity(a domain.Alert) string {
	for _, f := range a.Facts {
		if strings.HasPrefix(f, "窗口: ") {
			label := strings.TrimSpace(strings.TrimPrefix(f, "窗口: "))
			if a.ScopeID != "" {
				return label + "/" + a.ScopeID
			}
			if sc := a.Scope.Normalized(); sc != "" && sc != domain.ScopeAccount {
				return label + "/" + string(sc)
			}
			return label
		}
	}
	return ""
}

// alertGroups collapses alerts by credential key, kind, and quota window identity,
// preserving order. The caller sorts by severity first; the representative is the
// account-scope member when there is one, else the first (most severe) member.
func alertGroups(alerts []domain.Alert) []alertGroup {
	type groupID struct {
		key    string
		kind   domain.AlertKind
		window string
	}
	index := map[groupID]int{}
	var out []alertGroup
	for _, a := range alerts {
		id := groupID{key: a.Credential.Key, kind: a.Kind, window: alertWindowIdentity(a)}
		i, ok := index[id]
		if !ok {
			index[id] = len(out)
			out = append(out, alertGroup{representative: a, models: 1})
			continue
		}
		g := &out[i]
		g.models++
		repAccount := g.representative.Scope.Normalized() == domain.ScopeAccount
		if !repAccount && a.Scope.Normalized() == domain.ScopeAccount {
			g.representative = a
		}
	}
	// A group that carries an account-scope alert does not need the model tally.
	for i := range out {
		if out[i].representative.Scope.Normalized() == domain.ScopeAccount {
			out[i].models = 0
		}
	}
	return out
}

func (r *Renderer) alertNoticeElements(msg domain.Message, full bool) []any {
	alerts := append([]domain.Alert(nil), msg.Alerts...)
	sort.SliceStable(alerts, func(i, j int) bool {
		return severityRank(alerts[i].Severity) > severityRank(alerts[j].Severity)
	})
	groups := alertGroups(alerts)

	snaps := snapshotIndex(msg.Report)
	var out []any
	// The alert stands, but say when the data behind it is partial.
	if msg.Report != nil && msg.Report.Degraded {
		out = append(out, md(grey(oneLine(r.degradedLine(msg.Report)))))
	}
	// One-line verdict first: what changed and what to do about it. It carries
	// the account so the rows below need not repeat the instruction. When it is
	// present the old grey "建议改用…" tail is dropped: it said the same thing
	// in a second place.
	if s := r.alertConclusion(msg, groups); s != "" {
		out = append(out, md(s))
	}
	shown := 0
	for _, g := range groups {
		if shown == maxAlertLines {
			out = append(out, md(grey(fmt.Sprintf("另有 %d 条变化，见下方明细", len(groups)-shown))))
			break
		}
		spec := r.alertBlock(g.representative, snaps, msg.Report)
		if g.models > 1 {
			// Collapsed model-only changes: name one scope (already in the row)
			// and count the rest rather than inventing a scope-wide verdict.
			spec.sub = []string{appendScopeCount(spec.sub, "含 "+strconv.Itoa(g.models)+" 个模型")}
		}
		out = append(out, blockStyled(spec, full))
		shown++
	}
	// One explanation per card, not per row.
	if s := alertExplanation(alerts); s != "" {
		out = append(out, md(grey(s)))
	}

	if buttons := r.alertButtons(msg); buttons != nil {
		out = append(out, buttons)
	}
	if full && msg.Report != nil && len(msg.Report.Providers) > 0 {
		views, _ := r.summarizeAll(msg.Report, false)
		if panel := r.detailPanel(views, false, fmt.Sprintf("明细 · %d 个号", totalAccounts(views))); panel != nil {
			out = append(out, panel)
		}
	}
	return append(out, r.cardFooter(msg))
}

// appendScopeCount adds one grey context phrase to a block's single sub line,
// keeping the "tag + grey detail" shape the row already has.
func appendScopeCount(sub []string, note string) string {
	if len(sub) == 0 {
		return grey(note)
	}
	return sub[0] + " · " + grey(note)
}

// worsening is an alert that takes capacity away.
func worsening(a domain.Alert) bool {
	switch a.Kind {
	case domain.AlertQuotaReset, domain.AlertRecovered, domain.AlertRateLimitCleared, domain.AlertStale:
		return false
	}
	return a.Severity == domain.SeverityWarn || a.Severity == domain.SeverityUrgent
}

// alertAccountCount returns the number of distinct accounts represented across groups.
func alertAccountCount(groups []alertGroup) int {
	seen := map[string]bool{}
	for _, g := range groups {
		k := g.representative.Credential.Key
		if k == "" {
			k = g.representative.Credential.Alias
		}
		if k == "" {
			k = string(g.representative.Credential.Provider)
		}
		if k != "" {
			seen[k] = true
		}
	}
	return len(seen)
}

// alertConclusion is the one-line verdict at the top of an alert card: the
// account, the window, and what to do. It is deliberately narrow so it never
// invents advice:
//
//   - one confirmed worsening alert with a known scope: name the account and
//     its window, then say what to switch to (or to stop using it when there
//     is no alternative).
//   - one suspected or unknown-scope worsening: the same fact, no instruction.
//   - one recovery / reset / rate-limit-clear: "已恢复" or "已刷新".
//   - several alerts: a count, with the detail below.
//
// Cooldown and credential alerts get no separate line here: their rows, tags
// and the per-card explanation already say everything actionable.
//
// An empty return means no verdict line is shown.
func (r *Renderer) alertConclusion(msg domain.Message, groups []alertGroup) string {
	if len(groups) > 1 {
		accts := alertAccountCount(groups)
		if accts > 1 {
			return "**结论：** " + strconv.Itoa(accts) + " 个账号有变化，详见下方"
		}
		return "**结论：** " + strconv.Itoa(len(groups)) + " 条变化，详见下方"
	}
	if len(groups) == 0 {
		return ""
	}
	a := groups[0].representative
	// A channel-wide change with no account has no single account to name; the
	// rows below carry the title and scope instead.
	if a.Credential.Provider == "" && strings.TrimSpace(a.Credential.Alias) == "" && strings.TrimSpace(a.Credential.ShortID) == "" {
		return ""
	}
	name := r.conclusionAccountName(a.Credential)
	switch a.Kind {
	case domain.AlertQuotaReset:
		return "**结论：** " + name + " 已刷新"
	case domain.AlertRecovered, domain.AlertRateLimitCleared:
		return "**结论：** " + name + " 已恢复"
	case domain.AlertQuotaThreshold, domain.AlertQuotaExhausted:
		// handled below
	default:
		// Cooldown and credential changes are explained by their rows and the
		// per-card explanation; they get no separate verdict line.
		return ""
	}
	if !worsening(a) {
		return ""
	}
	w, ok := r.conclusionWindow(a, msg.Report)
	if !ok || w.UsedPercent == nil {
		return ""
	}
	window := shortWindowName(w)
	// Suspicion and unknown applicability are facts, not instructions: the
	// reader is told what is happening but not to change anything on evidence
	// that does not support it.
	if a.Evidence != domain.EvidenceConfirmed || a.Scope.Normalized() == domain.ScopeUnknown {
		return "**结论：** " + name + " 的 " + window + " 快用完了"
	}
	status := "快用完了"
	if a.Kind == domain.AlertQuotaExhausted || *w.UsedPercent >= 100 {
		status = "已用完"
	}
	base := name + " 的 " + window + " " + status
	targets := r.switchTargets(msg)
	if len(targets) == 0 {
		// No whole channel has headroom, but a sibling account in the same
		// channel does: name that account, which is the concrete move.
		targets = r.siblingAccounts(msg.Report, a.Credential)
	}
	if len(targets) > 0 {
		return "**结论：** " + base + "，改用 " + strings.Join(targets, " / ") + " 暂用"
	}
	return "**结论：** " + base + "，先别用了"
}

// siblingAccounts are the OTHER usable accounts of the alert's provider, named
// by alias. They are the switch targets when no other channel has headroom.
func (r *Renderer) siblingAccounts(rep *domain.Report, c domain.Credential) []string {
	if rep == nil {
		return nil
	}
	for _, p := range rep.Providers {
		if p.Provider != c.Provider {
			continue
		}
		v := r.summarize(rep, p, false)
		self := shortCredentialName(c)
		var names []string
		for _, row := range v.allRows {
			if row.label == self || !usableState(row.state) {
				continue
			}
			names = append(names, row.label)
		}
		return names
	}
	return nil
}

// conclusionAccountName names the account without the masked short id, and
// without the provider prefix the brand already carries (claude-External0.2 →
// External0.2).
func (r *Renderer) conclusionAccountName(c domain.Credential) string {
	name := r.displayName(c.Provider)
	tail := shortCredentialName(c)
	// shortCredentialName falls back to the raw provider string when the
	// credential has no alias or short id; naming the provider twice is noise.
	if tail == "" || tail == string(c.Provider) {
		return name
	}
	return name + " · " + tail
}

// conclusionWindow finds the window the conclusion talks about: the one the
// alert names, else the tightest window the snapshot reported.
func (r *Renderer) conclusionWindow(a domain.Alert, rep *domain.Report) (domain.QuotaWindow, bool) {
	return alertWindow(a, rep)
}

// alertWindow finds the window an alert is about: the one named in the facts,
// else the tightest reading in the snapshot. The fallback is what lets a card
// built from an alert without a matching "窗口:" fact still lead with a number
// (the header subtitle uses it too).
func alertWindow(a domain.Alert, rep *domain.Report) (domain.QuotaWindow, bool) {
	snap, ok := snapshotIndex(rep)[a.Credential.Key]
	if !ok {
		return domain.QuotaWindow{}, false
	}
	if w, found := matchAlertWindow(a, snap.Windows); found && w.UsedPercent != nil {
		return w, true
	}
	return headlineWindow(snap.Windows)
}

// switchTargets are usable things to switch to. A provider is the unit of
// capacity advice, so a worsening alert on one account does NOT disqualify its
// siblings: an exhausted Codex account can still switch to another Codex
// account. Only a provider with no usable account at all is skipped.
func (r *Renderer) switchTargets(msg domain.Message) []string {
	if msg.Report == nil {
		return nil
	}
	views, _ := r.summarizeAll(msg.Report, false)
	var names []string
	for _, v := range orderForBlocks(views) {
		if v.grade() != ample {
			continue
		}
		if _, _, usable := channelHeadroom(v); !usable {
			continue
		}
		names = append(names, v.name)
	}
	return names
}

// alertBlock is one changed account as a block: brand, alias and what
// happened on the left, the number that matters on the right, and one short
// grey line with the window, time and scope.
func (r *Renderer) alertBlock(a domain.Alert, snaps map[string]domain.QuotaSnapshot, rep *domain.Report) blockSpec {
	color, phrase := alertPhrase(a)
	spec := blockSpec{alarm: color == "red"}
	if spec.alarm {
		if _, usable := r.siblingHeadroom(rep, a.Credential); usable {
			// The account is out, its channel is not: no red row.
			spec.alarm = false
		}
	}
	if a.Credential.Key == "" && strings.TrimSpace(a.Credential.Alias) == "" && strings.TrimSpace(a.Credential.ShortID) == "" {
		// Not about one account (e.g. a channel-wide change).
		spec.left = inlineTag(color, phrase)
		var sub []string
		if t := oneLine(a.Title); t != "" {
			sub = append(sub, t)
		}
		if s := alertScopeText(a); s != "" {
			sub = append(sub, s)
		}
		if len(sub) > 0 {
			spec.sub = []string{grey(strings.Join(sub, " · "))}
		}
		return spec
	}
	p := a.Credential.Provider
	// The name line carries only brand and alias: on a phone the right-hand
	// column squeezes it, and a tag on the same line split the alias in two.
	// The status tag leads the grey line instead.
	spec.left = r.brandName(p) + " `" + shortCredentialName(a.Credential) + "`"
	tag := inlineTag(color, phrase)

	// Design C: the number that matters on the right with its time in small
	// grey under it; the left keeps name and tag, plus one grey line only for
	// context a reader acts on (scope, what the channel has left).
	var sub []string
	snap, ok := snaps[a.Credential.Key]
	switch a.Kind {
	case domain.AlertRateLimited:
		// The big spot is the recovery time, not the HTTP status: a reader
		// wants to know when it comes back, and the 429 is only context.
		facts := alertFacts(a)
		if t, err := time.Parse(time.RFC3339, facts[domain.FactPrefixRecovery]); err == nil {
			spec.big, spec.bigColor, spec.bigSub = r.formatShort(t), "red", "被 CPA 暂停"
		} else {
			spec.big, spec.bigColor, spec.bigSub = "已暂停", "red", "被 CPA 暂停"
		}
		if code := facts[domain.FactPrefixStatus]; code != "" {
			sub = append(sub, "状态码 "+code)
		}
	case domain.AlertRateLimitCleared:
		spec.big, spec.bigColor = "已恢复", "green"
		if d := durationFact(alertFacts(a)[domain.FactPrefixDuration]); d != "" {
			spec.bigSub = "停用 " + d
		}
	case domain.AlertCredential:
		if ok && snap.Code != "" {
			spec.big, spec.bigColor = snap.Code, color
		}
		if ok && snap.Failure != domain.FailureNone && snap.Failure != "" {
			spec.bigSub = domain.FailureReason(snap.Failure)
		}
		if p == domain.ProviderOllama {
			sub = append(sub, "需更新 Cookie")
		}
	case domain.AlertStale, domain.AlertSuspect:
		// The big spot is when the data was last good; the error code is
		// context on the grey line, not the headline.
		if ok {
			if snap.LastSuccessAt.IsZero() {
				spec.big, spec.bigColor = "从未成功", "grey"
			} else {
				spec.big, spec.bigColor = "上次成功 "+r.formatShort(snap.LastSuccessAt), "grey"
			}
			if snap.Code != "" {
				sub = append(sub, snap.Code)
			}
		}
	case domain.AlertRecovered:
		spec.big, spec.bigColor = "已恢复", "green"
	default:
		if !ok {
			break
		}
		w, found := matchAlertWindow(a, snap.Windows)
		if !found {
			// No named window matched a current one (older alert, renamed
			// window): fall back to the headline reading (5h, else tightest)
			// with the same layout.
			w, found = headlineWindow(snap.Windows)
		}
		if found && w.UsedPercent != nil {
			// Option B: if BOTH 5h and 7d exist in the SAME credential snapshot and scope,
			// show both side-by-side in a responsive column_set (flex_mode: flow).
			otherW, hasOther := findCompanionWindow(w, snap.Windows)
			if hasOther {
				first, second := w, otherW
				if is5hWindow(otherW) && !is5hWindow(w) {
					first, second = otherW, w
				}
				spec.dualSet = r.buildDualWindowColumnSet(first, second, a, first == w)
				spec.big = ""
				spec.bigColor = ""
				spec.bigSub = ""
			} else {
				spec.big, spec.bigColor = remainingPct(*w.UsedPercent), alertPctColor(a, *w.UsedPercent)
				if txt := r.resetText(w); txt != "未上报" {
					if a.Kind == domain.AlertQuotaExhausted {
						spec.bigSub = shortWindowName(w) + " · " + txt + " 恢复"
					} else {
						spec.bigSub = shortWindowName(w) + " · " + txt + " 刷新"
					}
				} else {
					spec.bigSub = shortWindowName(w)
				}
			}
			break
		}
		// Neither a named nor a tightest window carries a number: list every
		// window so the reader still sees the evidence.
		for _, w := range orderedWindows(snap.Windows) {
			if w.UsedPercent == nil {
				continue
			}
			part := shortWindowName(w) + " 剩 " + remainingPct(*w.UsedPercent)
			if w.Scope.Normalized() == domain.ScopeUnknown {
				// Unknown applicability is evidence; never let it read as account-wide.
				part += "（范围未知）"
			}
			sub = append(sub, part)
		}
	}
	// A refresh observed only after the new window was already well used
	// is not good news: say how much is gone (the poll found it this late).
	if a.Kind == domain.AlertQuotaReset && ok {
		w, found := matchAlertWindow(a, snap.Windows)
		if !found {
			w, found = headlineWindow(snap.Windows)
		}
		if found && w.UsedPercent != nil && *w.UsedPercent >= halfUsed {
			sub = append(sub, "刷新后已用 "+fmt.Sprintf("%.1f%%", *w.UsedPercent))
			spec.bigColor = "orange"
			if *w.UsedPercent >= 90 {
				spec.bigColor = "red"
			}
		}
	}

	// An account that went out of use: say whether its channel still has room.
	if a.Kind == domain.AlertQuotaExhausted || a.Kind == domain.AlertCredential || a.Kind == domain.AlertRateLimited {
		if used, usable := r.siblingHeadroom(rep, a.Credential); usable && used != nil {
			sub = append(sub, "其余号剩 "+remainingPct(*used))
		} else if !usable && rep != nil {
			sub = append(sub, "已无可用号")
		}
	}

	if s := alertScopeText(a); s != "" {
		sub = append(sub, s)
	}
	line := tag
	if len(sub) > 0 {
		line += " " + grey(strings.Join(sub, " · "))
	}
	spec.sub = []string{line}
	return spec
}

// alertExplanation is the one sentence under a card's cooldown blocks: what CPA
// did, and whether anyone has to act. Other kinds explain themselves through
// the tag and numbers.
func alertExplanation(alerts []domain.Alert) string {
	var limited, cleared, longCooldown bool
	for _, a := range alerts {
		switch a.Kind {
		case domain.AlertRateLimited:
			limited = true
			if quotaCooldown(a) {
				longCooldown = true
			}
		case domain.AlertRateLimitCleared:
			cleared = true
		}
	}
	switch {
	case longCooldown:
		return "说明：CPA 预计到点恢复（可能等待上游刷新）；期间请求已自动转流至其余可用号。"
	case limited:
		return "说明：CPA 已暂停使用该号，到点自动恢复；期间请求已自动转给其他可用号。"
	case cleared:
		return "说明：CPA 已恢复使用该号，请求会重新分配过来。"
	}
	return ""
}

// alertScopeText is "仅 模型 X" for a change that applies to one model or
// group only; a scoped recovery never reads as an account-wide one.
func alertScopeText(a domain.Alert) string {
	if sc := a.Scope.Normalized(); a.Scope == "" || sc == domain.ScopeAccount {
		return ""
	}
	scope := "仅 " + domain.ScopeText(a.Scope, a.ScopeID)
	if a.Kind == domain.AlertRecovered || a.Kind == domain.AlertRateLimitCleared {
		scope += "，不代表全账号恢复"
	}
	return scope
}

// siblingHeadroom is the tightest remaining share among the OTHER usable
// accounts of a credential's channel, so an alert about one exhausted account
// can say the channel itself is still fine.
func (r *Renderer) siblingHeadroom(rep *domain.Report, c domain.Credential) (*float64, bool) {
	if rep == nil {
		return nil, false
	}
	for _, p := range rep.Providers {
		if p.Provider != c.Provider {
			continue
		}
		v := r.summarize(rep, p, false)
		var rows []credRow
		for _, row := range v.allRows {
			if row.label != shortCredentialName(c) {
				rows = append(rows, row)
			}
		}
		v.allRows = rows
		used, _, usable := channelHeadroom(v)
		return used, usable
	}
	return nil, false
}

// urgentAlertsLeaveHeadroom reports that every urgent alert is about an
// account whose channel still has usable siblings.
func (r *Renderer) urgentAlertsLeaveHeadroom(msg domain.Message) bool {
	found := false
	for _, a := range msg.Alerts {
		if a.Severity != domain.SeverityUrgent {
			continue
		}
		found = true
		if _, usable := r.siblingHeadroom(msg.Report, a.Credential); !usable {
			return false
		}
	}
	return found
}

// quotaCooldown reports a cooldown alert whose recovery time lies beyond the
// longest backoff CPA applies by itself.
func quotaCooldown(a domain.Alert) bool {
	if a.Kind != domain.AlertRateLimited {
		return false
	}
	at, err := time.Parse(time.RFC3339, alertFacts(a)[domain.FactPrefixRecovery])
	return err == nil && at.Sub(a.OccurredAt) > domain.CPABackoffCap
}

// alertPctColor colours an alert's remaining share. A threshold alert follows
// the value (>=100 red, >=90 orange, else green); exhausted is always red.
func alertPctColor(a domain.Alert, used float64) string {
	switch {
	case a.Kind == domain.AlertQuotaExhausted || used >= 100:
		return "red"
	case used >= 90:
		return "orange"
	default:
		return "green"
	}
}

// alertFacts reads the cooldown watcher's "prefix value" facts.
func alertFacts(a domain.Alert) map[string]string {
	out := map[string]string{}
	for _, f := range a.Facts {
		for _, p := range []string{domain.FactPrefixStatus, domain.FactPrefixRecovery, domain.FactPrefixDuration} {
			if strings.HasPrefix(f, p) {
				out[p] = strings.TrimSpace(strings.TrimPrefix(f, p))
			}
		}
	}
	return out
}

// durationFact turns "12m" into "12 分钟".
func durationFact(v string) string {
	if v == "" {
		return ""
	}
	if d, err := time.ParseDuration(v); err == nil {
		return humanDuration(d)
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "m")); err == nil {
		return humanDuration(time.Duration(n) * time.Minute)
	}
	return v
}

// alertPhrase is the status tag of a change. A suspected diagnosis is always
// labelled as suspected: a flat "凭证失效" would overstate the evidence.
func alertPhrase(a domain.Alert) (color, phrase string) {
	color, phrase = alertKindPhrase(a)
	if quotaCooldown(a) {
		// CPA caps its own backoff at 30 minutes; a cooldown running past
		// that follows the upstream's reset time (long pause / waiting for upstream reset).
		return "orange", "长时间暂停"
	}
	if a.Evidence == domain.EvidenceSuspected && a.Kind != domain.AlertSuspect {
		return "orange", "疑似" + phrase
	}
	return color, phrase
}

func alertKindPhrase(a domain.Alert) (color, phrase string) {
	switch a.Kind {
	case domain.AlertQuotaReset:
		return "green", "额度已刷新"
	case domain.AlertRecovered:
		return "green", "已恢复"
	case domain.AlertQuotaThreshold:
		return "orange", "接近上限"
	case domain.AlertQuotaExhausted:
		return "red", "已用满"
	case domain.AlertCredential:
		return "red", "凭证失效"
	case domain.AlertSuspect:
		return "orange", "疑似异常"
	case domain.AlertStale:
		return "neutral", "数据过期"
	case domain.AlertRateLimited:
		return "orange", "限流冷却中"
	case domain.AlertRateLimitCleared:
		return "green", "已恢复可用"
	default:
		if c, p, ok := actionableAlertPhrase(a.Kind); ok {
			return c, p
		}
		if a.Title != "" {
			return "neutral", a.Title
		}
		return "neutral", "状态变化"
	}
}

// matchAlertWindow finds the window an alert is about. The evaluator names it
// in the first "窗口: " fact; the scope narrows it when several windows share a
// label. DisplayLabel is preferred; Name is a fallback so a future label
// rename cannot orphan the fact.
func matchAlertWindow(a domain.Alert, windows []domain.QuotaWindow) (domain.QuotaWindow, bool) {
	label := ""
	for _, f := range a.Facts {
		if strings.HasPrefix(f, "窗口: ") {
			label = strings.TrimSpace(strings.TrimPrefix(f, "窗口: "))
			break
		}
	}
	if label == "" {
		return domain.QuotaWindow{}, false
	}
	for _, w := range windows {
		if w.DisplayLabel() != label && w.Name != label {
			continue
		}
		if a.ScopeID != "" && w.ScopeID != a.ScopeID {
			continue
		}
		return w, true
	}
	return domain.QuotaWindow{}, false
}

// findCompanionWindow finds the corresponding 5h or 7d quota window within the SAME
// credential and matching relevant scope (e.g. if w is 5h, find 7d; if w is 7d, find 5h).
// It returns false if no matching companion window exists, or if scopes conflict, or if
// scope is unknown.
func findCompanionWindow(w domain.QuotaWindow, windows []domain.QuotaWindow) (domain.QuotaWindow, bool) {
	if w.Scope.Normalized() == domain.ScopeUnknown {
		return domain.QuotaWindow{}, false
	}
	is5h := is5hWindow(w)
	is7d := is7dWindow(w)
	if !is5h && !is7d {
		return domain.QuotaWindow{}, false
	}

	for _, cand := range windows {
		// Must not be the identical window
		if cand.Name == w.Name && cand.DisplayLabel() == w.DisplayLabel() {
			continue
		}
		// Must match the exact scope kind and ScopeID (prevent mixing Gemini group with Claude/GPT group)
		if cand.Scope.Normalized() != w.Scope.Normalized() || cand.ScopeID != w.ScopeID {
			continue
		}
		if is5h && is7dWindow(cand) {
			return cand, true
		}
		if is7d && is5hWindow(cand) {
			return cand, true
		}
	}
	return domain.QuotaWindow{}, false
}

// buildDualWindowColumnSet constructs an Option B responsive side-by-side column_set
// for two companion windows (typically 5h and 7d) within the same account and scope.
// It uses flex_mode: "flow" so the two tiles sit side-by-side on standard/desktop
// viewports and cleanly wrap to vertical stacking on narrow ~375px mobile screens.
func (r *Renderer) buildDualWindowColumnSet(first, second domain.QuotaWindow, a domain.Alert, firstIsTrigger bool) map[string]any {
	col1 := r.buildWindowTileColumn(first, a, firstIsTrigger)
	col2 := r.buildWindowTileColumn(second, a, !firstIsTrigger)
	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "flow",
		"margin":    "6px 0px 0px 0px",
		"columns":   []any{col1, col2},
	}
}

// buildWindowTileColumn builds one bounded column tile for a quota window in Option B.
func (r *Renderer) buildWindowTileColumn(w domain.QuotaWindow, a domain.Alert, isTrigger bool) map[string]any {
	winName := shortWindowName(w)
	if w.Scope.Normalized() == domain.ScopeUnknown {
		winName += "（范围未知）"
	}

	// Line 1: Header (Window Name + Trigger / Status Tag)
	header := "**" + winName + "**"
	if isTrigger {
		header += " " + inlineTag(alertTagColor(a), "触发")
	}

	// Line 2: Big remaining value
	var valLine string
	if w.UsedPercent != nil {
		rem := remainingPct(*w.UsedPercent)
		color := alertValueColor(*w.UsedPercent)
		if isTrigger {
			color = alertPctColor(a, *w.UsedPercent)
			valLine = fmt.Sprintf("**<font color='%s'>剩 %s</font>**", color, rem)
		} else {
			valLine = fmt.Sprintf("<font color='%s'>剩 %s</font>", color, rem)
		}
	} else {
		valLine = grey("未上报")
	}

	// Line 3: Refresh / Recovery time
	var resetLine string
	if txt := r.resetText(w); txt != "未上报" {
		if isTrigger && a.Kind == domain.AlertQuotaExhausted {
			resetLine = grey(txt + " 恢复")
		} else {
			resetLine = grey(txt + " 刷新")
		}
	}

	tileContent := header + "\n" + valLine
	if resetLine != "" {
		tileContent += "\n" + resetLine
	}

	return map[string]any{
		"tag":            "column",
		"width":          "auto",
		"vertical_align": "top",
		"padding":        "6px 10px 6px 10px",
		"elements": []any{
			md(tileContent),
		},
	}
}

// formatAlertFactLine formats one window fact line in an alert row, visually emphasizing
// the triggering window (bold value, trigger tag) while keeping the secondary restrained.
func (r *Renderer) formatAlertFactLine(w domain.QuotaWindow, a domain.Alert, isTrigger bool) string {
	winName := shortWindowName(w)
	if w.Scope.Normalized() == domain.ScopeUnknown {
		winName += "（范围未知）"
	}

	var remText string
	if w.UsedPercent != nil {
		rem := remainingPct(*w.UsedPercent)
		color := alertValueColor(*w.UsedPercent)
		if isTrigger {
			color = alertPctColor(a, *w.UsedPercent)
			remText = fmt.Sprintf("**<font color='%s'>%s</font>**", color, rem)
		} else {
			remText = fmt.Sprintf("<font color='%s'>%s</font>", color, rem)
		}
	} else {
		remText = grey("未上报")
	}

	var resetText string
	if txt := r.resetText(w); txt != "未上报" {
		if isTrigger && a.Kind == domain.AlertQuotaExhausted {
			resetText = txt + " 恢复"
		} else {
			resetText = txt + " 刷新"
		}
	}

	var line string
	if w.UsedPercent != nil {
		line = grey(winName+" · 剩 ") + remText
	} else {
		line = grey(winName + " · 未上报")
	}
	if resetText != "" {
		line += grey(" · " + resetText)
	}
	if isTrigger {
		line += " " + inlineTag(alertTagColor(a), "触发")
	}
	return line
}

func alertTagColor(a domain.Alert) string {
	switch a.Kind {
	case domain.AlertQuotaExhausted:
		return "red"
	case domain.AlertQuotaThreshold:
		return "orange"
	case domain.AlertQuotaReset, domain.AlertRecovered:
		return "green"
	default:
		return "orange"
	}
}

// snapshotIndex maps credential key to its latest snapshot.
func snapshotIndex(rep *domain.Report) map[string]domain.QuotaSnapshot {
	out := map[string]domain.QuotaSnapshot{}
	if rep == nil {
		return out
	}
	for _, p := range rep.Providers {
		for _, s := range p.Snapshots {
			out[s.Credential.Key] = s
		}
	}
	return out
}
