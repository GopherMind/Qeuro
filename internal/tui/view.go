package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"qeuro/internal/clientcfg"
	"qeuro/internal/state"
	"qeuro/internal/styles"
	"qeuro/internal/tools"
)

func (m model) View() string {
	if m.quit {
		return styles.Muted.Render("  see you") + "\n"
	}

	var b strings.Builder

	// A selector overlay (e.g. /models) takes precedence over the palette.
	if m.sel.open {
		b.WriteString(m.sel.view(styles.ContentWidth(m.width, 36, 88)))
		b.WriteString("\n")
	} else if m.pal.open {
		// Slash-command palette floats just above the input when open.
		b.WriteString(m.pal.view(styles.ContentWidth(m.width, 36, 88)))
		b.WriteString("\n")
	}

	// Transient info panel (/help, /context, /usage…): redrawn live and never
	// flushed to scrollback — it disappears on the next key press instead of
	// piling up in the transcript.
	if m.infoView != "" {
		b.WriteString(m.infoView)
		b.WriteString("\n  " + styles.HintBar(
			[2]string{"esc", "close"},
			[2]string{"any key", "close and type"},
		) + "\n")
	}

	// Live streaming reply: the in-flight agent block is redrawn here as tokens
	// arrive, then flushed to scrollback once the stream completes.
	if m.streaming && (m.streamText != "" || m.streamMeta != "") {
		b.WriteString(styles.Message(styles.RoleAgent, clock(m.turnStarted), m.streamMeta, m.streamText, m.width))
	}

	// Pending file-edit approval: show the proposed change and the y/n prompt.
	if m.awaitingApproval && m.pendingTool != nil {
		b.WriteString(m.approvalPanel())
		b.WriteString("\n")
	}

	// Transient notification line — a quiet amber dot, Claude Code style.
	if m.notice != "" {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Amber).Render("⏺") + " " + styles.Muted.Render(m.notice) + "\n")
	}

	b.WriteString(m.inputBox())
	b.WriteString("\n")
	b.WriteString(m.statusBar())
	return b.String()
}

// approvalPanel renders the pending file edit or command with a three-option
// chooser (approve / approve-and-don't-ask / reject) in the Claude Code style:
// arrow-selectable, also pickable by number or the y/n shortcuts.
func (m model) approvalPanel() string {
	name := m.pendingTool.Function.Name
	title := tools.Summary(name, m.pendingTool.Function.Arguments)
	heading := "Approval Gate"
	yesLabel := "Apply change"
	// "Allow for this session" is the second option's meaning for file edits only.
	// For a command it downgrades to "approve once", and for an MCP tool there is
	// no session grant at all — offering a label that promises one would be the UI
	// telling the user something the policy will not honour.
	sessionLabel := "Allow for this session"
	switch {
	case name == tools.ToolRunCommand:
		heading = "Approval Gate · run command"
		yesLabel = "Run command"
	case tools.IsMCPName(name):
		heading = "Approval Gate · external MCP tool"
		yesLabel = "Call this tool once"
		sessionLabel = "Call once (external tools always ask)"
	}

	options := []string{yesLabel, sessionLabel, "Reject"}

	var body strings.Builder
	body.WriteString(styles.FieldRow("action", styles.Strong.Render(title), styles.ContentWidth(m.width, 40, 72)))
	body.WriteString("\n\n")
	if m.pendingPreview != "" {
		body.WriteString(styles.Chip("PREVIEW", styles.Amber))
		body.WriteString("\n")
		body.WriteString(styles.Muted.Render(m.pendingPreview))
		body.WriteString("\n\n")
	}
	for i, opt := range options {
		num := itoa(i + 1)
		if i == m.approvalChoice {
			row := " " + num + "  " + opt + " "
			body.WriteString(styles.Selected.Width(widestOption(options) + 6).Render(row))
		} else {
			body.WriteString(styles.Subtle.Render("   "+num+"  ") + styles.Base.Render(opt))
		}
		body.WriteString("\n")
	}
	body.WriteString("\n" + styles.HintBar(
		[2]string{"↑↓", "select"},
		[2]string{"enter", "confirm"},
		[2]string{"1/2/3", "choose"},
		[2]string{"y/n", "shortcut"},
	))

	w := styles.ContentWidth(m.width, 44, 80)
	return styles.Frame(heading, body.String(), w)
}

