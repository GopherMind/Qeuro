package main

import (
	"bytes"
	"strings"
	"testing"
)

// A completion script is shell source that lands in someone's login shell, so
// the tests below are about two things only: it describes the CLI that actually
// exists, and nothing in it can be read as code.

func TestCompletionCommandIsRegistered(t *testing.T) {
	var found *command
	for _, cmd := range commands() {
		if cmd.matches("completion") {
			c := cmd
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal(`no "completion" command: roadmap §8 requires qeuro completion bash|zsh|fish`)
	}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		if !strings.Contains(found.usage, shell) {
			t.Errorf("usage %q does not mention %s", found.usage, shell)
		}
	}
}

// The point of generating scripts instead of shipping them: a command added to
// the registry appears in the completion without anyone remembering to update it.
// A completion that offers a command the binary does not have, or omits one it
// does, teaches the wrong surface — and the user only finds out on Enter.
func TestEveryShellScriptOffersEveryCommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			script := generate(t, shell)
			for _, cmd := range commands() {
				if !strings.Contains(script, cmd.name) {
					t.Errorf("%s completion never mentions %q", shell, cmd.name)
				}
			}
		})
	}
}

// Aliases dispatch, so a completion that hides them describes a different CLI.
// bash and zsh match on the word the user typed, which is why the alias has to
// appear in the per-command branch too, not just in the top-level word list.
func TestAliasesCompleteLikeTheCommandTheyRunTest(t *testing.T) {
	bash := generate(t, "bash")
	if !strings.Contains(bash, "chat|auto)") {
		t.Error("bash: `qeuro auto --budget` would offer nothing, unlike `qeuro chat --budget`")
	}
	for _, alias := range []string{"auto", "-v", "--version", "-h", "--help"} {
		if !strings.Contains(bash, alias) {
			t.Errorf("bash: alias %q dispatches but is not completable", alias)
		}
	}
	if zsh := generate(t, "zsh"); !strings.Contains(zsh, "chat|auto)") {
		t.Error("zsh: the alias does not share the command's flag branch")
	}
	if fish := generate(t, "fish"); !strings.Contains(fish, "__fish_seen_subcommand_from chat auto") {
		t.Error("fish: the alias is not in the seen-subcommand list")
	}
}

// Every spec key must be a real command, or the generated script offers flags
// for something that cannot be run. This is the check that fails when a command
// is renamed and the spec table is not.
func TestCompletionSpecCoversEveryCommand(t *testing.T) {
	specs := completionSpecs()
	for _, cmd := range commands() {
		if _, ok := specs[cmd.name]; !ok {
			t.Errorf("command %q has no completion spec: add one (an empty spec is a valid answer)", cmd.name)
		}
	}
	real := map[string]bool{}
	for _, cmd := range commands() {
		real[cmd.name] = true
	}
	for name := range specs {
		if !real[name] {
			t.Errorf("completion spec %q is not a command — the script would offer arguments for nothing", name)
		}
	}
}

// The flags in the spec have to be the flags the parsers accept. A completion
// offering `--sicne` is a typo the user cannot detect: it came from the tool.
func TestCompletionFlagsAreTheFlagsTheParsersAccept(t *testing.T) {
	specs := completionSpecs()

	for _, flag := range specs["cost"].flags {
		if _, _, err := parseCostArgs(withValue(flag, "7d")); err != nil {
			t.Errorf("cost completion offers %q but parseCostArgs refuses it: %v", flag, err)
		}
	}
	for _, flag := range specs["chat"].flags {
		args := withValue(flag, chatFlagValue(flag))
		// The offline endpoint/model flags only mean anything alongside --local, and
		// parseChatArgs says so. Offer them the context they need rather than
		// asserting the parser accepts a combination it deliberately refuses.
		if strings.HasPrefix(flag, "--local-") {
			args = append([]string{"--local"}, args...)
		}
		if _, err := parseChatArgs(args); err != nil {
			t.Errorf("chat completion offers %q but parseChatArgs refuses it: %v", flag, err)
		}
	}
	// Both offline flags must stay refused without --local: the completion offering
	// them is a hint, not permission, and silently using the backend after the user
	// named a local endpoint is the failure that check protects.
	for _, flag := range []string{"--local-url", "--local-model"} {
		if _, err := parseChatArgs(withValue(flag, chatFlagValue(flag))); err == nil {
			t.Errorf("parseChatArgs accepted %q without --local", flag)
		}
	}
	// And the other direction for cost, the one command whose whole flag set is
	// known here: an added flag must reach the completion.
	if len(specs["cost"].flags) != 2 {
		t.Errorf("cost has %d completable flags; if a flag was added or removed, update the spec", len(specs["cost"].flags))
	}
}

func chatFlagValue(flag string) string {
	switch flag {
	case "--local-url":
		return "http://127.0.0.1:11434"
	case "--local-model":
		return "qwen2.5-coder:7b"
	default:
		return "20"
	}
}

// withValue gives a flag a plausible value so a parser that requires one does not
// fail for the wrong reason.
func withValue(flag, value string) []string {
	switch flag {
	case "--json", "--headless", "--jsonl", "--local":
		return []string{flag}
	default:
		return []string{flag, value}
	}
}

