package clientcfg

import (
	"strings"
	"testing"
)

func TestParseFlatTOMLAcceptsTheSupportedSubset(t *testing.T) {
	const doc = `
# leading comment
base_url = "http://api.local"
model = 'literal\path\unescaped'
auto_approve = true
port_like = 8080
"quoted_key" = "v"
trailing = "value"  # comment after a value
bare_trailing = true   # comment after a bare value
`
	got, err := parseFlatTOML(strings.NewReader(doc), "test.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{
		"base_url":     "http://api.local",
		"model":        `literal\path\unescaped`,
		"auto_approve": "true",
		"port_like":    "8080",
		"quoted_key":   "v",
		"trailing":     "value",
		// A bare value drops its trailing comment too. Keeping it would turn
		// `auto_approve = true  # yes` into the value "true  # yes", which then
		// fails the boolean check and reports a type error on a correct line.
		"bare_trailing": "true",
	}
	for k, v := range want {
		if got[k].raw != v {
			t.Errorf("%s = %q, want %q", k, got[k].raw, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(got), len(want), got)
	}
	// Line numbers are what doctor prints; an off-by-one sends the user to the
	// wrong line of their own file.
	if got["base_url"].line != 3 {
		t.Errorf("base_url line = %d, want 3", got["base_url"].line)
	}
}

// TestParseFlatTOMLRejectsRatherThanIgnores is the point of writing a parser
// instead of taking a dependency. A reader that skips what it does not
// understand turns a construct someone reasonably expected to work into a
// setting that reads as applied while holding its default.
func TestParseFlatTOMLRejectsRatherThanIgnores(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"table", "[server]\nbase_url = \"x\"\n", "tables are not supported"},
		{"array", "model = [\"a\", \"b\"]\n", "arrays are not supported"},
		{"inline table", "model = {id = \"a\"}\n", "inline tables are not supported"},
		{"multiline string", "model = \"\"\"\nx\n\"\"\"\n", "multi-line strings are not supported"},
		{"dotted key", "a.b = \"x\"\n", "dotted key"},
		{"no equals", "base_url \"x\"\n", "expected key = value"},
		{"empty key", "= \"x\"\n", "empty key"},
		{"missing value", "base_url =\n", "missing value"},
		{"unterminated string", "base_url = \"oops\n", "unterminated string"},
		{"duplicate key", "model = \"a\"\nmodel = \"b\"\n", "set more than once"},
		{"bare value with text", "base_url = http://a b\n", "unexpected text after value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlatTOML(strings.NewReader(tc.doc), "test.toml")
			if err == nil {
				t.Fatalf("%s parsed without error; it must be refused, not skipped", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "test.toml") {
				t.Fatalf("error = %q, must name the file so the user can find it", err)
			}
		})
	}
}