func (m model) inputBox() string {
	width := m.width
	if width < 20 {
		width = 20
	}

	// Narrow mode: drop the box-drawing rail so the prompt doesn't wrap on
	// tiny terminals (split-pane, mobile SSH). Content still fully usable.
	if styles.IsNarrow(width) {
		return m.inputBoxNarrow(width)
	}

	glyph, gcolor := m.activityGlyph()
	mark := "❯"
	if glyph != ">" {
		mark = glyph
	}
	prompt := lipgloss.NewStyle().Foreground(gcolor).Bold(true).Render(mark + " ")

	field := m.input.View()
	line := lipgloss.JoinHorizontal(lipgloss.Top, prompt, field)

	ruleW := width - 4
	if ruleW < 4 {
		ruleW = 4
	}
	topRule := inputRail(ruleW, gcolor)
	bottomRule := lipgloss.NewStyle().Foreground(styles.Faint).
		Render("╰" + strings.Repeat("─", ruleW-2) + "╯")

	// Generous breathing room above and below the input band.
	const pad = "\n"
	return pad + "  " + topRule + "\n  " + styles.Subtle.Render("│ ") + line + "\n  " + bottomRule + "\n"
}

func inputRail(width int, color lipgloss.Color) string {
	return lipgloss.NewStyle().Foreground(color).Bold(true).Render("╭") +
		lipgloss.NewStyle().Foreground(styles.Faint).Render(strings.Repeat("─", width-2)) +
		lipgloss.NewStyle().Foreground(color).Render("╮")
}

// statusModelName is what the status bar should call the model in force.
//
// In offline mode the cloud catalogue selection is not the model answering — the
// local server's own model is — so showing that label would name a model this
// session never uses, and the model picker is the one place a user checks before
// trusting a session with sensitive code. The configured name is shown when there
// is one; otherwise the server chooses and the bar says so rather than guessing
// (naming it would cost a request on every frame).
func (m model) statusModelName() string {
	if m.local {
		if m.localModel != "" {
			return "local " + clientcfg.DisplaySafe(m.localModel)
		}
		return "local (server default)"
	}
	if m.app.Mode == state.ModeAuto {
		return "auto"
	}
	return m.app.Model.Label
}

// activityGlyph reflects the current phase: spinner while generating.
func (m model) activityGlyph() (string, lipgloss.Color) {
	switch m.app.Phase {
	case state.PhaseGenerating:
		return m.spin.View(), styles.Accent2
	case state.PhaseError:
		return "✗", styles.Red
	default:
		return ">", styles.Accent2
	}
}

// statusBar shows the model (or "auto") and the project context usage on the
// left, with the remaining credits pinned to the far-right corner.
func (m model) statusBar() string {
	// Narrow terminals: show only model name and phase to avoid line wrap.
	if styles.IsNarrow(m.width) {
		essential := styles.Segment{Glyph: "◆", Text: "model " + m.statusModelName(), Color: styles.Accent2}
		return styles.StatusBarCompact(m.width, m.creditsText(), essential)
	}

	// Standard and wide: full segmented bar as normal.

	modelName := m.statusModelName()
	model := styles.Segment{Glyph: "◆", Text: "model " + modelName, Color: styles.Accent2}

	ctx := styles.Segment{
		Glyph: "◼",
		Text:  m.contextStatusText(),
		Color: ctxColor(m.app.CtxPercent()),
	}

	left := []styles.Segment{model, ctx}

	if usage := m.lastUsageStatus(); usage != "" {
		left = append(left, styles.Segment{Glyph: "▣", Text: usage, Color: styles.Sky})
	}

	// Surface non-default levers compactly so they stay discoverable.
	if m.app.Output != state.OutputFull {
		left = append(left, styles.Segment{Glyph: "◻", Text: "output " + m.app.Output.String(), Color: styles.Gray})
	}
	if m.app.Approvals != state.ApprovalAsk {
		left = append(left, styles.Segment{Glyph: "◇", Text: "approve " + m.app.Approvals.String(), Color: styles.Amber})
	}
	left = append(left, styles.Segment{Glyph: phaseGlyph(m.app.Phase), Text: phaseLabel(m.app.Phase), Color: phaseColor(m.app.Phase)})

	// Right corner: remaining credits.
	right := m.creditsText()

	return styles.StatusBarSplit(m.width, right, left...)
}

