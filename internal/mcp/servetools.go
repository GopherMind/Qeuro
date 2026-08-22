package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
)

// Bounds on what the tools report. A tool result crosses into another agent's
// context window, so "the whole tree" is never the right answer; these are the
// sizes at which the answer is still useful and still cheap.
const (
	maxPlanEntries   = 60
	maxDiffEntries   = 200
	serveGitTimeout  = 10 * time.Second
	serveHTTPTimeout = 15 * time.Second
)

// serverToolFuncs binds each advertised tool to its implementation. It is a
// separate map from serverToolSpecs so a spec without an implementation fails at
// dispatch with "unknown tool" instead of a nil call, and so the pairing can be
// asserted in a test.
var serverToolFuncs = map[string]serverToolFunc{
	"qeuro.plan":     toolPlan,
	"qeuro.diff":     toolDiff,
	"qeuro.cost":     toolCost,
	"qeuro.run_task": toolRunTask,
}

// toolPlan describes the repository the CLI was started in.
func toolPlan(ctx context.Context, _ json.RawMessage) (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "cannot determine the working directory: " + err.Error(), true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "working directory: %s\n", wd)
	if branch, err := gitOutput(ctx, wd, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		fmt.Fprintf(&b, "git branch: %s\n", firstLineOf(branch))
	} else {
		b.WriteString("git branch: (not a git repository, or git is unavailable)\n")
	}

	entries, err := os.ReadDir(wd)
	if err != nil {
		return b.String() + "cannot read the directory: " + err.Error(), true
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && name != ".github" {
			// Dotfiles are noise for a layout summary, and .env-shaped files should
			// not be advertised to another agent at all.
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)

	b.WriteString("\ntop-level entries:\n")
	for i, n := range names {
		if i >= maxPlanEntries {
			fmt.Fprintf(&b, "  …and %d more\n", len(names)-maxPlanEntries)
			break
		}
		b.WriteString("  " + n + "\n")
	}

	if found := detectBuildFiles(wd); len(found) > 0 {
		b.WriteString("\nbuild files: " + strings.Join(found, ", ") + "\n")
	}
	return b.String(), false
}

// detectBuildFiles names the manifests that identify the stack.
func detectBuildFiles(dir string) []string {
	candidates := []string{
		"go.mod", "go.work", "package.json", "pnpm-workspace.yaml",
		"Cargo.toml", "pyproject.toml", "requirements.txt", "pom.xml",
		"build.gradle", "build.gradle.kts", "Makefile", "docker-compose.yml",
		"Dockerfile", "CLAUDE.md", "AGENTS.md",
	}
	var out []string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// toolDiff reports which files changed, by name and status only.
//
// File contents are deliberately excluded. A diff is the most sensitive thing in
// a working tree — it is where a half-finished credential edit or a private note
// lives — and this server answers an agent whose handling of the text we cannot
// see. Names and statuses are enough to decide what to look at next, and looking
// is the calling agent's own decision to make with its own tools.
func toolDiff(ctx context.Context, _ json.RawMessage) (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "cannot determine the working directory: " + err.Error(), true
	}
	out, err := gitOutput(ctx, wd, "status", "--porcelain")
	if err != nil {
		return "cannot read the working tree: " + err.Error(), true
	}
	lines := splitLines(strings.TrimRight(out, "\n"))
	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return "the working tree is clean", false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d changed path(s) (status and name only; contents are not exposed):\n", len(lines))
	for i, l := range lines {
		if i >= maxDiffEntries {
			fmt.Fprintf(&b, "  …and %d more\n", len(lines)-maxDiffEntries)
			break
		}
		b.WriteString("  " + sanitizeOneLine(l) + "\n")
	}
	return b.String(), false
}

// toolCost reports the plan and remaining credits.
//
// It reports only what GET /v1/me returns, because that is the only usage
// endpoint the backend has: there is no GET /v1/usage, so a per-day breakdown
// would have to be invented. Saying what is known beats returning a plausible
// number nobody can reconcile with an invoice.
func toolCost(ctx context.Context, _ json.RawMessage) (string, bool) {
	cfg, err := clientcfg.Load()
	if err != nil {
		return "the CLI configuration is unreadable: " + err.Error(), true
	}
	if !cfg.LoggedIn() {
		return "not signed in: run `qeuro login <token>` in the terminal", true
	}

	cctx, cancel := context.WithTimeout(ctx, serveHTTPTimeout)
	defer cancel()
	me, err := client.New(cfg.BaseURL, cfg.Secret()).Me(cctx)
	if err != nil {
		return "cannot reach the Qeuro backend: " + err.Error(), true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "plan: %s\n", valueOrUnknown(me.Tier))
	fmt.Fprintf(&b, "credits remaining: %.2f\n", me.CreditsBalance)
	if me.CreditsTotal > 0 {
		fmt.Fprintf(&b, "credits this period: %.2f\n", me.CreditsTotal)
	}
	if me.PeriodEnd != nil {
		fmt.Fprintf(&b, "period ends: %s\n", me.PeriodEnd.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n(this is the whole picture the backend exposes today: there is no " +
		"per-day usage endpoint, so no breakdown is reported rather than an invented one)\n")
	return b.String(), false
}

// toolRunTask reports the approval-channel limitation of this transport.
//
// It is advertised anyway, and it returns isError rather than a protocol error,
// because those are different messages to a caller: "no such tool" invites the
// agent to look for another name, while an execution error with this text tells
// it that the capability exists but this transport cannot safely host it.
func toolRunTask(_ context.Context, args json.RawMessage) (string, bool) {
	var p struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(nonEmptyJSON(args), &p); err != nil {
		return "arguments must be an object with a \"task\" string", true
	}
	if strings.TrimSpace(p.Task) == "" {
		return "the \"task\" argument is required", true
	}
	// Движок агента существует (internal/agentcore.Engine.Run), но этот
	// транспорт не может провести подтверждение: JSON-RPC занимает тот же stdin,
	// по которому Engine ждёт approval_response, поэтому запрос решения человека
	// здесь некому ответить. Единственный способ довести запуск до конца — снять
	// подтверждения, то есть отдать правку файлов и запуск команд MCP-клиенту,
	// которого мы не контролируем (.ai/RULES.md:24). Отказ честнее.
	return "not available over MCP: running a task needs an approval channel, and this transport " +
		"has none — the same stdin carries JSON-RPC, so a request for your decision cannot be answered. " +
		"Run `qeuro run --headless --jsonl` (the host answers approvals) or use the interactive CLI.", true
}

// ---- helpers -------------------------------------------------------------

// gitOutput runs one read-only git command in dir.
//
// argv, never a shell string (.ai/RULES.md:23). Every argument here is a
// compile-time literal; nothing from a tool call reaches this function, which is
// what keeps the read-only property true rather than merely intended.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, serveGitTimeout)
	defer cancel()
	// #nosec G204 -- fixed argv from literals in this file; no caller-supplied
	// argument is ever passed through, and there is no shell.
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func firstLineOf(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func valueOrUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

// nonEmptyJSON substitutes an empty object for absent arguments, so a caller that
// omits "arguments" entirely gets the missing-field message rather than a JSON
// syntax error.
func nonEmptyJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}
