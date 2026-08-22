package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"qeuro/internal/styles"
)

// selectorKind identifies which action to take when an item is chosen.
type selectorKind int

const (
	selBrand     selectorKind = iota // choose a provider brand → drills into models
	selModel                         // choose a model within a brand
	selEffort                        // choose a reasoning effort level
	selSettings                      // choose routing mode (auto/chat)
	selOutput                        // choose output verbosity (token economy)
	selApprovals                     // choose auto-approval level
)

// selectorItem is one navigable row in a selector overlay.
type selectorItem struct {
	label string
	note  string // dimmed description shown to the right
	value string // payload returned on choose
}

// selector is a modal list the user drives with ↑/↓ and confirms with Enter.
// While open it captures navigation keys ahead of the text input.
type selector struct {
	open     bool
	kind     selectorKind
	title    string
	items    []selectorItem
	selected int
	active   string // currently-applied value, marked with a bullet
	canBack  bool   // esc returns to the previous level instead of closing
}

func (s *selector) openWith(kind selectorKind, title string, items []selectorItem, active string, canBack bool) {
	s.open = true
	s.kind = kind
	s.title = title
	s.items = items
	s.active = active
	s.canBack = canBack
	s.selected = 0
	for i, it := range items {
		if it.value == active {
			s.selected = i
			break
		}
	}
}

func (s *selector) close() {
	s.open = false
	s.items = nil
	s.selected = 0
}

func (s *selector) up() {
	if len(s.items) == 0 {
		return
	}
	s.selected = (s.selected - 1 + len(s.items)) % len(s.items)
}

func (s *selector) down() {
	if len(s.items) == 0 {
		return
	}
	s.selected = (s.selected + 1) % len(s.items)
}

func (s *selector) current() (selectorItem, bool) {
	if !s.open || len(s.items) == 0 {
		return selectorItem{}, false
	}
	return s.items[s.selected], true
}

// effortDot returns a small coloured glyph conveying the intensity of an
// effort level, so the list reads at a glance (green = light → red = max).
func effortDot(value string) string {
	color := styles.Gray
	switch value {
	case "low":
		color = styles.Green
	case "medium":
		color = styles.Sky
	case "high":
		color = styles.Amber
	case "xhigh":
		color = styles.Red
	}
	return lipgloss.NewStyle().Foreground(color).Render("◗")
}

// view renders the overlay panel.
func (s *selector) view(width int) string {
	if !s.open {
		return ""
	}
	nameW := 0
	for _, it := range s.items {
		if w := lipgloss.Width(it.label); w > nameW {
			nameW = w
		}
	}

	// Inner row width inside the panel (border + padding eat 4 cells; leave a
	// little breathing room on the right).
	rowW := width - 6
	if rowW < 24 {
		rowW = 24
	}

	var b strings.Builder
	for i, it := range s.items {
		focused := i == s.selected
		active := it.value == s.active

		// Leading marker column: accent chevron when focused, green dot when
		// this is the applied value, otherwise blank.
		var marker string
		switch {
		case focused:
			marker = lipgloss.NewStyle().Foreground(styles.Cyan).Bold(true).Render("›")
		case active:
			marker = styles.OK.Render("●")
		default:
			marker = " "
		}

		// Optional per-level effort dot before the label.
		lead := ""
		if s.kind == selEffort {
			lead = effortDot(it.value) + " "
		}

		label := it.label
		if active && !focused {
			label += " " + styles.OK.Render("✓")
		}
		pad := nameW - lipgloss.Width(it.label) + 2
		if pad < 2 {
			pad = 2
		}

		if focused {
			// Full-width highlighted bar. Build plain text first, then apply a
			// single Width+Background style so the fill is uniform (nested
			// foreground colours would leave gaps in the background).
			leadPlain := ""
			if s.kind == selEffort {
				leadPlain = "◗ "
			}
			text := " " + leadPlain + label + strings.Repeat(" ", pad) + it.note
			row := styles.Selected.Width(rowW).Render(text)
			b.WriteString(marker + " " + row + "\n")
		} else {
			row := " " + lead + label + strings.Repeat(" ", pad) + styles.Muted.Render(it.note)
			b.WriteString(marker + " " + row + "\n")
		}
	}
	escHint := [2]string{"esc", "cancel"}
	if s.canBack {
		escHint = [2]string{"esc", "back"}
	}
	footer := styles.HintBar(
		[2]string{"↑↓", "select"},
		[2]string{"↵", "choose"},
		escHint,
	)
	body := strings.TrimRight(b.String(), "\n") + "\n" + footer
	return styles.Frame(s.title, body, width)
}