func (m model) contextStatusText() string {
	if m.local && m.app.Usage.Requests == 0 {
		// Local wire protocols do not report token usage. Showing ctx 0% would be a
		// measured-looking number that means only "the server told us nothing".
		return "ctx unknown"
	}
	text := "ctx " + itoa(m.app.CtxPercent()) + "%"
	last := m.app.Usage.Last
	if last.InputTokens > 0 {
		text += " · " + formatCompactInt(last.InputTokens) + " in"
	}
	return text
}

func (m model) lastUsageStatus() string {
	last := m.app.Usage.Last
	if last.TotalTokens() == 0 {
		return ""
	}
	text := "out " + formatCompactInt(last.OutputTokens)
	if last.CachedInputTokens > 0 {
		text += " · cache " + formatCompactInt(last.CachedInputTokens)
	}
	return text
}

// creditsText renders the remaining-credits chip for the status bar corner.
//
// When a session ceiling is in force it shows what is left under the ceiling
// instead of the account balance. A hard stop the user could not see coming
// reads as a broken client; the balance is the less urgent of the two numbers
// once a limit exists, because the limit is what will end the session first.
func (m model) creditsText() string {
	// Offline mode outranks both branches below: there is no balance and no
	// ceiling to report, and the one fact worth a permanent corner of the screen
	// is that this session is not talking to the backend (roadmap §8 "Offline").
	if m.local {
		return styles.Chip("local", styles.Sky)
	}
	if !m.loggedIn {
		return styles.Chip("offline", styles.Faint)
	}
	if m.budget.active() {
		left := m.budget.remaining()
		col := styles.Green
		switch {
		case left <= 0:
			col = styles.Red
		case left <= m.budget.limit/10:
			col = styles.Red
		case left <= m.budget.limit/4:
			col = styles.Amber
		}
		return styles.Chip("budget "+fmtCredits(left)+"/"+fmtCredits(m.budget.limit), col)
	}
	if !m.creditsKnown {
		return styles.Chip("credits ...", styles.Faint)
	}
	col := styles.Green
	switch {
	case m.credits <= 5:
		col = styles.Red
	case m.credits <= 20:
		col = styles.Amber
	}
	return styles.Chip("credits "+fmtCredits(m.credits), col)
}

// inputBoxNarrow is a rail-free input line for narrow terminals (< 60 cols).
// It keeps the prompt functional without decorative box-drawing characters.
func (m model) inputBoxNarrow(width int) string {
	glyph, gcolor := m.activityGlyph()
	mark := ">"
	if glyph != ">" {
		mark = glyph
	}
	prompt := lipgloss.NewStyle().Foreground(gcolor).Bold(true).Render(mark + " ")
	field := m.input.View()
	line := lipgloss.JoinHorizontal(lipgloss.Top, prompt, field)
	_ = width
	return "\n  " + line + "\n"
}

func widestOption(options []string) int {
	w := 0
	for _, opt := range options {
		if ow := lipgloss.Width(opt); ow > w {
			w = ow
		}
	}
	return w
}

// fmtCredits formats a credit balance without trailing ".0" noise.
func fmtCredits(c float64) string {
	if c == float64(int(c)) {
		return itoa(int(c))
	}
	return fmt.Sprintf("%.1f", c)
}

func ctxColor(p int) lipgloss.Color {
	switch {
	case p >= 85:
		return styles.Red
	case p >= 60:
		return styles.Amber
	default:
		return styles.Gray
	}
}

func phaseLabel(p state.Phase) string {
	switch p {
	case state.PhaseGenerating:
		return "thinking"
	case state.PhaseError:
		return "error"
	case state.PhaseDone:
		return "done"
	default:
		return "idle"
	}
}

func phaseGlyph(p state.Phase) string {
	switch p {
	case state.PhaseGenerating:
		return "●"
	case state.PhaseError:
		return "✕"
	case state.PhaseDone:
		return "✓"
	default:
		return "○"
	}
}

func phaseColor(p state.Phase) lipgloss.Color {
	switch p {
	case state.PhaseGenerating:
		return styles.Amber
	case state.PhaseError:
		return styles.Red
	case state.PhaseDone:
		return styles.Green
	default:
		return styles.Gray
	}
}
