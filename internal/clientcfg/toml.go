package clientcfg

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Roadmap §8 asks for `./.qeuro.toml` and `~/.qeuro/config.toml` in the
// precedence chain. `.ai/RULES.md:30` forbids adding a dependency without a
// demonstrated need, and a full TOML implementation is not needed here: the
// config surface is a flat set of scalars (see settingSpecs), so this reads the
// subset that surface can express and *rejects* everything else rather than
// guessing.
//
// Rejecting is the important half. A parser that silently skips what it does not
// understand turns a typo, or a construct someone reasonably expected to work
// (`[section]`, an array, a multi-line string), into a setting that reads as
// applied while holding its default — the same silent-green failure as an alert
// on a metric nobody exports. `qeuro config doctor` would then confidently
// report the wrong source. So an unsupported construct is a hard error naming
// the file and line, and the CLI surfaces it instead of running degraded.

// tomlValue is one parsed key with the line it came from, so diagnostics can
// point at the exact spot in the file.
type tomlValue struct {
	raw  string // the scalar as written, already unquoted for strings
	line int
}

// parseFlatTOML reads a flat key/value TOML document. Keys are bare or
// quoted; values are strings, integers or booleans. Tables, arrays, inline
// tables, dotted keys and multi-line strings are all refused by name so the
// message says what to do instead of only that something is wrong.
func parseFlatTOML(r io.Reader, name string) (map[string]tomlValue, error) {
	out := map[string]tomlValue{}
	sc := bufio.NewScanner(r)
	// A config file is small; a long line means something is wrong with it, and
	// the default 64 KiB token limit is already far past anything legitimate.
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)

	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "[") {
			return nil, fmt.Errorf("%s:%d: tables are not supported; this config is a flat list of key = value pairs", name, line)
		}
		key, rest, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected key = value", name, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", name, line)
		}
		// Quoted keys are accepted so a key can be written either way, but the
		// stored form is always bare — otherwise "token" and token would be two
		// different settings.
		if unquoted, ok := unquoteTOML(key); ok {
			key = unquoted
		}
		if strings.Contains(key, ".") {
			return nil, fmt.Errorf("%s:%d: dotted key %q is not supported; use a flat name", name, line, key)
		}
		if _, dup := out[key]; dup {
			// Last-wins would make the file's meaning depend on read order, and
			// first-wins would silently ignore the edit someone just made. Both
			// are worse than saying so.
			return nil, fmt.Errorf("%s:%d: %q is set more than once; remove one", name, line, key)
		}
		value, err := parseTOMLScalar(strings.TrimSpace(rest), name, line)
		if err != nil {
			return nil, err
		}
		out[key] = tomlValue{raw: value, line: line}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// "token too long" names the scanner, not the user's problem.
			return nil, fmt.Errorf("%s:%d: line is longer than 64 KiB; a config value should not be that large", name, line+1)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// parseTOMLScalar validates one right-hand side and returns its string form.
// Integers and booleans keep their written text; the typed helpers in
// settings.go convert them, so a type error is reported per setting with the
// expected type rather than generically here.
func parseTOMLScalar(text, name string, line int) (string, error) {
	if text == "" {
		return "", fmt.Errorf("%s:%d: missing value", name, line)
	}
	switch text[0] {
	case '[':
		return "", fmt.Errorf("%s:%d: arrays are not supported", name, line)
	case '{':
		return "", fmt.Errorf("%s:%d: inline tables are not supported", name, line)
	case '"', '\'':
		if strings.HasPrefix(text, `"""`) || strings.HasPrefix(text, "'''") {
			return "", fmt.Errorf("%s:%d: multi-line strings are not supported", name, line)
		}
		// Find the closing quote first, then check what follows. Unquoting the
		// whole remainder would fail on the common `key = "v"  # note` form, and
		// a `#` inside the quotes is part of the value, not a comment.
		quoted, rest, ok := splitQuoted(text)
		if !ok {
			return "", fmt.Errorf("%s:%d: unterminated string", name, line)
		}
		if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("%s:%d: unexpected text after the closing quote", name, line)
		}
		unquoted, ok := unquoteTOML(quoted)
		if !ok {
			return "", fmt.Errorf("%s:%d: invalid string escape", name, line)
		}
		if i := indexControl(unquoted); i >= 0 {
			// A basic string accepts \u escapes, so without this a project file
			// could smuggle an ANSI escape into a value. Every consumer of these
			// values prints them: `config doctor` renders the table, the TUI shows
			// a notice. An OSC sequence retitles the user's window, and a CSI or a
			// bare \r can redraw the line so doctor displays one thing while
			// another is in effect — which destroys the only guarantee the command
			// makes. No legitimate setting on this surface contains a control
			// character, so refusing is free.
			return "", fmt.Errorf("%s:%d: value contains a control character (0x%02x at byte %d); "+
				"escape sequences are not allowed in config values", name, line, unquoted[i], i)
		}
		return unquoted, nil
	}
	// Bare value: strip a trailing comment, but only when it is separated by
	// whitespace. `#` inside a bare token is not a comment, and treating it as
	// one would silently truncate the value.
	if i := strings.IndexAny(text, " \t"); i >= 0 {
		head, tail := text[:i], strings.TrimSpace(text[i:])
		if !strings.HasPrefix(tail, "#") {
			return "", fmt.Errorf("%s:%d: unexpected text after value; quote it if the value contains spaces", name, line)
		}
		text = head
	}
	return text, nil
}

// indexControl returns the offset of the first C0/C1 control byte, or -1. Tab is
// allowed: it is whitespace a person might legitimately quote, and it cannot move
// the cursor or address the terminal.
func indexControl(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return i
		}
	}
	return -1
}

// splitQuoted returns the quoted run at the start of s (including both quotes)
// and whatever follows it. For basic strings a `\"` does not close the string;
// for literal strings there are no escapes, so the first matching quote wins.
func splitQuoted(s string) (quoted, rest string, ok bool) {
	q := s[0]
	escaped := false
	for i := 1; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		if q == '"' && s[i] == '\\' {
			escaped = true
			continue
		}
		if s[i] == q {
			return s[:i+1], s[i+1:], true
		}
	}
	return "", "", false
}

// unquoteTOML removes matching quotes. Basic strings go through strconv so the
// usual escapes work; literal strings are taken verbatim, which is what makes
// Windows paths writable without doubling every backslash.
func unquoteTOML(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	switch {
	case s[0] == '"' && s[len(s)-1] == '"':
		v, err := strconv.Unquote(s)
		if err != nil {
			return "", false
		}
		return v, true
	case s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1], true
	}
	return "", false
}
