// Package tools implements the local file-operation tools the model can call
// (function calling). The backend forwards the model's tool-call requests to
// the CLI; the CLI executes them here against the user's working directory and
// returns the results, so the model never has to emit whole files as text.
//
// Every path is sandboxed to the project root: a tool call that resolves
// outside the root is rejected. Only the standard library is used.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"qeuro/internal/clientcfg"
	"qeuro/internal/hooks"
	"qeuro/internal/memory"
	"qeuro/internal/styles"
)

// CommandOKPrefix is the status prefix runCommand emits when a shell command
// exits with code 0. The TUI verification gate matches on it, so both sides
// must stay in sync via this constant.
const CommandOKPrefix = "ok (exit code 0)"

// CommandOutputSeparator divides runCommand's status line from the command's own
// literal output. It is exported because it is a format two other packages read
// back: the agent protocol carries the output as command evidence
// (agentcore.commandOutputOf), and the console renders it in the task page's
// evidence panel. An unexported separator with copies elsewhere is the drift this
// constant prevents.
const CommandOutputSeparator = "--- output ---"

// maxReadBytes caps how much of a file read_file returns, to protect the
// context window. Larger files are truncated with a marker.
const maxReadBytes = 64 * 1024

// maxCmdOutput caps captured command output; commandTimeout bounds runtime.
const (
	maxCmdOutput   = 16 * 1024
	commandTimeout = 120 * time.Second
)

const wholeFilePatchLineThreshold = 8

// Definitions returns the OpenAI-style tool (function) definitions advertised
// to the model. The result is marshalled straight into the chat request's
// "tools" field.
//
// Only the eight built-ins. MCP tool definitions are deliberately NOT included
// here: they are subject to a description token budget and are added by the
// caller via WithMCP, so a server with a hundred verbose tools cannot silently
// enlarge every request (roadmap §4.8).
func Definitions() json.RawMessage {
	return definitionsFrom(builtins)
}

// DefaultMCPDescriptionBudget bounds the total bytes of MCP tool names and
// descriptions sent to the model (.ai/AI.md:49, "bounded discovery
// descriptions").
//
// The cost is per request, not per session: tool definitions are re-sent on every
// step of a tool loop, so a server with fifty verbose tools is paid for many times
// in one turn. 8 KiB is roughly two thousand tokens, which fits a realistic set
// of allow-listed tools while keeping a server that pads its descriptions from
// crowding out the conversation.
const DefaultMCPDescriptionBudget = 8 << 10

// WithMCP returns the built-in definitions plus the MCP tools that fit within
// budget bytes of description text, and reports how many were dropped.
//
// Dropping is deterministic — MCPSpecs sorts by name, so the same set is offered
// on every request within a session. That matters more than which tools win: a
// tool list that changed between steps of one turn would make the model's choices
// unreproducible and would break provider prompt caching on every request.
func WithMCP(budget int) (defs json.RawMessage, dropped int) {
	specs := append([]Spec(nil), builtins...)
	spent := 0
	for _, s := range MCPSpecs() {
		cost := len(s.Name) + len(s.Description)
		if spent+cost > budget {
			dropped++
			continue
		}
		spent += cost
		specs = append(specs, s)
	}
	return definitionsFrom(specs), dropped
}

// funcDef wraps one tool as an OpenAI-style function definition. params is the
// tool's JSON Schema; it is emitted as-is, so an MCP server's schema reaches the
// model unmodified except for the checks in the mcp package.
func funcDef(name, desc string, params map[string]any) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters":  params,
		},
	}
}

// Runner executes tool calls against a fixed project root and keeps durable
// content-addressed checkpoints for file changes so an applied edit can be rolled
// back after a process restart. Checkpoint artifacts live under .infinity and are
// never exposed through tools or model context.
//
// A single Runner is shared between the UI goroutine (which calls Undo on a
// /undo command) and a team run's worker goroutine (which calls Execute), so
// all access to checkpoint state and local memory is serialized by mu (C4: data
// race). The project root is immutable after construction and needs no locking.
type Runner struct {
	root string

	// base is the tree this Runner reads through to when it is an isolated
	// per-writer worktree (roadmap-v3 §4.1); empty for an ordinary Runner working
	// directly in the user's tree. When set, root is an overlay: reads fall through
	// to base, and every write is confined to root. See isolate.go.
	base string

	mu          sync.Mutex // guards checkpoint mutations and serializes file changes + memory writes
	checkpoints *checkpointStore
	mem         *memory.Store // local project memory (.infinity/)
}

// NewRunner returns a Runner rooted at dir (typically the working directory).
func NewRunner(dir string) (*Runner, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Runner{root: abs, checkpoints: newCheckpointStore(abs), mem: memory.New(abs)}, nil
}

// Memory exposes the local project-memory store (for the TUI to inject a
// digest and open session logs).
func (r *Runner) Memory() *memory.Store { return r.mem }

// Names of the tools, for display.
const (
	ToolReadFile   = "read_file"
	ToolListDir    = "list_dir"
	ToolSearchCode = "search_code"
	ToolPatchFile  = "patch_file"
	ToolWriteFile  = "write_file"
	ToolRunCommand = "run_command"
	ToolRemember   = "remember"
	ToolRecall     = "recall"
)

