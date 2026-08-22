package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// syncInputHeight grows the textarea to fit its content, counting soft-wrapped
// display rows (not just explicit newlines) using the same wrap width the
// textarea draws at, capped at maxInputRows.
func (m *model) syncInputHeight() {
	w := m.inputWrapWidth()
	rows := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		// Display width (not rune count) so wide glyphs wrap correctly; a full
		// line of width w occupies ceil(width/w) rows, and an empty line still
		// occupies one.
		lw := lipgloss.Width(ln)
		rows += lw/w + 1
	}
	if rows < 1 {
		rows = 1
	}
	if rows > maxInputRows {
		rows = maxInputRows
	}
	m.input.SetHeight(rows)
}

// bufferInput appends a printable string (a rune or a paste-embedded newline)
// to the pending-input buffer WITHOUT touching the textarea — O(1) per key, so a
// large paste cannot stutter or split — and (re)schedules a flush. During a
// paste, keys arrive faster than pasteFlushDelay and keep resetting the timer,
// so the whole block commits at once when the burst ends; normal keystrokes
// commit individually almost immediately.
func (m model) bufferInput(s string) (tea.Model, tea.Cmd) {
	m.pendingInput += s
	m.lastRuneAt = time.Now()
	m.pasteGen++
	return m, schedulePasteFlush(m.pasteGen)
}

// onPasteFlush commits the pending buffer once input has settled (no newer key).
func (m model) onPasteFlush(gen int) (tea.Model, tea.Cmd) {
	if gen != m.pasteGen {
		return m, nil // more input arrived; a later flush will handle it
	}
	m.commitPending()
	return m, nil
}

// commitPending flushes the buffered input into the textarea: a multi-line run
// becomes a compact "[paste N lines]" label (stored for expansion on submit),
// anything else is inserted verbatim at the cursor. No-op when nothing pending.
func (m *model) commitPending() {
	if m.pendingInput == "" {
		return
	}
	buf := m.pendingInput
	m.pendingInput = ""

	if lines := strings.Count(buf, "\n") + 1; lines >= 2 {
		m.pastes = append(m.pastes, buf)
		m.input.InsertString(fmt.Sprintf("[paste %d lines]", lines))
	} else {
		m.input.InsertString(buf)
	}
	m.syncInputHeight()
	m.pal.sync(m.input.Value())
}

// onPaste handles a paste delivered as a single event (bracketed paste, or a
// terminal that batches the whole payload into one key). Multi-line pastes
// collapse to a compact "[paste N lines]" label; single-line pastes insert
// inline.
func (m model) onPaste(content string) (tea.Model, tea.Cmd) {
	lines := strings.Count(content, "\n") + 1
	if lines < 2 {
		m.input.InsertString(content)
		m.syncInputHeight()
		m.pal.sync(m.input.Value())
		return m, nil
	}
	m.pastes = append(m.pastes, content)
	m.input.InsertString(fmt.Sprintf("[paste %d lines]", lines))
	m.syncInputHeight()
	m.pal.sync(m.input.Value())
	return m, nil
}
