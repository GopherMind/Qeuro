package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"qeuro/internal/session"
)

// isolateSessions points the journal directory at a temp dir. The subcommand
// reads the real ~/.qeuro/sessions otherwise, and a test that lists the
// developer's own transcripts is both flaky and a privacy problem.
func isolateSessions(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if session.Dir() == "" {
		t.Skip("no config dir available on this platform")
	}
}

func seedSession(t *testing.T, id string, clean bool, recs ...session.Record) {
	t.Helper()
	j, err := session.New(id, time.Now(), session.Record{Version: "test", Dir: "/work/repo"})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	for _, r := range recs {
		if err := j.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if clean {
		if err := j.Close(time.Now()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return
	}
	t.Cleanup(func() { _ = j.Close(time.Now()) })
}

func TestResumeCommandIsRegistered(t *testing.T) {
	var found *command
	for _, cmd := range commands() {
		if cmd.matches("resume") {
			c := cmd
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal(`no "resume" command: roadmap §8 requires "qeuro resume [id]"`)
	}
	if found.run == nil || found.usage == "" || found.summary == "" {
		t.Fatal(`"resume" is registered but not dispatchable or not in help`)
	}
}

// TestResumePlanExitCodes pins the argument handling a script sees. Each of
// these is a decision to not start the TUI, and the only thing distinguishing
// them from a successful resume is the exit code.
func TestResumePlanExitCodes(t *testing.T) {
	isolateSessions(t)
	seedSession(t, "20260813-070000-aaaaaa", true,
		session.Record{Kind: session.KindUser, Text: "q"})

	cases := []struct {
		name string
		args []string
		id   string
		code int
		why  string
	}{
		{"no args resumes the newest", nil, "", -1,
			"a bare `qeuro resume` must open the TUI, not print anything"},
		{"known id", []string{"20260813-070000-aaaaaa"}, "20260813-070000-aaaaaa", -1,
			"an id that loads must reach the TUI"},
		{"id is trimmed", []string{"  20260813-070000-aaaaaa\n"}, "20260813-070000-aaaaaa", -1,
			"a copy-pasted id with surrounding whitespace should still resume"},
		{"list", []string{"list"}, "", 0, "listing is a success, not a resume"},
		{"ls alias", []string{"LS"}, "", 0, "the alias is case-insensitive"},
		{"empty id", []string{""}, "", 2,
			"`qeuro resume $ID` with ID unset must not silently resume the newest session"},
		{"empty id after trim", []string{"   "}, "", 2,
			"whitespace is the same shell accident as an empty string"},
		{"too many args", []string{"a", "b"}, "", 2, "usage error"},
		{"unknown id", []string{"20260813-999999-zzzzzz"}, "", 1,
			"a missing session is a failure a script can act on"},
		{"unsafe id", []string{"../../etc/passwd"}, "", 1,
			"a path-shaped id must be refused, not opened"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			id, code := resumePlan(c.args, &out, &errOut)
			if code != c.code {
				t.Errorf("code = %d, want %d: %s\nstdout: %s\nstderr: %s",
					code, c.code, c.why, out.String(), errOut.String())
			}
			if id != c.id {
				t.Errorf("id = %q, want %q", id, c.id)
			}
			if code < 0 && (out.Len() > 0 || errOut.Len() > 0) {
				t.Errorf("a resume that starts the TUI must print nothing first:\nstdout: %s\nstderr: %s",
					out.String(), errOut.String())
			}
			if code > 0 && errOut.Len() == 0 {
				t.Error("a non-zero exit must say why on stderr")
			}
		})
	}
}

func TestResumeListShowsSessionsNewestFirst(t *testing.T) {
	isolateSessions(t)
	seedSession(t, "20260813-080000-aaaaaa", true,
		session.Record{Kind: session.KindUser, Text: "older question"})
	seedSession(t, "20260813-090000-bbbbbb", false,
		session.Record{Kind: session.KindUser, Text: "newer question"},
		session.Record{Kind: session.KindAssistant, Text: "answer"})

	var out bytes.Buffer
	if code := resumeList(&out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := out.String()
	older := strings.Index(got, "20260813-080000-aaaaaa")
	newer := strings.Index(got, "20260813-090000-bbbbbb")
	if older < 0 || newer < 0 {
		t.Fatalf("both sessions must be listed:\n%s", got)
	}
	if newer > older {
		t.Fatal("sessions must be listed newest first: the newest is the one being continued")
	}
	if !strings.Contains(got, "CRASHED") {
		t.Fatalf("a session with no end marker must be marked:\n%s", got)
	}
	if !strings.Contains(got, "2 turns") {
		t.Fatalf("turn counts are what tell two sessions apart:\n%s", got)
	}
	if !strings.Contains(got, "/work/repo") {
		t.Fatalf("the working directory should be shown:\n%s", got)
	}
}

func TestResumeListOnAnEmptyDirectoryIsNotAFailure(t *testing.T) {
	isolateSessions(t)
	var out bytes.Buffer
	if code := resumeList(&out); code != 0 {
		t.Fatalf("exit code = %d, want 0: no sessions yet is a normal state", code)
	}
	if !strings.Contains(out.String(), "no sessions recorded yet") {
		t.Fatalf("output should say the directory is empty:\n%s", out.String())
	}
}

func TestResumeListSanitisesTheJournalDirectoryItEchoes(t *testing.T) {
	isolateSessions(t)
	// The meta record's Dir comes from a file, so it is untrusted for display
	// purposes: an escape sequence there could repaint the listing the user is
	// choosing a session from.
	id := "20260813-081000-aaaaaa"
	j, err := session.New(id, time.Now(), session.Record{
		Version: "test",
		Dir:     "/work\x1b[2Kfake",
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := j.Append(session.Record{Kind: session.KindUser, Text: "q"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := j.Close(time.Now()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var out bytes.Buffer
	resumeList(&out)
	if strings.Contains(out.String(), "\x1b[2K") {
		t.Fatalf("an escape sequence from a journal reached the terminal:\n%q", out.String())
	}
	if !strings.Contains(out.String(), `\x1b`) {
		t.Fatalf("the escape should be shown in visible form:\n%s", out.String())
	}
}
