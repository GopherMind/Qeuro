package styles

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Segment is one cell of the status bar.
type Segment struct {
	Glyph string         // optional leading glyph (e.g. ●)
	Text  string         // segment text
	Color lipgloss.Color // glyph/text accent
}

// renderSegment renders one status segment (optional glyph + coloured text).
func renderSegment(s Segment) string {
	var sb strings.Builder
	if s.Glyph != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(s.Color).Render(s.Glyph) + " ")
	}
	sb.WriteString(lipgloss.NewStyle().Foreground(Fg).Render(s.Text))
	return sb.String()
}

// StatusBar renders segments separated by subtle dividers, led by the Qeuro
// brand mark. Flat, borderless, terminal-native.
func StatusBar(width int, segs ...Segment) string {
	parts := make([]string, 0, len(segs)+1)
	parts = append(parts, Accent.Render("✳ qeuro"))
	for _, s := range segs {
		parts = append(parts, renderSegment(s))
	}
	sep := lipgloss.NewStyle().Foreground(Faint).Render("  │  ")
	bar := strings.Join(parts, sep)
	if width > 0 && lipgloss.Width(bar) > width {
		bar = lipgloss.NewStyle().MaxWidth(width).Render(bar)
	}
	return "  " + bar
}

// StatusBarCompact renders a single-segment minimal bar for narrow terminals.
// It shows only the most essential segment (first one) and the right chip.
func StatusBarCompact(width int, right string, essential Segment) string {
	left := "  " + Accent.Render("✳") + " " + renderSegment(essential)
	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
	if width <= 0 {
		return left + "  " + right
	}
	gap := width - lw - rw - 1
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// StatusBarSplit renders left-aligned segments with a single right-aligned chip
// pinned to the far-right corner, padding the gap so the chip hugs the edge.
// Used for the credits balance in the bottom-right corner.
func StatusBarSplit(width int, right string, segs ...Segment) string {
	sep := lipgloss.NewStyle().Foreground(Faint).Render("  │  ")

	parts := make([]string, 0, len(segs)+1)
	parts = append(parts, Accent.Render("✳ qeuro"))
	for _, s := range segs {
		parts = append(parts, renderSegment(s))
	}
	left := "  " + strings.Join(parts, sep)

	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
	if width <= 0 {
		return left + "    " + right
	}
	// One trailing space keeps the chip off the very last column.
	gap := width - lw - rw - 1
	if gap < 2 {
		if lw+rw+1 > width {
			return lipgloss.NewStyle().MaxWidth(width).Render(left + "  " + right)
		}
		gap = 2
	}
	return left + strings.Repeat(" ", gap) + right
}
