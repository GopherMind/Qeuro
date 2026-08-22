package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/styles"
	"qeuro/internal/tui"
)

// cmdResume implements `qeuro resume [id]` and `qeuro resume list`
// (roadmap §8, row "Сессии").
//
// `list` exists because the row asks for `resume [id]` and an id is a timestamp
// nobody memorises. Listing is a separate subcommand rather than the default so
// that the common case — "continue what I was doing" — stays a bare `qeuro
// resume` with no argument to choose.
func cmdResume(args []string) {
	id, code := resumePlan(args, os.Stdout, os.Stderr)
	if code >= 0 {
		os.Exit(code)
	}

	enableVirtualTerminal()
	if err := tui.RunResume(version, id); err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("TUI error: ")+err.Error())
		os.Exit(1)
	}
}

// resumePlan decides what `qeuro resume` should do, without doing it: it returns
// the session id to open and an exit code, where a negative code means "no exit,
// start the TUI". Every rejection here is a script-visible exit code, so it is
// kept out of cmdResume — which cannot be tested, because os.Exit ends the test
// binary along with the process.
func resumePlan(args []string, out, errOut io.Writer) (string, int) {
	if len(args) > 0 {
		switch first := strings.ToLower(strings.TrimSpace(args[0])); first {
		case "list", "ls":
			return "", resumeList(out)
		case "":
			// An empty argument is a shell accident ("qeuro resume $ID" with ID
			// unset). Resuming the newest session on it would be a surprise.
			return "", resumeUsageErr(errOut, "empty session id")
		}
	}
	if len(args) > 1 {
		return "", resumeUsageErr(errOut, "too many arguments")
	}

	id := ""
	if len(args) == 1 {
		id = strings.TrimSpace(args[0])
		// Fail before starting the TUI: an unknown id inside the TUI is one line
		// in a status bar that scrolls away, while a script needs an exit code.
		if _, err := session.Load(id); err != nil {
			fmt.Fprintln(errOut, "  "+styles.Err.Render("cannot resume: ")+
				styles.Muted.Render(clientcfg.DisplaySafe(err.Error())))
			fmt.Fprintln(errOut, "  "+styles.Muted.Render("see: ")+styles.UserTag.Render("qeuro resume list"))
			return "", 1
		}
	}
	return id, -1
}

func resumeUsageErr(errOut io.Writer, msg string) int {
	fmt.Fprintln(errOut, "  "+styles.Err.Render(msg))
	fmt.Fprintln(errOut, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render(resumeUsage))
	return 2
}

const resumeUsage = "qeuro resume [id] | qeuro resume list"

// resumeList prints the recorded sessions, newest first.
func resumeList(out io.Writer) int {
	dir := session.Dir()
	if dir == "" {
		fmt.Fprintln(out, "  "+styles.Warn.Render("sessions are not recorded: ")+
			styles.Muted.Render("the OS reports no user config directory"))
		return 1
	}

	sessions := session.List(resumeListLimit)
	if len(sessions) == 0 {
		fmt.Fprintln(out, "  "+styles.Muted.Render("no sessions recorded yet in ")+styles.Base.Render(dir))
		return 0
	}

	now := time.Now()
	var b strings.Builder
	for _, s := range sessions {
		turns := len(s.Turns())
		row := styles.UserTag.Render(s.ID) + styles.Subtle.Render("  "+fmt.Sprintf("%d turns", turns))
		if age := session.Age(s, now); age != "" {
			row += styles.Subtle.Render(" · " + age)
		}
		if s.Crashed {
			row += " " + styles.Chip("CRASHED", styles.Amber)
		}
		if s.Skipped > 0 {
			row += " " + styles.Chip("DAMAGED", styles.Amber)
		}
		b.WriteString(row + "\n")
		// The working directory is what tells two sessions of the same day apart,
		// and it comes from the journal, so it is display-sanitized.
		if s.Meta.Dir != "" {
			b.WriteString("  " + styles.Subtle.Render(clientcfg.DisplaySafe(s.Meta.Dir)) + "\n")
		}
	}
	fmt.Fprintln(out, styles.Indent(styles.Frame("Sessions", strings.TrimRight(b.String(), "\n"), 74), "  "))
	fmt.Fprintln(out, "  "+styles.Subtle.Render("resume  ")+styles.UserTag.Render("qeuro resume <id>"))
	return 0
}

// resumeListLimit bounds the listing: every entry is parsed to count its turns,
// so an unbounded list would read every journal the directory keeps.
const resumeListLimit = 20
