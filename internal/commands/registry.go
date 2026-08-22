// Package commands is the single registry of slash commands. New commands are
// added here without touching the TUI core — the palette reads this list.
package commands

import "strings"

// Command describes one slash command shown in the palette.
type Command struct {
	Name   string // without the leading slash, e.g. "help"
	Desc   string // short description shown in the palette
	Hotkey string // optional hotkey hint shown on the right, e.g. "ctrl+l"
}

// registry is the ordered source of truth for available commands.
var registry = []Command{
	{"help", "show help and hotkeys", "?"},
	{"team", "AI agent team mode (orchestration)", ""},
	{"chat", "single-model chat mode", ""},
	{"clear", "clear conversation history", "ctrl+l"},
	{"context", "show context window usage", ""},
	{"usage", "token usage details: input/output/cache/credits", ""},
	{"model", "pick a model: brand → version", ""},
	{"effort", "model reasoning effort", ""},
	{"mode", "output mode: concise/full/caveman", ""},
	{"approvals", "auto-approval: ask/edits/all", ""},
	{"login", "sign in with your console API token: /login <token>", ""},
	{"logout", "sign out and remove the saved token", ""},
	{"providers", "AI providers linked to your web console account", ""},
	{"undo", "undo the last file edit", ""},
	{"resume", "restore a previous session: /resume [id]", ""},
	{"sessions", "recorded sessions and this one's id", ""},
	{"memory", "project memory (.infinity/)", ""},
	{"settings", "settings", ""},
	{"doctor", "environment diagnostics", ""},
	{"update", "check for updates", ""},
	{"exit", "exit Qeuro CLI", "ctrl+c ×2"},
}

// All returns every registered command.
func All() []Command { return registry }

// Filter returns commands whose name contains the query (case-insensitive),
// ranked so that prefix matches come before substring matches. The query is
// the text after the leading slash.
func Filter(query string) []Command {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return All()
	}
	var prefix, contains []Command
	for _, c := range registry {
		name := strings.ToLower(c.Name)
		switch {
		case strings.HasPrefix(name, q):
			prefix = append(prefix, c)
		case strings.Contains(name, q):
			contains = append(contains, c)
		}
	}
	return append(prefix, contains...)
}
