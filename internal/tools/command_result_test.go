package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// commandResult is the format two other packages read back: agentcore lifts the
// literal output out of it for the agent protocol, and the console renders that in
// the task page's evidence panel. These tests pin the two properties those readers
// depend on — the separator is always there, and the output is always valid UTF-8.

func TestCommandResultAlwaysCarriesTheSeparator(t *testing.T) {
	// Every outcome that started a process, timeouts included. The timeout branch is
	// what this exists for: it used to return a status line with no separator, and
	// agentcore.commandOutputOf keys on the separator, so the output a hanging
	// command had already printed was silently dropped from the evidence panel.
	cases := []struct {
		name   string
		status string
		output string
	}{
		{"ok", CommandOKPrefix, "ok  \tqeuro/webapi\t1.204s\n"},
		{"failed", "failed: exit status 1", "--- FAIL: TestX\n"},
		{"timed out", "command timed out", "step 1 of 4 done\n"},
		{"timed out, nothing printed", "command timed out", ""},
		{"ok, nothing printed", CommandOKPrefix, ""},
		{"whitespace only", CommandOKPrefix, "  \n\t "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commandResult(tc.status, tc.output)

			status, rest, found := strings.Cut(got, "\n")
			if !found {
				t.Fatalf("нет строки статуса: %q", got)
			}
			if status != tc.status {
				t.Fatalf("статус = %q, want %q", status, tc.status)
			}
			sep, body, found := strings.Cut(rest, "\n")
			if !found || sep != CommandOutputSeparator {
				t.Fatalf("вторая строка = %q, want %q", sep, CommandOutputSeparator)
			}
			if strings.TrimSpace(tc.output) == "" {
				// A caller cannot tell "printed nothing" from "the field is missing"
				// unless the empty case says so.
				if body != "(no output)" {
					t.Fatalf("пустой вывод = %q, want \"(no output)\"", body)
				}
				return
			}
			if body != tc.output {
				t.Fatalf("вывод = %q, want %q", body, tc.output)
			}
		})
	}
}

func TestFinishedCommandResultCarriesTheSeparatorOnEveryOutcome(t *testing.T) {
	// The call-site form of the property above. Asserting only on commandResult
	// would leave the branch that chooses the status free to bypass it, which is how
	// the timeout case lost its separator in the first place.
	cases := []struct {
		name       string
		output     string
		runErr     error
		ctxErr     error
		wantStatus string
	}{
		{"success", "ok  \tpkg\t1.2s\n", nil, nil, CommandOKPrefix},
		{"failure", "--- FAIL\n", errors.New("exit status 1"), nil, "failed: exit status 1"},
		{"timeout", "step 1 of 4 done\n", errors.New("signal: killed"), context.DeadlineExceeded, "command timed out"},
		{"timeout, nothing printed", "", errors.New("signal: killed"), context.DeadlineExceeded, "command timed out"},
		{"wrapped deadline", "partial\n", nil, fmt.Errorf("run: %w", context.DeadlineExceeded), "command timed out"},
		{"canceled, not a deadline", "", errors.New("signal: killed"), context.Canceled, "failed: signal: killed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := finishedCommandResult(tc.output, tc.runErr, tc.ctxErr)

			status, rest, found := strings.Cut(got, "\n")
			if !found || status != tc.wantStatus {
				t.Fatalf("статус = %q, want %q (полный результат %q)", status, tc.wantStatus, got)
			}
			sep, body, found := strings.Cut(rest, "\n")
			if !found || sep != CommandOutputSeparator {
				t.Fatalf("нет разделителя после статуса: %q", got)
			}
			want := tc.output
			if strings.TrimSpace(want) == "" {
				want = "(no output)"
			}
			if body != want {
				t.Fatalf("вывод = %q, want %q", body, want)
			}
		})
	}
}

func TestFinishedCommandResultTruncatesOnAValidBoundary(t *testing.T) {
	// The byte cap can land inside a multi-byte rune, and this text goes on to a JSON
	// protocol and a Postgres text column, both of which reject invalid UTF-8.
	long := strings.Repeat("щ", maxCmdOutput)
	got := finishedCommandResult(long, nil, nil)

	if !utf8.ValidString(got) {
		t.Fatal("усечённый вывод невалиден в UTF-8")
	}
	if !strings.Contains(got, "[output truncated]") {
		t.Fatal("нет отметки об усечении: вывод выдаётся за полный")
	}
}

func TestCommandResultForcesValidUTF8(t *testing.T) {
	// A process writes bytes. A build in a cp1251 console, or output cut at
	// maxCmdOutput mid-rune, is not valid UTF-8 — and encoding/json refuses to
	// marshal such a string while Postgres refuses it in a text column, so an
	// invalid byte here would lose the whole evidence row instead of one glyph.
	cases := map[string]string{
		"lone continuation byte": "FAIL\xffpkg\n",
		"truncated rune":         "тест" + string([]byte{0xd1}),
		"cp1251 text":            string([]byte{0xf2, 0xe5, 0xf1, 0xf2}),
		"bare 0x9b":              "before\x9bafter",
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			got := commandResult(CommandOKPrefix, output)
			if !utf8.ValidString(got) {
				t.Fatalf("commandResult(%q) невалиден в UTF-8: %q", output, got)
			}
			// The valid part survives: replacing the string wholesale would discard
			// the output the panel exists to show.
			if strings.Contains(output, "FAIL") && !strings.Contains(got, "FAIL") {
				t.Fatalf("потерян валидный текст: %q", got)
			}
		})
	}
}

func TestCommandResultRoundTripsThroughTheEvidenceReader(t *testing.T) {
	// The contract with agentcore, asserted from this side too: whatever
	// commandResult composes, the separator is findable as a whole line, so the
	// reader can split status from output without knowing anything else about the
	// format.
	const output = "line one\nline two\n"
	got := commandResult("failed: exit status 2", output)

	idx := strings.Index(got, "\n"+CommandOutputSeparator+"\n")
	if idx < 0 {
		t.Fatalf("разделитель не отдельной строкой: %q", got)
	}
	if body := got[idx+len(CommandOutputSeparator)+2:]; body != output {
		t.Fatalf("вывод после разделителя = %q, want %q", body, output)
	}
}