// Execute runs one named tool with JSON-string arguments and returns a result
// string (suitable as the content of a tool-role message). The bool reports
// whether the call mutated the filesystem (for UI emphasis). Errors are
// returned as a readable string in the result, not as a Go error, because the
// model is expected to read and react to them.
func (r *Runner) Execute(name, argsJSON string) (result string, mutated bool) {
	switch name {
	case ToolReadFile:
		return r.readFile(argsJSON), false
	case ToolListDir:
		return r.listDir(argsJSON), false
	case ToolSearchCode:
		return r.searchCode(argsJSON), false
	case ToolPatchFile:
		return r.patchFile(argsJSON)
	case ToolWriteFile:
		return r.writeFile(argsJSON)
	case ToolRunCommand:
		return r.runCommand(argsJSON)
	case ToolRemember:
		return r.remember(argsJSON), false
	case ToolRecall:
		return r.recall(argsJSON), false
	default:
		return "error: unknown tool " + name, false
	}
}

// checkpointFile durably saves a complete file checkpoint before a mutating
// operation. Callers hold r.mu; persistence failure prevents the workspace
// mutation, and a source-write failure removes the otherwise unapplied HEAD.
func (r *Runner) checkpointFile(rel, tool string, next []byte, nextExists bool) (checkpointRecord, error) {
	return r.checkpoints.checkpoint(rel, tool, next, nextExists)
}

// Undo restores the newest durable checkpoint and refuses workspace drift.
func (r *Runner) Undo() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpoints.undo(r.resolve, r.ensureInsideRoot)
}

// UndoDepth reports the bounded validated durable checkpoint lineage.
func (r *Runner) UndoDepth() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpoints.depth()
}

// Summary renders a one-line human description of a tool call for the UI.
func Summary(name, argsJSON string) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &a)
	get := func(k string) string {
		if v, ok := a[k].(string); ok {
			return v
		}
		return ""
	}
	switch name {
	case ToolReadFile:
		return "reading " + get("path")
	case ToolListDir:
		p := get("path")
		if p == "" {
			p = "."
		}
		return "listing " + p
	case ToolSearchCode:
		return "searching «" + get("query") + "»"
	case ToolPatchFile:
		return "editing " + get("path")
	case ToolWriteFile:
		return "writing " + get("path")
	case ToolRunCommand:
		return "$ " + get("command")
	case ToolRemember:
		cat := get("category")
		if cat == "" {
			cat = "notes"
		}
		return "remember → " + cat
	case ToolRecall:
		cat := get("category")
		if cat == "" {
			return "memory: digest"
		}
		return "memory: " + cat
	default:
		// A registered MCP tool: name the server, because that is the fact the
		// user needs when deciding whether to approve. The server-supplied
		// description is deliberately not shown here — it is untrusted text, and
		// a one-line UI summary is a poor place to render it.
		if s, ok := lookup(name); ok && s.Origin == OriginMCP {
			return "mcp " + s.Server + ": " + s.Tool()
		}
		return name
	}
}

// Mutating reports whether a tool changes the filesystem (drives the ✎ glyph).
// False for MCP tools: whether a remote tool writes is not something the client
// can know, and the glyph promises an undo we cannot offer for a remote effect.
func Mutating(name string) bool {
	s, ok := lookup(name)
	return ok && s.Mutating
}

// TouchesWorkTree reports whether a call can change the shared working tree.
// This is deliberately a third predicate rather than a reuse of Mutating or
// RequiresApproval, because the three answer different questions and disagree on
// real tools (roadmap-v3 §4.1):
//
//   - Mutating drives the ✎ glyph and is false for run_command, since that tool
//     does not go through the undo stack. But a command absolutely can rewrite
//     the tree — `go build -o x`, `npm install`, `git checkout` — so a
//     concurrency rule built on Mutating would wave commands through.
//   - RequiresApproval asks "may this run without a human". It is fail-closed to
//     true for unknown names, which is right for approval and wrong here: the
//     answer "requires approval" says nothing about whether two agents doing it
//     at once corrupt each other's work.
//
// Fail-closed for anything not a local built-in: an MCP tool's effects are
// described by the server that hosts it, and .ai/RULES.md:22 forbids letting
// untrusted text decide an authorization question. So an unrecognized name is
// treated as a writer, which is the answer that costs concurrency rather than
// correctness. remember() is a writer too: it appends to .infinity/memory, which
// is shared state even though it is not source.
func TouchesWorkTree(name string) bool {
	switch name {
	case ToolReadFile, ToolListDir, ToolSearchCode, ToolRecall:
		return false
	default:
		return true
	}
}

// RequiresApproval reports whether a tool must be confirmed by the user before
// running: file edits and arbitrary command execution. Read-only built-ins
// (read, list, search, recall, remember) run automatically.
//
// Fail-closed by design (.ai/RULES.md:12): a name that is neither a built-in nor
// a registered MCP tool requires approval. Before the registry this returned
// false for every unknown name, which meant an MCP tool — or a name the model
// simply invented — would have executed with no human in the loop. Approval is
// the second line here anyway: dispatch also refuses unknown names outright.
func RequiresApproval(name string) bool {
	s, ok := lookup(name)
	if !ok {
		return true
	}
	return s.Approval
}

