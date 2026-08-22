package tools

import (
	"encoding/json"
	"sort"
	"sync"
)

// Origin says who authored a tool, which is an authorization-relevant fact and
// not a label: a built-in is code we wrote and reviewed, an MCP tool is a name
// and a description supplied at runtime by a third-party server. The MCP
// specification calls tool descriptions and annotations untrusted unless the
// server itself is trusted, so nothing a server says about its own tool may
// relax policy (.ai/RULES.md:22 — untrusted text never feeds an authorization
// decision).
type Origin int

const (
	OriginBuiltin Origin = iota
	OriginMCP
)

// Spec describes one tool: how it is presented to the model and what may be
// done with it. Policy lives here, as data, instead of being a property of the
// name — that is the whole point. RequiresApproval used to be a pure function
// closed over eight constants, so every name it had never heard of (i.e. every
// MCP tool) came back "no approval needed".
type Spec struct {
	Name        string
	Description string
	Schema      map[string]any

	// Mutating drives the ✎ glyph in the UI only. For MCP tools it stays false:
	// we genuinely do not know whether a remote tool writes, and guessing from a
	// server-supplied description would be exactly the mistake described above.
	Mutating bool

	// Approval means a human must confirm before the call runs. Always true for
	// OriginMCP, with no config key to turn it off (roadmap §4.8 forbids
	// auto-approving writing tools; since a client cannot tell whether a remote
	// tool writes, the safe reading is "all of them").
	Approval bool

	Origin Origin
	Server string // non-empty only for OriginMCP
}

// builtins is the single source of truth for the eight local tools. Definitions()
// walks this slice in order, so the JSON sent to the model is byte-identical to
// what the hand-written literal produced (pinned by
// TestCharacterizationDefinitionsAreByteStable).
var builtins = []Spec{
	{
		Name:        ToolReadFile,
		Description: "Read a relevant project file before editing.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		},
	},
	{
		Name:        ToolListDir,
		Description: "Inspect project tree; empty path is root.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	},
	{
		Name:        ToolPatchFile,
		Description: "Apply a minimal targeted replacement.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string"},
				"old_content": map[string]any{"type": "string"},
				"new_content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "old_content", "new_content"},
		},
		Mutating: true,
		Approval: true,
	},
	{
		Name:        ToolWriteFile,
		Description: "Create a new file only; use patch_file for existing files.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		Mutating: true,
		Approval: true,
	},
	{
		Name:        ToolSearchCode,
		Description: "Find symbols/errors before guessing; returns file:line hits.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"path":  map[string]any{"type": "string"},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        ToolRunCommand,
		Description: "Shell for focused facts, search, build/test/lint, formatting.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
		// Not Mutating: run_command does not go through the undo stack, so the ✎
		// glyph would promise a rollback that does not exist. It still needs
		// approval — unconditionally, even in ask-mode-off (see toolloop.go).
		Approval: true,
	},
	{
		Name:        ToolRemember,
		Description: "Save one durable project fact.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category": map[string]any{"type": "string"},
				"note":     map[string]any{"type": "string"},
			},
			"required": []string{"category", "note"},
		},
	},
	{
		Name:        ToolRecall,
		Description: "Read project memory; category optional.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"category": map[string]any{"type": "string"}},
		},
	},
}

// registry holds the built-ins plus whatever MCP tools the current session
// registered. It is package-level because the five policy functions
// (Definitions/Summary/Mutating/RequiresApproval/Preview) are called from the UI
// goroutine, the team engine's worker goroutines and the tool loop, and changing
// their signatures would touch a dozen call sites for no gain.
//
// The mutex is not decoration: RegisterMCP runs during MCP server startup while
// the TUI may already be rendering, and team mode reads definitions from worker
// goroutines (the same C4 data-race class already fixed on Runner.undo).
var registry = struct {
	sync.RWMutex
	mcp map[string]Spec // key: model-visible name, i.e. mcp__<server>__<tool>
}{mcp: map[string]Spec{}}

// lookup returns the spec for a model-visible tool name.
func lookup(name string) (Spec, bool) {
	for i := range builtins {
		if builtins[i].Name == name {
			return builtins[i], true
		}
	}
	registry.RLock()
	defer registry.RUnlock()
	s, ok := registry.mcp[name]
	return s, ok
}

