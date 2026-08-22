package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/clientcfg"
	"qeuro/internal/styles"
)

// Transient overlays (Approval Gate, Selector, Command Palette) are drawn
// inside Bubble Tea's inline-rendered frame. When one of them closes the
// frame shrinks, and on some terminals (notably Windows CMD/PowerShell, or
// after the user scrolled) the stale frame lines survive in the scrollback
// history. Claude Code solves this by wiping the interactive block and
// leaving a single one-line log entry — this file implements the same idea.
//
// ansiEraseBelow erases from the cursor to the end of the screen (CSI 0J).
// It is emitted through tea.Println: the renderer writes queued println
// lines starting at the top of the previously drawn frame, so the sequence
// wipes the stale overlay lines — whatever their height — right before the
// fresh, shorter frame is repainted below. Unlike counting panel lines and
// issuing cursor-up (CSI A) + erase-line (CSI 2K) pairs, erasing downwards
// can never eat the assistant transcript above the frame, and it cannot
// over- or under-erase when a panel's height changes. Bubble Tea enables
// virtual terminal processing on Windows consoles, so the same escape
// sequence works in CMD and PowerShell.
const ansiEraseBelow = "\x1b[0J"

// eraseOverlayCmd wipes the just-closed overlay's frame from the visible
// terminal and prints one neat status line in its place. An empty status
// leaves only a blank separator line in the transcript.
func eraseOverlayCmd(status string) tea.Cmd {
	return tea.Println(ansiEraseBelow + status)
}

// approvalStatus is the one-line transcript record that replaces the whole
// approval panel once the user decides.
//
// The summary is built from the tool call's arguments, which the model chose, so
// it is untrusted text on its way into a terminal (.ai/SECURITY.md: model output
// is data). A path or command carrying escape sequences would otherwise clear the
// screen or move the cursor from inside the line that says what was approved —
// the one line the user relies on to know what just happened. One line, so
// newlines are escaped too: a summary that spans rows could push the real status
// out of view and print a fake one.
func approvalStatus(approved bool, summary string) string {
	summary = clientcfg.DisplaySafe(summary)
	if approved {
		return "  " + styles.OK.Render("✓ Выполнено: "+summary)
	}
	return "  " + styles.Err.Render("✗ Отклонено: "+summary)
}

// seqCmds sequences the non-nil commands in order, so the erase/status
// output always lands in the terminal before any follow-up command output.
func seqCmds(cmds ...tea.Cmd) tea.Cmd {
	out := make([]tea.Cmd, 0, len(cmds))
	for _, c := range cmds {
		if c != nil {
			out = append(out, c)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	default:
		return tea.Sequence(out...)
	}
}