// The fixed subcommand words must be the ones those commands accept.
func TestCompletionWordsMatchTheSubcommandsThatExist(t *testing.T) {
	specs := completionSpecs()
	if got := specs["completion"].words; strings.Join(got, ",") != "bash,zsh,fish" {
		t.Errorf("completion offers %v, but writeCompletion accepts bash, zsh, fish", got)
	}
	for _, shell := range specs["completion"].words {
		var buf bytes.Buffer
		if err := writeCompletion(&buf, []string{shell}); err != nil {
			t.Errorf("completion offers %q but writeCompletion refuses it: %v", shell, err)
		}
	}
	if got := specs["resume"].words; strings.Join(got, ",") != "list" {
		t.Errorf("resume offers %v, want just list", got)
	}
}

func TestUnsupportedShellIsNamedNotGuessedAt(t *testing.T) {
	for _, args := range [][]string{
		{"powershell"}, {"cmd"}, {"nushell"}, {""}, {"BASH!"}, {"bash", "zsh"},
	} {
		var buf bytes.Buffer
		if err := writeCompletion(&buf, args); err == nil {
			t.Errorf("writeCompletion(%v) produced a script for a shell it does not support", args)
		}
	}
	// The error has to say which shells work: "unsupported shell" leaves the user
	// guessing at three words.
	var buf bytes.Buffer
	err := writeCompletion(&buf, []string{"powershell"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"bash", "zsh", "fish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s as a supported shell", err, want)
		}
	}
}

// Case and stray whitespace are shell accidents, not requests.
func TestShellNameIsNormalised(t *testing.T) {
	for _, in := range []string{"BASH", "  bash  ", "Zsh", "FISH"} {
		var buf bytes.Buffer
		if err := writeCompletion(&buf, []string{in}); err != nil {
			t.Errorf("writeCompletion(%q) errored: %v", in, err)
		}
		if buf.Len() == 0 {
			t.Errorf("writeCompletion(%q) produced nothing", in)
		}
	}
}

// A flag that takes a value must suppress suggestions entirely. Offering the
// command list after `--budget` tells the user that "logout" is a valid number of
// credits — the completion is then actively misleading rather than merely absent.
func TestValueFlagsSuppressSuggestions(t *testing.T) {
	script := generate(t, "bash")

	// Every flag that consumes the next word has to be in the guard.
	for _, flag := range bashValueFlags() {
		if !strings.Contains(script, flag) {
			t.Errorf("value flag %q is missing from the generated script", flag)
		}
	}
	// And the guard has to return without setting COMPREPLY, which is the only
	// thing that actually suppresses the list.
	guard := between(script, `case "$prev" in`, "esac")
	if guard == "" {
		t.Fatal("no $prev guard in the bash script: a value flag would be completed as a command")
	}
	for _, flag := range bashValueFlags() {
		if !strings.Contains(guard, flag) {
			t.Errorf("%q is not guarded, so the command list is offered as its value", flag)
		}
	}
	if !strings.Contains(guard, "return 0") {
		t.Error("the guard does not return, so COMPREPLY is still populated below it")
	}

	// The flags in the guard must be the ones that really take a value: a flag
	// listed here by mistake would silently stop completing after itself.
	valued := map[string]bool{"--budget": true, "--since": true, "--model": true, "--url": true}
	for _, flag := range bashValueFlags() {
		if !valued[flag] {
			t.Errorf("%q does not take a value, so guarding it suppresses completion for nothing", flag)
		}
	}
	// --json takes no value; guarding it would break `qeuro cost --json <TAB>`.
	if strings.Contains(guard, "--json") {
		t.Error("--json is guarded but takes no value")
	}
}

// between returns the text between the first occurrence of start and the next
// occurrence of end after it.
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// Nothing but the script may reach stdout. The output is eval'd or redirected
// into a file that gets sourced, so a styled status line would be run as code.
func TestScriptsCarryNoAnsiAndNoControlBytes(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script := generate(t, shell)
		if strings.Contains(script, "\x1b") {
			t.Errorf("%s script contains an ANSI escape", shell)
		}
		for i, r := range script {
			if r < 32 && r != '\n' && r != '\t' {
				t.Errorf("%s script has a control byte %#x at offset %d", shell, r, i)
			}
		}
	}
}

// The quoting is the security property of this whole feature. A summary is a
// sentence written by a human ("forget the token (and revoke it server-side)"),
// and one apostrophe closes a single-quoted string and turns the rest of the line
// into commands — in the user's login shell, at source time, before they have run
// anything. So the escaping is tested against the hostile cases directly rather
// than trusted because today's summaries happen not to contain a quote.
func TestShellSingleQuoteClosesNothing(t *testing.T) {
	cases := map[string]string{
		"plain":          `'plain'`,
		"it's":           `'it'\''s'`,
		"'; rm -rf /; '": `''\''; rm -rf /; '\'''`,
		`back\slash`:     `'back\slash'`,
		`$(id)`:          `'$(id)'`,
		"`id`":           "'`id`'",
		`${HOME}`:        `'${HOME}'`,
		`a'b'c`:          `'a'\''b'\''c'`,
		`"double"`:       `'"double"'`,
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %s, want %s", in, got, want)
		}
	}
	// The invariant behind the table: outside the quoted regions there is nothing
	// but the quote characters we put there, so a quoted string can never end early.
	for in := range cases {
		got := shellSingleQuote(in)
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("shellSingleQuote(%q) is not enclosed: %s", in, got)
		}
		if strings.Contains(got, `''`) && !strings.Contains(got, `'\''`) {
			t.Errorf("shellSingleQuote(%q) left an empty quote pair: %s", in, got)
		}
	}
}

