package tui

import (
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// RunResume starts the TUI with a previous session already restored, which is
// what `qeuro resume [id]` does. An empty id means the newest session.
//
// It goes through the same model and the same resumeSession path as the slash
// command rather than reimplementing the restore: two restore paths would be two
// definitions of what "the same place" means, and only one of them would keep
// being tested.
func RunResume(version, id string) error {
	m, ok := resumeModel(version, id)
	if !ok {
		return Run(version)
	}
	return run(m)
}

// RunWithFlags is Run with a flag layer on top of the config resolution, which
// is how `qeuro chat --budget 20` reaches the session ceiling. Flags are keyed by
// setting name (see clientcfg.settingSpecs) rather than assigned over the loaded
// config, so `config doctor` reports the layer that actually won.
func RunWithFlags(version string, flags map[string]string) error {
	return run(newModelWithFlags(version, flags))
}

// resumeModel builds the model `qeuro resume` starts from. It is separate from
// RunResume because everything interesting happens here, while RunResume itself
// is a call into tea.Program.Run, which needs a terminal and cannot be tested.
func resumeModel(version, id string) (model, bool) {
	m := newModel(version)
	restored, cmd := m.resumeSession(id)
	rm, ok := restored.(model)
	if !ok {
		return m, false
	}
	// resumeSession returns the command that prints the restored transcript. Run
	// it from Init rather than dropping it: a resume whose history is invisible
	// looks like a resume that did nothing, and the notice line alone does not
	// show the user what came back.
	rm.initCmd = cmd
	return rm, true
}

// Run starts the inline TUI and blocks until the user exits.
//
// Mouse tracking is intentionally NOT enabled: with it on, the terminal forwards
// clicks to the program and disables its own text selection, so the user cannot
// select/copy output with the mouse. Leaving it off keeps normal click-drag
// selection and copy working in every terminal.
func Run(version string) error { return run(newModel(version)) }

// run drives one prepared model to completion and performs the shutdown both
// entry points owe: MCP child processes and the journal's end marker.
func run(m model) error {
	p := tea.NewProgram(m)
	final, err := p.Run()
	// MCP servers are child processes. Shutting them down after the loop exits —
	// rather than in a defer inside Init — is what keeps them from outliving the
	// CLI holding whatever token was passed to them. The final model carries the
	// manager; a model value without one (any error path) makes this a no-op.
	if fm, ok := final.(model); ok {
		fm.closeMCP()
		// The end marker distinguishes a clean exit from a crash on the next
		// resume, so it is written here — after the loop, where the final model is
		// available — rather than from a key handler that a panic would skip.
		_ = fm.journal.Close(time.Now())
	}
	return err
}

func itoa(n int) string { return strconv.Itoa(n) }

// clock formats a time as a short HH:MM stamp for message headers.
func clock(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.Format("15:04")
}