// formatDiff renders old and new content as a colored diff when useANSI is true.
// Lines are capped to avoid flooding the approval prompt.
//
// Both sides are put through clientcfg.DisplaySafe first, and that is a security
// property rather than tidiness. This text is a tool-call argument, so the model
// chose every byte of it, and the panel it lands in is the one place the user
// looks before letting an edit touch their disk (.ai/SECURITY.md: model output is
// data). A CSI sequence smuggled through old_content would repaint that panel from
// the inside — erase the lines above it, move the cursor over the option list, or
// push the real diff out of view and leave a harmless-looking one behind. A lone
// carriage return is enough on its own: it rewrites the line it sits on.
//
// Escaped rather than stripped, per line rather than per block: the user should be
// able to see that the model tried, a silently shortened line hides the attempt,
// and the line split has to happen on our newlines, not the model's. This is also
// why sanitising cannot be done by turning colour off — our sequences frame a
// line, the model's would rewrite the screen, and the row asks for a diff that
// looks like the UI's whatever the input contains.
func formatDiff(old, new string, useANSI bool) string {
	var b strings.Builder
	oldLines := safeLines(capLines(old, 8))
	newLines := safeLines(capLines(new, 8))

	if useANSI {
		// Create a dedicated renderer with forced TrueColor profile and TTY=true.
		// lipgloss requires both WithProfile AND WithTTY to emit ANSI codes when
		// the output isn't actually a terminal (tests, redirected output).
		r := lipgloss.NewRenderer(os.Stdout, termenv.WithProfile(termenv.TrueColor), termenv.WithTTY(true))
		red := r.NewStyle().Foreground(styles.Red)
		green := r.NewStyle().Foreground(styles.Green)
		for _, ln := range oldLines {
			b.WriteString(red.Render("- "+ln) + "\n")
		}
		for _, ln := range newLines {
			b.WriteString(green.Render("+ "+ln) + "\n")
		}
	} else {
		for _, ln := range oldLines {
			b.WriteString("- " + ln + "\n")
		}
		for _, ln := range newLines {
			b.WriteString("+ " + ln + "\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// Preview renders a human-readable preview of a mutating tool call (a diff for
// patch_file, a content head for write_file) for the approval prompt. Lines
// are capped so a huge file does not flood the screen.
func Preview(name, argsJSON string) string {
	var a struct {
		Path       string `json:"path"`
		OldContent string `json:"old_content"`
		NewContent string `json:"new_content"`
		Content    string `json:"content"`
		Command    string `json:"command"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)

	switch name {
	case ToolRunCommand:
		if line := SanitizedCommandLine(a.Command); line != "" {
			return "⚠ runs on your machine (no shell; allow-listed tool):\n$ " + line
		}
		// The refused branch is the one that needs escaping most: it prints the
		// model's string verbatim precisely because SanitizeCommand would not
		// vouch for it, so the bytes reaching the terminal here have passed no
		// check at all. One line, because a command is one line.
		return "$ " + clientcfg.DisplaySafe(a.Command)
	case ToolPatchFile:
		return formatDiff(a.OldContent, a.NewContent, clientcfg.ShouldUseANSI())
	case ToolWriteFile:
		n := strings.Count(a.Content, "\n") + 1
		var b strings.Builder
		fmt.Fprintf(&b, "(%d lines)\n", n)
		for _, ln := range safeLines(capLines(a.Content, 10)) {
			b.WriteString("  " + ln + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	default:
		if s, ok := lookup(name); ok && s.Origin == OriginMCP {
			return mcpPreview(s, argsJSON)
		}
		return ""
	}
}

// mcpPreview renders the approval prompt for an MCP call. The user is deciding
// whether to let a third-party process run, so the two facts that matter are
// which server and which arguments — shown verbatim and capped, never
// interpreted. The server's own description of the tool is not shown: it is
// untrusted text, and this prompt is the one place where a persuasive sentence
// written by a server would be read as advice from us.
func mcpPreview(s Spec, argsJSON string) string {
	var b strings.Builder
	b.WriteString("⚠ calls MCP server «" + s.Server + "», tool " + s.Tool() + "\n")
	args := strings.TrimSpace(argsJSON)
	if args == "" || args == "{}" {
		b.WriteString("  (no arguments)")
		return b.String()
	}
	// Re-encode indented when it parses, so a long single-line JSON blob is
	// readable; fall back to the raw text when it does not, because refusing to
	// show anything would be worse than showing what the model actually sent.
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		if pretty, err := json.MarshalIndent(v, "  ", "  "); err == nil {
			args = string(pretty)
		}
	}
	// json.MarshalIndent re-escapes control bytes on the branch where the
	// arguments parse, but the fallback branch prints the raw text by design, and
	// that is the branch a hostile caller picks by sending JSON that does not
	// parse. Escaping both keeps the two branches from differing in what they let
	// through.
	for _, ln := range safeLines(capLines(args, 12)) {
		b.WriteString("  " + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// safeLines escapes control characters in each line of an already-split preview.
//
// It runs after capLines, not before: the split has to happen on real newlines,
// and DisplaySafe escapes those too (a preview line that spans rows can push the
// real content out of view). Splitting first and escaping the pieces keeps the
// line structure ours while leaving nothing addressable to the terminal inside
// them. The slice is rewritten in place — capLines already returns a fresh
// backing array in the truncating case and a slice of one in the common case, and
// neither is retained by the caller.
func safeLines(lines []string) []string {
	for i, ln := range lines {
		lines[i] = clientcfg.DisplaySafe(ln)
	}
	return lines
}

// capLines splits s into lines and returns at most max of them, appending an
// ellipsis marker when truncated.
func capLines(s string, max int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= max {
		return lines
	}
	out := lines[:max]
	return append(out, fmt.Sprintf("… (+%d lines)", len(lines)-max))
}

// resolve validates a user-supplied path and returns its absolute form,
// guaranteeing it stays within the project root (prevents path traversal).
func (r *Runner) resolve(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("absolute paths are not allowed: %s", rel)
	}
	abs := filepath.Join(r.root, clean)
	if err := r.ensureInsideRoot(abs); err != nil {
		return "", fmt.Errorf("path outside the project: %s", rel)
	}
	return abs, nil
}

// ensureInsideRoot verifies both the lexical path and the real filesystem target
// stay under r.root. The component walk stops malicious repos from smuggling
// external files through symlinks/junctions/reparse points.
func (r *Runner) ensureInsideRoot(abs string) error {
	root := filepath.Clean(r.root)
	abs = filepath.Clean(abs)
	if err := checkInside(root, abs); err != nil {
		return err
	}
	if err := checkExistingPathComponents(root, abs); err != nil {
		return err
	}
	// Physical canonicalization is platform-specific. On Windows, managed or
	// ACL-restricted filesystems can deny EvalSymlinks even after every path
	// component was successfully inspected above; the Windows implementation
	// has a narrow permission-error fallback while retaining the lexical and
	// reparse-point gates. Unix keeps this check strict.
	if err := checkCanonicalContainment(root, abs); err != nil {
		return fmt.Errorf("outside root")
	}
	return nil
}

// strictCanonicalContainment resolves both the project root and the deepest
// existing ancestor of target, then requires the physical target to stay below
// the physical root. Platform wrappers decide whether a filesystem-specific
// failure has a safe fallback after the lexical and component checks above.
func strictCanonicalContainment(root, target string) error {
	canonRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	canonTarget, err := canonicalizePath(target)
	if err != nil {
		return err
	}
	return checkInside(canonRoot, canonTarget)
}

// canonicalizePath resolves abs to its physical (symlink-free) path. For a
// path that does not fully exist yet, the deepest existing ancestor is
// resolved and the non-existing suffix re-joined, so new files are validated
// against the real directory they will land in.
func canonicalizePath(abs string) (string, error) {
	suffix := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if suffix == "" {
				return filepath.Clean(resolved), nil
			}
			return filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

func checkInside(root, target string) error {
	root = normalizePathForCompare(root)
	target = normalizePathForCompare(target)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("outside root")
	}
	return nil
}

func checkExistingPathComponents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if isSymlinkOrReparse(info) {
			return fmt.Errorf("path crosses symlink or reparse point")
		}
	}
	return nil
}

func normalizePathForCompare(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, `\\?\`)
		path = strings.TrimPrefix(path, `\??\`)
		path = strings.ToLower(path)
	}
	return path
}

func (r *Runner) readFile(argsJSON string) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return "error: path argument required"
	}
	// resolveRead, not resolve: in an isolated worktree the writer must see its own
	// edits and fall through to the base tree for everything it has not touched.
	// For an ordinary Runner this is exactly resolve.
	abs, err := r.resolveRead(args.Path)
	if err != nil {
		return "error: " + err.Error()
	}
	if err := checkHardLink(abs); err != nil {
		return "error: " + err.Error()
	}
	// #nosec G304 -- abs comes from r.resolveRead, which resolves through r.resolve
	// for the overlay and resolveBase for the base tree; both apply the same
	// containment checks, and checkHardLink above rejects multiply-linked files, so
	// the read target cannot be redirected outside either root.
	data, err := os.ReadFile(abs)
	if err != nil {
		return "read error: " + err.Error()
	}
	if len(data) > maxReadBytes {
		return string(data[:maxReadBytes]) + "\n…[file truncated, showing first " + itoa(maxReadBytes) + " bytes]"
	}
	return string(data)
}

// ignoredDirs are directories never worth showing the model — they waste
// context and are rarely relevant to a coding task (token economy, plan §8.1).
var ignoredDirs = map[string]bool{
	"node_modules": true, ".git": true, ".infinity": true, "dist": true, "build": true,
	"vendor": true, ".next": true, "target": true, "__pycache__": true,
	".venv": true, ".idea": true, ".cache": true, "bin": true, "obj": true,
}

func (r *Runner) listDir(argsJSON string) string {
	var args struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	if args.Path == "" {
		args.Path = "."
	}
	// An isolated writer lists the union of its overlay and the base tree. Listing
	// only the overlay would show a nearly empty project and send the model looking
	// for files it can plainly read; listing only the base would hide the files the
	// writer just created.
	entries, err := r.readDirMerged(args.Path)
	if err != nil {
		return "list error: " + err.Error()
	}
	seen := make(map[string]bool, len(entries))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if ignoredDirs[name] {
				continue // skip junk dirs
			}
			name += "/"
		}
		if seen[name] {
			continue // present in both trees
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty)"
	}
	return strings.Join(names, "\n")
}

func (r *Runner) patchFile(argsJSON string) (string, bool) {
	var args struct {
		Path       string `json:"path"`
		OldContent string `json:"old_content"`
		NewContent string `json:"new_content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: invalid arguments", false
	}
	if args.Path == "" || args.OldContent == "" {
		return "error: path and old_content required", false
	}
	// In an isolated worktree the file has to become the writer's own before it is
	// patched, or the patch would land on a path that reads through to the base
	// tree — which is the shared-tree behaviour this isolation exists to remove.
	// materialize is a no-op for an ordinary Runner and idempotent per path.
	if err := r.materialize(args.Path); err != nil {
		return "write blocked: " + err.Error(), false
	}
	abs, err := r.resolve(args.Path)
	if err != nil {
		return "error: " + err.Error(), false
	}
	if err := checkHardLink(abs); err != nil {
		return "write blocked: " + err.Error(), false
	}
	// #nosec G304 -- abs comes from r.resolve (absolute paths rejected, result
	// confined under r.root by ensureInsideRoot) and checkHardLink above rejects
	// multiply-linked files, so the read target cannot be redirected outside.
	data, err := os.ReadFile(abs)
	if err != nil {
		return "read error: " + err.Error(), false
	}
	content := string(data)
	count := strings.Count(content, args.OldContent)
	if count == 0 {
		return "error: old_content not found in file", false
	}
	if count > 1 {
		return fmt.Sprintf("error: old_content occurs %d times; make the fragment unique", count), false
	}
	if wholeFilePatch(content, args.OldContent) {
		return "error: patch_file old_content matches the whole file. Use a smaller unique fragment so only the necessary lines change.", false
	}
	updated := strings.Replace(content, args.OldContent, args.NewContent, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.checkpointFile(args.Path, ToolPatchFile, []byte(updated), true)
	if err != nil {
		return "checkpoint error: " + err.Error(), false
	}
	// #nosec G703,G306 -- abs comes from r.resolve, which rejects absolute paths
	// and verifies containment via ensureInsideRoot; the taint analysis cannot
	// see that check across the call boundary. 0644 is deliberate: this writes a
	// file in the user's own source tree, which is meant to stay readable.
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		if abandonErr := r.checkpoints.abandon(rec); abandonErr != nil {
			return "write error: " + err.Error() + "; checkpoint unavailable: " + abandonErr.Error(), false
		}
		return "write error: " + err.Error(), false
	}

	// Вызываем post-diff hook асинхронно
	go r.runPostDiffHook(abs, content, updated)

	return "ok: file " + args.Path + " modified", true
}

func wholeFilePatch(content, oldContent string) bool {
	if strings.TrimSpace(content) != strings.TrimSpace(oldContent) {
		return false
	}
	return strings.Count(content, "\n")+1 > wholeFilePatchLineThreshold
}

func (r *Runner) writeFile(argsJSON string) (string, bool) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return "error: path and content required", false
	}
	// The existence check runs against the merged view (resolveRead), not the
	// overlay alone. In an isolated worktree the overlay starts empty, so checking
	// only there would report every existing project file as absent and let
	// write_file replace it wholesale — turning the isolation into a way around the
	// "use patch_file for existing files" rule rather than a way to contain writes.
	abs, err := r.resolveRead(args.Path)
	if err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			return "error: " + args.Path + " already exists; do not rewrite existing files with write_file. Use read_file, then patch_file with the smallest old_content/new_content replacement.", false
		} else if !os.IsNotExist(statErr) {
			return "error checking file: " + statErr.Error(), false
		}
	}
	// Writes always go to this Runner's own root.
	abs, err = r.resolve(args.Path)
	if err != nil {
		return "error: " + err.Error(), false
	}
	if err := checkHardLink(abs); err != nil {
		return "write blocked: " + err.Error(), false
	}
	if dir := filepath.Dir(abs); dir != "" {
		// #nosec G301 -- creating a directory in the user's own source tree;
		// project directories are meant to be traversable, so 0750 would be wrong.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "mkdir error: " + err.Error(), false
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.checkpointFile(args.Path, ToolWriteFile, []byte(args.Content), true)
	if err != nil {
		return "checkpoint error: " + err.Error(), false
	}
	// #nosec G306 -- writing a file in the user's own source tree; project
	// sources are meant to be group/world readable, so 0600 would be wrong here.
	if err := os.WriteFile(abs, []byte(args.Content), 0o644); err != nil {
		if abandonErr := r.checkpoints.abandon(rec); abandonErr != nil {
			return "write error: " + err.Error() + "; checkpoint unavailable: " + abandonErr.Error(), false
		}
		return "write error: " + err.Error(), false
	}

	// Вызываем post-diff hook асинхронно (не блокируем операцию)
	go r.runPostDiffHook(abs, "", args.Content)

	return "ok: file " + args.Path + " written", true
}

// forbiddenShellChars are the shell metacharacters that enable pipelines,
// redirection, substitution and grouping. Commands are executed WITHOUT a
// shell (see runCommand), but these are rejected outright so a sanitized argv
// can never smuggle shell syntax into a tool that itself spawns a shell.
const forbiddenShellChars = ";&|$`><(){}"

// allowedCommands is the white-list of executables run_command may launch.
// Everything else is refused — including every shell and interpreter — so the
// model can drive ordinary build/test/lint workflows but cannot obtain
// arbitrary code execution through the command tool.
var allowedCommands = map[string]bool{
	"git": true, "go": true, "npm": true, "yarn": true, "pnpm": true,
	"make": true, "docker": true, "cargo": true, "pip": true, "pip3": true,
	"python": true, "python3": true, "poetry": true, "gcc": true,
}

// blockedInterpreters are executables that would turn an approved command into
// arbitrary code execution or an exfiltration channel. They are named
// explicitly (instead of relying on the allow-list alone) so the refusal
// message is clear about WHY the command is dangerous.
var blockedInterpreters = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"powershell": true, "pwsh": true, "cmd": true, "command": true, "iex": true,
	"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true,
	"perl": true, "ruby": true, "node": true, "deno": true, "bun": true, "php": true,
	"eval": true, "exec": true, "env": true, "xargs": true,
	"sudo": true, "doas": true, "su": true, "runas": true,
}