// TestEscapedQuoteDoesNotEndABasicString covers the escape handling in
// splitQuoted, which no other case reached — mutation testing found that
// treating `\"` as a closing quote left every test green. The value would then
// be silently truncated at the escape, and the remainder would surface as
// "unexpected text after the closing quote" pointing at a line that is correct.
func TestEscapedQuoteDoesNotEndABasicString(t *testing.T) {
	got, err := parseFlatTOML(strings.NewReader(`model = "say \"hi\" now"`+"\n"), "test.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := `say "hi" now`; got["model"].raw != want {
		t.Fatalf("model = %q, want %q", got["model"].raw, want)
	}
	// A literal string has no escapes, so there the backslash is part of the
	// value and the first quote does close it. This is what makes Windows paths
	// writable without doubling separators.
	got, err = parseFlatTOML(strings.NewReader(`skills_dir = 'C:\skills\'`+"\n"), "test.toml")
	if err != nil {
		t.Fatalf("parse literal: %v", err)
	}
	if want := `C:\skills\`; got["skills_dir"].raw != want {
		t.Fatalf("skills_dir = %q, want %q", got["skills_dir"].raw, want)
	}
}

// TestControlCharactersAreRefused: a basic string accepts \u escapes, so
// without this a cloned repository's `.qeuro.toml` could smuggle an ANSI escape
// into a value that `config doctor` and the TUI both print. An OSC sequence
// retitles the window; a CSI or a bare \r can redraw the line so doctor displays
// one value while another is in effect, which destroys the only guarantee the
// command makes.
func TestControlCharactersAreRefused(t *testing.T) {
	// Written as TOML \u escapes, which is how a hostile file would carry them:
	// the control bytes are produced by the parser from plain ASCII source rather
	// than embedded in this test file.
	cases := map[string]string{
		"OSC window title": `x\u001B]0;PWNED\u0007y`,
		"CSI cursor move":  `a\u001B[2Kb`,
		"carriage return":  `visible\u000Dhidden`,
		"newline":          `a\u000Ab`,
		"NUL":              `a\u0000b`,
		"DEL":              `a\u007Fb`,
		"backspace":        `wrong\u0008\u0008\u0008\u0008\u0008right`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlatTOML(strings.NewReader(`model = "`+value+`"`+"\n"), "test.toml")
			if err == nil {
				t.Fatalf("%s was accepted; it would reach the terminal verbatim", name)
			}
			if !strings.Contains(err.Error(), "control character") {
				t.Fatalf("error = %q, want it to name the control character", err)
			}
		})
	}
	// A tab is whitespace someone might legitimately quote and cannot address the
	// terminal, so it stays allowed.
	got, err := parseFlatTOML(strings.NewReader(`model = "a\tb"`+"\n"), "test.toml")
	if err != nil {
		t.Fatalf("tab was refused: %v", err)
	}
	if got["model"].raw != "a\tb" {
		t.Fatalf("model = %q, want a tab preserved", got["model"].raw)
	}
}

// TestOverlongLineIsReportedInUserTerms: bufio's own message names the scanner
// ("token too long"), which tells the user nothing about their file.
func TestOverlongLineIsReportedInUserTerms(t *testing.T) {
	doc := "model = \"" + strings.Repeat("x", 70_000) + "\"\n"
	_, err := parseFlatTOML(strings.NewReader(doc), "test.toml")
	if err == nil {
		t.Fatal("an over-long line was accepted")
	}
	if !strings.Contains(err.Error(), "64 KiB") || !strings.Contains(err.Error(), "test.toml") {
		t.Fatalf("error = %q, want the file and a size the user can act on", err)
	}
}

// TestHashInsideBareValueIsNotAComment: treating it as one would silently
// truncate the value, which for a URL or a path is worse than refusing it.
func TestHashInsideBareValueIsNotAComment(t *testing.T) {
	got, err := parseFlatTOML(strings.NewReader("model = a#b\n"), "test.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["model"].raw != "a#b" {
		t.Fatalf("model = %q, want a#b", got["model"].raw)
	}
}

// TestQuotedStringsKeepSignificantWhitespace — quoting is the documented way to
// write a value with spaces, so trimming inside quotes would corrupt it.
func TestQuotedStringsKeepSignificantWhitespace(t *testing.T) {
	got, err := parseFlatTOML(strings.NewReader("model = \" spaced \"\n"), "test.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["model"].raw != " spaced " {
		t.Fatalf("model = %q, want %q", got["model"].raw, " spaced ")
	}
}

func TestUnquoteTOML(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`"a"`, "a", true},
		{`'a'`, "a", true},
		// unquoteTOML is only the unescaping step; the control-character check
		// lives one level up in parseTOMLScalar, which is what a config file goes
		// through. See TestControlCharactersAreRefused.
		{`"a\nb"`, "a\nb", true},
		{`'a\nb'`, `a\nb`, true}, // literal strings do not process escapes
		{`"unbalanced`, "", false},
		{`x`, "", false},
		{`"`, "", false},
		{``, "", false},
	}
	for _, tc := range cases {
		got, ok := unquoteTOML(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("unquoteTOML(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
