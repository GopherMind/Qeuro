package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"qeuro/internal/styles"
)

const completionUsage = "qeuro completion bash|zsh|fish"

// The completion scripts are generated from the same command registry that
// dispatch and `qeuro help` read. That is the whole point of generating them
// rather than shipping three hand-written files: a shipped script drifts the
// first time a command is added, and a completion that offers a command the
// binary does not have is worse than no completion — it teaches the wrong
// surface and the error only appears on Enter.
//
// What is NOT generated: values. Completing a model id or a session id means
// running the binary during completion, which turns pressing Tab into a network
// call or a directory scan on someone's prompt. Flags and subcommands are static
// facts about this build; values are not.

// completionSpec is the completable surface of one subcommand: the flags it
// parses and the fixed subcommand words it accepts. Both come from reading the
// argument parsers, so this table is the one place that has to be updated
// alongside them — and TestCompletionSpecCoversEveryCommand fails when it is not.
type completionSpec struct {
	flags []string // long flags this command accepts
	words []string // fixed positional subcommands
}

func completionSpecs() map[string]completionSpec {
	return map[string]completionSpec{
		"chat":    {flags: []string{"--budget", "--local", "--local-url", "--local-model"}},
		"run":     {flags: []string{"--headless", "--jsonl", "--model"}},
		"login":   {flags: []string{"--url"}},
		"whoami":  {},
		"cost":    {flags: []string{"--since", "--json"}},
		"star":    {},
		"logout":  {},
		"resume":  {words: []string{"list"}},
		"config":  {words: []string{"doctor"}},
		"mcp":     {words: []string{"list", "tools", "call", "serve"}},
		"fix":     {},
		"version": {},
		"help":    {},
		// completion completes itself: the shells are a fixed set, and a user who
		// has just learned the command exists is exactly who needs the hint.
		"completion": {words: []string{"bash", "zsh", "fish"}},
	}
}

func cmdCompletion(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "  "+styles.Err.Render("which shell? ")+styles.Muted.Render("bash, zsh or fish"))
		fmt.Fprintln(os.Stderr, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render(completionUsage))
		os.Exit(2)
	}
	if err := writeCompletion(os.Stdout, args); err != nil {
		fmt.Fprintln(os.Stderr, "  "+styles.Err.Render("error: ")+err.Error())
		fmt.Fprintln(os.Stderr, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render(completionUsage))
		os.Exit(2)
	}
}

// writeCompletion is separate from cmdCompletion because the script itself is
// the thing worth testing and os.Exit would end the test binary. The script goes
// to stdout and nothing else does: the output is meant to be eval'd or written
// to a file, so a stray styled line would be sourced as shell code.
func writeCompletion(w io.Writer, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("one shell at a time, got %d arguments", len(args))
	}
	shell := strings.ToLower(strings.TrimSpace(args[0]))
	switch shell {
	case "bash":
		return writeBashCompletion(w)
	case "zsh":
		return writeZshCompletion(w)
	case "fish":
		return writeFishCompletion(w)
	default:
		// Named, not listed generically: "unsupported shell" leaves the user
		// guessing which three words are accepted.
		return fmt.Errorf("unsupported shell %q — supported: bash, zsh, fish", shell)
	}
}

// completionNames returns every top-level word a completion should offer,
// sorted. Aliases are included on purpose: `--version` and `-v` dispatch, so a
// completion that hides them describes a different CLI than the one installed.
// Sorted because the registry order is a help-text decision and a shell list
// that reorders itself between releases produces noisy diffs in dotfiles.
func completionNames() []string {
	var names []string
	for _, c := range commands() {
		names = append(names, c.name)
		names = append(names, c.aliases...)
	}
	sort.Strings(names)
	return names
}

// dispatchWords returns every word that dispatches to the named command — the
// name and its aliases. The generated scripts match on the word the user typed,
// not on the canonical name, so without this `qeuro auto --bu` offers nothing
// while `qeuro chat --bu` completes: the alias runs the same command and has to
// complete like it.
func dispatchWords(name string) []string {
	for _, c := range commands() {
		if c.name == name {
			return append([]string{c.name}, c.aliases...)
		}
	}
	// A spec with no command is a bug the test below catches; emitting the key
	// keeps the generated script valid in the meantime.
	return []string{name}
}

// completionSummaries pairs each command name with its one-line summary for the
// shells that show descriptions (zsh, fish). Aliases are omitted here: three
// entries with identical text is noise in a menu, while in the plain word list
// above they are correctness.
func completionSummaries() [][2]string {
	var out [][2]string
	for _, c := range commands() {
		out = append(out, [2]string{c.name, c.summary})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