// A quote in a summary must survive as data all the way into the script. Counting
// quote characters proves nothing here: inside the escape sequence
//
//	'\''
//
// the quote is backslash-escaped, so the raw count is legitimately odd. The
// property is that the *shell's* parser reads the whole thing as one word, so this
// decodes the quoting the way a shell does instead of pattern-matching it.
func TestAHostileSummaryCannotEscapeIntoCode(t *testing.T) {
	for _, hostile := range []string{
		`it's a trap'; touch /tmp/pwned; echo '`,
		`'; rm -rf ~; '`,
		"$(id)",
		"`id`",
		`plain`,
		`trailing'`,
		`'leading`,
	} {
		got, err := unquotePOSIX(shellSingleQuote(hostile))
		if err != nil {
			t.Errorf("shellSingleQuote(%q) = %s, which a shell cannot parse: %v",
				hostile, shellSingleQuote(hostile), err)
			continue
		}
		if got != hostile {
			t.Errorf("round trip changed the value: %q became %q", hostile, got)
		}
	}
}

// unquotePOSIX decodes a fully single-quoted POSIX word the way a shell does: text
// inside '...' is literal, and the only thing that can appear between quoted runs
// is a backslash-escaped quote. Anything else means the word ended early — which
// is exactly the failure being tested for, so it is an error rather than a
// best-effort parse.
func unquotePOSIX(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\'' {
			// Unquoted text: legal in a shell, but it means our quoting let something
			// out of the string, where the shell would interpret it.
			return "", errUnquoted
		}
		i++ // opening quote
		end := strings.IndexByte(s[i:], '\'')
		if end < 0 {
			return "", errUnterminated
		}
		out.WriteString(s[i : i+end])
		i += end + 1 // closing quote
		if i < len(s) {
			// Between quoted runs only \' is allowed.
			if i+1 < len(s) && s[i] == '\\' && s[i+1] == '\'' {
				out.WriteByte('\'')
				i += 2
				continue
			}
			return "", errUnquoted
		}
	}
	return out.String(), nil
}

var (
	errUnquoted     = errShell("text outside the quoted string")
	errUnterminated = errShell("unterminated quoted string")
)

type errShell string

func (e errShell) Error() string { return string(e) }

func TestFishQuoteEscapesBackslashBeforeQuote(t *testing.T) {
	// Order matters: escaping the quote first would then double the backslash that
	// was just added, producing \\' — an escaped backslash followed by a real quote,
	// which ends the string.
	if got, want := fishQuote(`it's`), `'it\'s'`; got != want {
		t.Errorf("fishQuote(%q) = %s, want %s", `it's`, got, want)
	}
	if got, want := fishQuote(`back\slash`), `'back\\slash'`; got != want {
		t.Errorf("fishQuote(%q) = %s, want %s", `back\slash`, got, want)
	}
	if got, want := fishQuote(`\'`), `'\\\''`; got != want {
		t.Errorf("fishQuote(%q) = %s, want %s", `\'`, got, want)
	}
}

// zsh's _describe splits value from description on a colon, so an unescaped colon
// in a summary silently truncates the description at that point.
func TestZshDescribeEscapesItsSeparators(t *testing.T) {
	if got, want := zshDescribe("usage: tokens"), `usage\: tokens`; got != want {
		t.Errorf("zshDescribe = %s, want %s", got, want)
	}
	if got, want := zshDescribe("a[b]c"), `a\[b\]c`; got != want {
		t.Errorf("zshDescribe = %s, want %s", got, want)
	}
	if got, want := zshDescribe(`back\slash`), `back\\slash`; got != want {
		t.Errorf("zshDescribe = %s, want %s", got, want)
	}
}

// Regenerating must produce identical bytes. Go randomises map iteration, and a
// script whose lines shuffle on every run produces a meaningless dotfiles diff
// and makes "did the completion change?" unanswerable.
func TestScriptsAreByteStableAcrossRuns(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		first := generate(t, shell)
		for i := 0; i < 5; i++ {
			if got := generate(t, shell); got != first {
				t.Fatalf("%s script changed between runs (iteration %d)", shell, i)
			}
		}
	}
}

func generate(t *testing.T, shell string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeCompletion(&buf, []string{shell}); err != nil {
		t.Fatalf("writeCompletion(%q) errored: %v", shell, err)
	}
	if buf.Len() == 0 {
		t.Fatalf("writeCompletion(%q) produced an empty script", shell)
	}
	return buf.String()
}
