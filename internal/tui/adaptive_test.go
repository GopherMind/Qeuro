package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"

	"qeuro/internal/state"
	"qeuro/internal/styles"
)

// TestAdaptiveBreakpointsAreConsistent ensures the exported constants are
// coherent and the helpers agree with them.
func TestAdaptiveBreakpointsAreConsistent(t *testing.T) {
	if styles.NarrowWidth >= styles.StandardWidth {
		t.Fatalf("NarrowWidth (%d) must be < StandardWidth (%d)",
			styles.NarrowWidth, styles.StandardWidth)
	}
	if !styles.IsNarrow(styles.NarrowWidth - 1) {
		t.Fatalf("IsNarrow should be true below NarrowWidth")
	}
	if styles.IsNarrow(styles.NarrowWidth) {
		t.Fatalf("IsNarrow should be false at NarrowWidth")
	}
	if !styles.IsWide(styles.StandardWidth) {
		t.Fatalf("IsWide should be true at StandardWidth")
	}
}

func TestContentWidthClamping(t *testing.T) {
	if styles.ContentWidth(40, 44, 80) != 44 {
		t.Fatalf("ContentWidth should return minW when terminal is too narrow")
	}
	if styles.ContentWidth(200, 44, 80) != 80 {
		t.Fatalf("ContentWidth should return maxW when terminal is very wide")
	}
	// mid case: 80 terminal, gutter 4 => available 76, within [44,80] => 76
	if got := styles.ContentWidth(80, 44, 80); got != 76 {
		t.Fatalf("ContentWidth(80,44,80) should be 76 (80-4), got %d", got)
	}
}

func TestHelpScreenAdaptsToTerminalWidth(t *testing.T) {
	narrow := helpScreen(50)
	standard := helpScreen(80)
	wide := helpScreen(140)

	// All must render the command list.
	for name, s := range map[string]string{"narrow": narrow, "standard": standard, "wide": wide} {
		if !strings.Contains(s, "/help") {
			t.Fatalf("%s helpScreen missing /help command", name)
		}
	}

	// Wider terminals should produce wider output.
	if lipglossWidth(standard) <= lipglossWidth(narrow) {
		t.Fatalf("standard helpScreen should be wider than narrow")
	}
	if lipglossWidth(wide) <= lipglossWidth(standard) {
		t.Fatalf("wide helpScreen should be wider than standard")
	}
}

func TestStatusBarNarrowMode(t *testing.T) {
	m := model{
		app:      state.New(),
		width:    45,
		loggedIn: true,
	}
	bar := m.statusBar()
	if strings.Contains(bar, "· ") {
		// wide bar has multiple "|" dividers; narrow should be leaner
		lines := strings.Split(bar, "\n")
		for _, l := range lines {
			if len(l) > m.width+8 { // +8 for ANSI escape overhead estimate
				t.Fatalf("narrow status bar line longer than terminal: %d > %d", len(l), m.width+8)
			}
		}
	}
}

func TestInputBoxNarrowSkipsRail(t *testing.T) {
	m := newInputTestModel(42)
	narrow := m.inputBoxNarrow(42)
	if strings.Contains(narrow, "╭") || strings.Contains(narrow, "╮") {
		t.Fatalf("narrow inputBox must not include box-drawing rail characters")
	}
	if narrow == "" {
		t.Fatalf("narrow inputBox must not be empty")
	}
}

func TestInputBoxWideHasRail(t *testing.T) {
	m := newInputTestModel(100)
	box := m.inputBox()
	if !strings.Contains(box, "╭") {
		t.Fatalf("standard inputBox should include the top-left rail corner")
	}
}

// lipglossWidth returns the visible width of the first non-empty line.
func lipglossWidth(s string) int {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return len([]rune(l)) // rough: rune count as proxy for visible width
		}
	}
	return 0
}

// newInputTestModel builds a minimal model with an input component for tests.
func newInputTestModel(width int) model {
	in := textarea.New()
	in.SetWidth(width - 8)
	return model{
		app:   state.New(),
		width: width,
		input: in,
	}
}
