package styles

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Layout breakpoints for adaptive TUI rendering.
const (
	NarrowWidth    = 60  // < NarrowWidth: compact, rail-free input
	StandardWidth  = 120 // >= StandardWidth: wide mode, content up to WideContentMax
	WideContentMax = 96  // maximum content width in wide mode
)

// ContentWidth returns the width for a panel or frame given the terminal width.
// min is the smallest acceptable width, max is the semantic maximum for the panel.
func ContentWidth(termWidth, minW, maxW int) int {
	if minW < 28 {
		minW = 28
	}
	// Leave a 4-column gutter on each side so the frame never kisses the edges.
	available := termWidth - 4
	if available < minW {
		return minW
	}
	if available > maxW {
		return maxW
	}
	return available
}

// IsNarrow reports whether the terminal is in narrow mode.
func IsNarrow(termWidth int) bool { return termWidth < NarrowWidth }

// IsWide reports whether the terminal is in wide mode.
func IsWide(termWidth int) bool { return termWidth >= StandardWidth }

// Role identifies who authored a message; it drives the block styling.
type Role int

const (
	RoleUser Role = iota
	RoleAgent
	RoleSystem
	RoleError
)

// glyph + accent per role.
func roleChrome(r Role) (glyph string, accent lipgloss.Color, label string) {
	switch r {
	case RoleUser:
		return "❯", Cyan, "you"
	case RoleAgent:
		return "●", Accent2, "qeuro"
	case RoleError:
		return "✖", Red, "error"
	default:
		return "◦", Gray, "system"
	}
}

// Message renders one chat block in the Claude Code spirit: the user's turn
// is a compact muted "❯ ..." echo without a header, the agent's turn is a
// "●" bullet header (label, time, model meta) with the body cleanly indented
// underneath — no rails or boxes around long text. ts is a preformatted clock
// string (e.g. "14:32"); empty hides it.
func Message(r Role, ts, meta, body string, width int) string {
	glyph, accent, label := roleChrome(r)
	if width < 24 {
		width = 24
	}
	mark := lipgloss.NewStyle().Foreground(accent).Bold(true).Render(glyph)

	bodyStyle := Base.Width(width - 6)
	switch r {
	case RoleError:
		bodyStyle = bodyStyle.Foreground(Red)
	case RoleUser:
		bodyStyle = bodyStyle.Foreground(Gray)
	}
	wrapped := bodyStyle.Render(body)
	lines := strings.Split(wrapped, "\n")

	// User turns echo compactly, without a header line.
	if r == RoleUser {
		var b strings.Builder
		for i, ln := range lines {
			if i == 0 {
				b.WriteString("  " + mark + " " + ln + "\n")
			} else {
				b.WriteString("    " + ln + "\n")
			}
		}
		return b.String()
	}

	header := mark + " " + lipgloss.NewStyle().Foreground(accent).Bold(true).Render(label)
	if ts != "" {
		header += "  " + Subtle.Render(ts)
	}
	if meta != "" {
		header += Subtle.Render("  ·  ") + Muted.Render(meta)
	}

	var b strings.Builder
	b.WriteString("  " + header + "\n")
	for _, ln := range lines {
		b.WriteString("    " + ln + "\n")
	}
	return b.String()
}

// Separator returns a faint horizontal rule of the given width.
func Separator(width int) string {
	if width < 8 {
		width = 8
	}
	left := lipgloss.NewStyle().Foreground(Accent2).Render("╺")
	right := lipgloss.NewStyle().Foreground(Sky).Render("╸")
	if width <= 2 {
		return left + right
	}
	return left + Subtle.Render(strings.Repeat("─", width-2)) + right
}

// Panel renders a titled block in the Qeuro style: a bold title row marked
// with an accent gutter glyph, then the content indented under a thin vertical
// rule. It stays terminal-native while feeling closer to a cloud console.
func Panel(title, content string, width int) string {
	if width < 8 {
		width = 8
	}
	rule := lipgloss.NewStyle().Foreground(Faint).Render("│ ")

	var b strings.Builder
	if title != "" {
		mark := lipgloss.NewStyle().Foreground(Accent2).Bold(true).Render("▎ ")
		b.WriteString(mark + Accent.Render(title) + Subtle.Render("  "+strings.Repeat("─", max(4, width-lipgloss.Width(title)-6))) + "\n")
	}
	for _, ln := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		b.WriteString(rule + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Frame renders a compact bordered panel for overlays and static screens.
func Frame(title, content string, width int) string {
	if width < 28 {
		width = 28
	}
	innerW := width - 4
	titleText := ""
	if strings.TrimSpace(title) != "" {
		titleText = " " + title + " "
	}

	var b strings.Builder
	if titleText != "" {
		styledTitle := Accent.Render(titleText)
		fill := width - 3 - lipgloss.Width(styledTitle)
		if fill < 1 {
			fill = 1
		}
		b.WriteString(Accent.Render("╭─") + styledTitle + Subtle.Render(strings.Repeat("─", fill)) + Accent.Render("╮") + "\n")
	} else {
		b.WriteString(Accent.Render("╭") + Subtle.Render(strings.Repeat("─", width-2)) + Accent.Render("╮") + "\n")
	}

	for _, ln := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		if lipgloss.Width(ln) > innerW {
			ln = lipgloss.NewStyle().MaxWidth(innerW).Render(ln)
		}
		b.WriteString(Accent.Render("│ ") + padCell(ln, innerW) + Accent.Render(" │") + "\n")
	}
	b.WriteString(Accent.Render("╰") + Subtle.Render(strings.Repeat("─", width-2)) + Accent.Render("╯"))
	return b.String()
}

// Chip renders a small terminal-native label used in headers and status rows.
func Chip(text string, color lipgloss.Color) string {
	return lipgloss.NewStyle().
		Foreground(color).
		Background(Surface2).
		Padding(0, 1).
		Render(text)
}

// Pill renders a denser highlighted label for important state.
func Pill(text string, color lipgloss.Color) string {
	return lipgloss.NewStyle().
		Foreground(Surface).
		Background(color).
		Bold(true).
		Padding(0, 1).
		Render(text)
}

// Metric renders a compact two-line telemetry tile for the welcome surface.
func Metric(label, value string) string {
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, false, true).
		BorderForeground(Faint).
		Padding(0, 1).
		Render(Subtle.Render(label) + "\n" + Base.Render(value))
}

// Hint renders a single "key — description" hotkey hint.
func Hint(key, desc string) string {
	return Chip(key, Faint) + Muted.Render(" "+desc)
}

// HintBar joins several hints with separators onto one muted line.
func HintBar(pairs ...[2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, Hint(p[0], p[1]))
	}
	return strings.Join(parts, Subtle.Render("   ·   "))
}

// Indent prefixes every rendered line with the same padding.
func Indent(s, prefix string) string {
	if s == "" {
		return prefix
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// FieldRow renders a label/value line with a fixed label column.
func FieldRow(label, value string, width int) string {
	if width < 16 {
		width = 16
	}
	labelW := 13
	if labelW > width/2 {
		labelW = width / 2
	}
	return Subtle.Render(padText(label, labelW)) + Base.Render(value)
}

// ProgressBar renders a stable-width horizontal usage bar.
func ProgressBar(percent, width int, color lipgloss.Color) string {
	if width < 4 {
		width = 4
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := percent * width / 100
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		Subtle.Render(strings.Repeat("░", width-filled))
}

func padText(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