// RegisterMCP replaces the set of MCP tools offered to the model. Passing an
// empty slice unregisters everything, which is what happens when no server is
// enabled.
//
// Approval is forced on here rather than trusted from the caller: this is the
// one place every MCP tool must pass through, so making it the enforcement point
// means a future caller cannot forget. Names that collide with a built-in are
// rejected — a server advertising "read_file" must not be able to shadow a tool
// whose policy the user already trusts.
func RegisterMCP(specs []Spec) {
	next := make(map[string]Spec, len(specs))
	for _, s := range specs {
		if s.Name == "" || isBuiltin(s.Name) {
			continue
		}
		s.Origin = OriginMCP
		s.Approval = true
		s.Mutating = false
		next[s.Name] = s
	}
	registry.Lock()
	registry.mcp = next
	registry.Unlock()
}

// isBuiltin reports whether name is one of the eight local tools.
func isBuiltin(name string) bool {
	for i := range builtins {
		if builtins[i].Name == name {
			return true
		}
	}
	return false
}

// MCPSpecs returns the registered MCP tools sorted by name, for display.
func MCPSpecs() []Spec {
	registry.RLock()
	out := make([]Spec, 0, len(registry.mcp))
	for _, s := range registry.mcp {
		out = append(out, s)
	}
	registry.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MCPSpec returns the registered spec for one model-visible MCP name. The
// dispatcher resolves through this rather than by splitting the name on "__",
// because the registry holds only what the allow-list admitted: a well-formed
// name for a tool that was never allowed has no entry, and parsing would have
// produced a callable server and tool from it.
func MCPSpec(name string) (Spec, bool) {
	s, ok := lookup(name)
	if !ok || s.Origin != OriginMCP {
		return Spec{}, false
	}
	return s, true
}

// Known reports whether the name is a tool the CLI is prepared to execute:
// a built-in, or an MCP tool currently registered and allow-listed. Dispatch
// uses this to refuse names the model invented or that belong to a server the
// user did not enable.
func Known(name string) bool {
	_, ok := lookup(name)
	return ok
}

// ServerOf returns the MCP server a tool belongs to, or "" for built-ins.
func ServerOf(name string) string {
	s, ok := lookup(name)
	if !ok || s.Origin != OriginMCP {
		return ""
	}
	return s.Server
}

// MCPPrefix and mcpSep build the model-visible name mcp__<server>__<tool>.
//
// "__" is a safe separator, not a guess: the MCP specification limits tool names
// to letters, digits, "_", "-" and ".", 1–128 characters, so a double underscore
// cannot occur inside a server or tool name and the split is unambiguous. Tool
// names are unique only within a server, and the specification explicitly tells
// aggregating clients to implement their own disambiguation rather than relying
// on serverInfo.name.
const (
	MCPPrefix = "mcp__"
	mcpSep    = "__"
)

// MCPName builds the model-visible name for a tool on a server.
func MCPName(server, tool string) string {
	return MCPPrefix + server + mcpSep + tool
}

// Tool returns the server-side tool name for an MCP spec (the model-visible name
// minus the mcp__<server>__ prefix), or Name for a built-in.
func (s Spec) Tool() string {
	if s.Origin != OriginMCP {
		return s.Name
	}
	prefix := MCPPrefix + s.Server + mcpSep
	if len(s.Name) > len(prefix) && s.Name[:len(prefix)] == prefix {
		return s.Name[len(prefix):]
	}
	return s.Name
}

// IsMCPName reports whether a name is in the MCP namespace. Used to reject a
// model-invented mcp__* name before it reaches a server, and to keep built-ins
// out of that namespace.
func IsMCPName(name string) bool {
	return len(name) > len(MCPPrefix) && name[:len(MCPPrefix)] == MCPPrefix
}

// ValidMCPIdent reports whether s is usable as a server or tool name: the
// character set from the MCP specification, 1–128 characters, and no "__" so the
// namespace separator stays unambiguous. Rejecting is the right response to an
// odd name — a server that wants its tools used can name them legally.
func ValidMCPIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.'
		if !ok {
			return false
		}
		if c == '_' && i+1 < len(s) && s[i+1] == '_' {
			return false
		}
	}
	return true
}

// definitionsFrom renders specs as OpenAI-style function definitions.
func definitionsFrom(specs []Spec) json.RawMessage {
	defs := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		defs = append(defs, funcDef(s.Name, s.Description, s.Schema))
	}
	b, _ := json.Marshal(defs)
	return b
}