// parseCommandLine splits a command into an argv vector without any shell
// involved. It understands single and double quotes; everything else splits on
// whitespace. Shell metacharacters are rejected wholesale — quoting them does
// not make them safe, because several allow-listed tools re-interpret their
// arguments.
func parseCommandLine(command string) ([]string, error) {
	if strings.ContainsAny(command, forbiddenShellChars) {
		return nil, fmt.Errorf("shell metacharacters (%s) are not allowed", forbiddenShellChars)
	}
	if strings.ContainsAny(command, "\r\n") {
		return nil, fmt.Errorf("newlines are not allowed in commands")
	}
	var (
		argv    []string
		cur     strings.Builder
		quote   rune
		inQuote bool
		hasCur  bool
	)
	for _, r := range command {
		switch {
		case inQuote:
			if r == quote {
				inQuote = false
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			inQuote, quote, hasCur = true, r, true
		case unicode.IsSpace(r):
			if hasCur {
				argv = append(argv, cur.String())
				cur.Reset()
				hasCur = false
			}
		default:
			cur.WriteRune(r)
			hasCur = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unbalanced quotes")
	}
	if hasCur {
		argv = append(argv, cur.String())
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return argv, nil
}

// normalizeBinName reduces an executable reference to a comparable name:
// lower-cased, unquoted, directory prefix stripped, ".exe" suffix stripped.
func normalizeBinName(s string) string {
	s = strings.Trim(strings.ToLower(s), `"'`)
	s = filepath.Base(filepath.FromSlash(s))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		s = strings.TrimSuffix(s, ext)
	}
	return s
}

func allowedCommandList() string {
	names := make([]string, 0, len(allowedCommands))
	for n := range allowedCommands {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// screenArgs blocks dangerous argument shapes even for allow-listed tools:
// interpreter escapes (python -c), remote-mutating or history-rewriting git,
// and container escapes.
func screenArgs(bin string, args []string) string {
	switch bin {
	case "python", "python3":
		for _, a := range args {
			if a == "-c" || a == "-m" {
				return "python " + a + " executes arbitrary code; run a project script by path instead"
			}
		}
	case "git":
		if len(args) > 0 {
			switch args[0] {
			case "push":
				return "git push is blocked; push manually after reviewing the changes"
			case "clean":
				return "git clean is blocked; it deletes untracked files"
			case "reset":
				for _, a := range args[1:] {
					if a == "--hard" {
						return "git reset --hard is blocked; it discards local changes"
					}
				}
			}
		}
	case "docker":
		if len(args) > 0 && (args[0] == "run" || args[0] == "exec") {
			return "docker " + args[0] + " can execute arbitrary code on the host; use build/compose workflows instead"
		}
	case "npm", "yarn", "pnpm":
		if len(args) > 0 && (args[0] == "exec" || args[0] == "x" || args[0] == "dlx") {
			return bin + " " + args[0] + " launches arbitrary executables; call the underlying tool directly"
		}
	}
	return ""
}

// SanitizeCommand parses a command line into an argv vector and validates it
// against the hybrid allow-list. It returns the exact vector to execute (no
// shell involved) or a human-readable refusal reason. Exported so the TUI and
// the team pipeline can pre-screen a command and show the user the sanitized
// argv they are approving.
func SanitizeCommand(command string) ([]string, string) {
	argv, err := parseCommandLine(command)
	if err != nil {
		return nil, err.Error()
	}
	// Explicitly reject path-qualified command names (./go, ../../evil,
	// /usr/bin/python). normalizeBinName would silently strip these to the
	// base name and the allow-list would pass them — the user would then
	// see an unexpected binary running instead of a clear refusal.
	raw0 := argv[0]
	if strings.ContainsAny(raw0, "/\\") || strings.HasPrefix(raw0, ".") {
		return nil, "command must be a bare name (no path separators or relative prefix): " + raw0
	}
	bin := normalizeBinName(argv[0])
	if blockedInterpreters[bin] {
		return nil, "«" + bin + "» is an interpreter/shell/network tool and is blocked; only allow-listed build tools may run"
	}
	if !allowedCommands[bin] {
		return nil, "«" + bin + "» is not on the allowed tool list (" + allowedCommandList() + ")"
	}
	if reason := screenArgs(bin, argv[1:]); reason != "" {
		return nil, reason
	}
	// Execute the normalized name via PATH so a look-alike binary smuggled in
	// by path (./git, ../../git.exe) is never launched.
	argv[0] = bin
	return argv, ""
}

// SanitizedCommandLine renders the exact argv that will be executed, for the
// approval prompt. Empty when the command is refused.
func SanitizedCommandLine(command string) string {
	argv, reason := SanitizeCommand(command)
	if reason != "" {
		return ""
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t") {
			quoted[i] = `"` + a + `"`
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}

// ScreenCommand reports why a command is refused, or "" if it passes the
// hybrid allow-list. It is exported so callers (and tests) can pre-screen a
// command before prompting the user for approval.
func ScreenCommand(command string) string {
	_, reason := SanitizeCommand(command)
	return reason
}

// runCommand executes an allow-listed command with a timeout and capped
// output. It is gated behind explicit user approval AND the hybrid allow-list
// (see SanitizeCommand): the parsed argv is executed directly — the model's
// text is NEVER handed to a shell, so pipelines, substitution and redirection
// cannot occur. NOTE: cmd.Dir only sets the working directory — it is NOT a
// sandbox. The command runs with the full privileges of the user running the
// CLI, which is exactly why approval stays mandatory on top of the allow-list.
func (r *Runner) runCommand(argsJSON string) (string, bool) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Command) == "" {
		return "error: command argument required", false
	}

	// An isolated worktree cannot contain a command's effects. The overlay is a
	// copy-on-write view, so a build or an install run inside it would either fail
	// on the files it cannot see or write outside the isolation entirely, and either
	// way its effects would not appear in Changes() and would escape both the undo
	// stack and integration. Widening the checkpoint boundary to command effects is
	// roadmap-v3 §4.2 (C-2); until then a writer in an isolated tree does not get a
	// shell. No role is affected today — only the tester holds allowCommands, and it
	// runs sequentially in the real tree.
	if r.base != "" {
		return "error: commands are unavailable in an isolated worktree; " +
			"the integration step runs the build and tests once, in the project tree", false
	}

	// Defence in depth: even an approved command is re-validated, so a single
	// careless "yes" cannot wipe the machine.
	argv, reason := SanitizeCommand(args.Command)
	if reason != "" {
		return "command rejected by security policy: " + reason, false
	}

	// Выполняем pre-commit hook если это git commit
	if ok, err := r.runPreCommitHook(args.Command); !ok {
		if err != nil {
			return "pre-commit hook failed: " + err.Error(), false
		}
		return "pre-commit hook blocked the command", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	// #nosec G204 -- running user-approved commands is this tool's purpose. The
	// argv slice is produced by SanitizeCommand above (no shell), and approval
	// is re-validated on every call.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.root // working directory only — NOT an isolation boundary

	out, err := cmd.CombinedOutput()
	return finishedCommandResult(string(out), err, ctx.Err()), false
}

// finishedCommandResult turns what exec produced into runCommand's return value.
//
// Split from runCommand so every outcome is reachable from a test. The timeout
// branch in particular is not: commandTimeout is 120 s, so a test driving a real
// process cannot reach it, and it is the branch that used to return a status line
// with no separator — silently dropping the output of a command killed at the
// deadline, which is exactly the case where what it printed before hanging is the
// useful part.
func finishedCommandResult(output string, runErr, ctxErr error) string {
	if len(output) > maxCmdOutput {
		output = output[:maxCmdOutput] + "\n…[output truncated]"
	}
	status := CommandOKPrefix
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		status = "command timed out"
	case runErr != nil:
		status = "failed: " + runErr.Error()
	}
	return commandResult(status, output)
}

// commandResult composes runCommand's return value: a status line, the separator,
// then the command's own output.
//
// Every outcome that actually started a process goes through here, timeouts
// included. That matters because agentcore.commandOutputOf keys on the separator to
// find the literal output the evidence panel shows: a status line emitted without
// it silently drops the output, and a command killed at the deadline is precisely
// the case where what it printed before hanging is the useful part.
//
// A separate function because the timeout branch is otherwise unreachable from a
// test — commandTimeout is 120s — so the invariant would rest on inspection.
//
// The output is also forced to valid UTF-8 here. A process writes bytes, not runes:
// a program in a cp1251 console, or one whose output was cut at maxCmdOutput
// mid-rune, produces bytes that are not valid UTF-8. Both destinations of this
// text reject that — encoding/json refuses to marshal it, and Postgres refuses it
// in a text column — so an invalid byte would lose the whole evidence row instead
// of one glyph.
func commandResult(status, output string) string {
	output = strings.ToValidUTF8(output, "�")
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	}
	return status + "\n" + CommandOutputSeparator + "\n" + output
}

// searchArgs / searchCode: a grep-like search over the project, the practical
// retrieval mechanism (smart context) — the model finds relevant code without
// reading whole files.
func (r *Runner) searchCode(argsJSON string) string {
	var args struct {
		Query string `json:"query"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Query) == "" {
		return "error: query argument required"
	}
	// An isolated writer searches its overlay and then the base tree, so it finds
	// both its own new files and the rest of the project. Ordinary Runners and any
	// explicitly scoped search walk exactly one root, as before.
	roots := r.searchRoots(args.Path)
	if roots == nil {
		root := r.root
		if args.Path != "" {
			abs, err := r.resolve(args.Path)
			if err != nil {
				return "error: " + err.Error()
			}
			root = abs
		}
		roots = []string{root}
	}

	const maxHits = 60
	var hits []string
	q := strings.ToLower(args.Query)
	// Paths already reported from an earlier root. The overlay is walked first, so
	// a file the writer changed is reported from its own copy and the base tree's
	// stale version of the same path is skipped rather than shown alongside it.
	reported := map[string]bool{}
	var err error
	for _, root := range roots {
		walkRoot := root
		err = filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if ignoredDirs[d.Name()] {
					return filepath.SkipDir
				}
				// The isolation directory holds other writers' overlays and this
				// writer's checkpoints; neither is part of the project being searched.
				if d.Name() == ".infinity" {
					return filepath.SkipDir
				}
				if p != walkRoot && isWalkEntryLinkOrReparse(d) {
					return filepath.SkipDir
				}
				return nil
			}
			if isWalkEntryLinkOrReparse(d) {
				return nil
			}
			if len(hits) >= maxHits {
				return filepath.SkipAll
			}
			// #nosec G122,G304 -- symlinks and reparse points are skipped above for
			// both directories and files, so the walk cannot be redirected out of
			// root, and p is therefore always a real path under it.
			data, readErr := os.ReadFile(p)
			if readErr != nil || len(data) > maxReadBytes || !looksTextual(data) {
				return nil
			}
			rel, _ := filepath.Rel(walkRoot, p)
			rel = filepath.ToSlash(rel)
			if reported[rel] {
				return nil
			}
			reported[rel] = true
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(strings.ToLower(line), q) {
					trimmed := strings.TrimSpace(line)
					if len(trimmed) > 200 {
						trimmed = trimmed[:200] + "…"
					}
					hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, trimmed))
					if len(hits) >= maxHits {
						break
					}
				}
			}
			return nil
		})
		if err != nil {
			break
		}
	}
	if err != nil {
		return "search error: " + err.Error()
	}
	if len(hits) == 0 {
		return "no matches for query: " + args.Query
	}
	return strings.Join(hits, "\n")
}

func isWalkEntryLinkOrReparse(d os.DirEntry) bool {
	info, err := d.Info()
	if err != nil {
		return true
	}
	return isSymlinkOrReparse(info)
}

// remember stores one curated fact into local project memory (.infinity/).
func (r *Runner) remember(argsJSON string) string {
	var args struct {
		Category string `json:"category"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Note) == "" {
		return "error: category and note required"
	}
	if r.mem == nil {
		return "error: memory unavailable"
	}
	return r.mem.Remember(args.Category, args.Note)
}

// recall reads local project memory (a category, or the whole digest).
func (r *Runner) recall(argsJSON string) string {
	var args struct {
		Category string `json:"category"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	if r.mem == nil {
		return "error: memory unavailable"
	}
	return r.mem.Recall(args.Category)
}

// looksTextual rejects binary files (presence of a NUL byte in the head).
func looksTextual(data []byte) bool {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	for _, b := range data[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// runPostDiffHook вызывает post-diff hook после изменения файла.
// Выполняется асинхронно и не блокирует основную операцию.
func (r *Runner) runPostDiffHook(filePath, oldContent, newContent string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	paths, err := hooks.DefaultSearchPaths()
	if err != nil {
		return
	}

	mgr := hooks.New(paths)
	mgr.SetTimeout(5 * time.Second)
	_, _ = mgr.Execute(ctx, hooks.Event{
		Point: hooks.PostDiff,
		Data: map[string]string{
			"file": filePath,
		},
		Env: map[string]string{
			"OLD_CONTENT": oldContent,
			"NEW_CONTENT": newContent,
		},
	})
}

// runPreCommitHook вызывает pre-commit hook перед git commit.
// Возвращает true если разрешено продолжать, false если заблокировано.
func (r *Runner) runPreCommitHook(command string) (bool, error) {
	// Проверяем что это git commit команда
	if !isGitCommitCommand(command) {
		return true, nil
	}

	paths, err := hooks.DefaultSearchPaths()
	if err != nil {
		return true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := hooks.New(paths)
	mgr.SetTimeout(30 * time.Second)
	result, err := mgr.Execute(ctx, hooks.Event{
		Point: hooks.PreCommit,
		Data: map[string]string{
			"command": command,
		},
		Env: map[string]string{
			"CWD": r.root,
		},
	})

	if err != nil {
		return false, err
	}

	if !result.Executed {
		return true, nil
	}

	// Non-zero exit блокирует commit
	return result.ExitCode == 0, nil
}

// isGitCommitCommand проверяет, является ли команда git commit.
func isGitCommitCommand(command string) bool {
	trimmed := strings.TrimSpace(command)
	if len(trimmed) < 10 {
		return false
	}
	return strings.HasPrefix(trimmed, "git commit")
}
