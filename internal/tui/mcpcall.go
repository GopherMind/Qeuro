package tui

import (
	"context"
	"encoding/json"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/mcp"
	"qeuro/internal/tools"
)

// maxMCPUntrustedChars bounds one fenced MCP payload.
//
// The number is not about cost — mcp.Text() already caps a result at 16 KiB —
// it is about the fence surviving the trimmer. client.TrimMessages clips old
// user messages at maxOldUserContentChars (6000) and appends a truncation
// marker, which would cut the closing fence line off the end. An unclosed fence
// is worse than a short result: everything after it, including our own later
// instructions, would read as being inside the untrusted block. Capping below
// the trimmer's threshold means the block is either sent whole or dropped whole.
const maxMCPUntrustedChars = 4000

// mcpCaller is the part of *mcp.Manager the tool loop uses.
//
// It is an interface for one reason: the code that decides where a server's text
// goes — the tool-role note versus the fenced user message — is the security
// boundary of this file, and testing it must not require spawning a child
// process. A fake caller returning hostile text exercises exactly the path a
// real server drives.
type mcpCaller interface {
	Call(ctx context.Context, name string, args json.RawMessage) (*mcp.CallResult, error)
	Close()
}

// mcpReadyMsg carries the outcome of starting the configured MCP servers.
type mcpReadyMsg struct {
	mgr      *mcp.Manager
	warnings []string
	err      error
}

// startMCPCmd connects the configured MCP servers off the UI goroutine.
//
// It runs on its own background context rather than a turn context: the servers
// outlive any single turn, and cancelling a turn must not tear down the child
// processes. Startup failures are surfaced as a notice, never as a fatal error —
// a broken mcp.json must not stop the CLI from working without MCP.
func startMCPCmd() tea.Cmd {
	return func() tea.Msg {
		mgr, warnings, err := mcp.Start(context.Background())
		return mcpReadyMsg{mgr: mgr, warnings: warnings, err: err}
	}
}

// onMCPReady installs the manager and reports what went wrong, if anything.
func (m model) onMCPReady(msg mcpReadyMsg) (tea.Model, tea.Cmd) {
	// A typed nil *mcp.Manager in an interface field is non-nil, and every guard
	// in this file tests the interface for nil. Assigning only when the pointer is
	// real keeps "no manager" a single condition instead of two.
	if msg.err == nil && msg.mgr != nil {
		m.mcp = msg.mgr
	}
	if msg.err != nil {
		// Only the config itself can fail this way (unreadable, invalid JSON, over
		// a limit). Say so: the alternative is a user who wrote an mcp.json and
		// never learns why no tool appeared.
		m.notice = "mcp: " + clientcfg.DisplaySafe(msg.err.Error())
		return m, nil
	}
	if len(msg.warnings) > 0 {
		// One line, like the config warnings: `qeuro mcp list` prints them all.
		m.notice = clientcfg.DisplaySafe(msg.warnings[0]) + "  (qeuro mcp list)"
		return m, nil
	}
	if n := len(tools.MCPSpecs()); n > 0 {
		m.notice = "mcp: " + itoa(n) + " external tool(s) available — each call asks for approval"
		// A tool the description budget dropped is allow-listed and connected, and
		// `qeuro mcp tools` shows it as usable, but the model is never offered it.
		// Without this the symptom is "the model ignores that tool" with nothing to
		// go on, which is the same silent gap the allow-list warnings exist to close.
		if _, dropped := tools.WithMCP(tools.DefaultMCPDescriptionBudget); dropped > 0 {
			m.notice += "; " + itoa(dropped) +
				" not offered — their descriptions exceed the budget (trim allowTools)"
		}
	}
	return m, nil
}

// closeMCP shuts the servers down. Called once, after the program loop exits: a
// server is a child process, and leaving it running after the CLI is gone would
// leak a process holding whatever token was passed to it.
func (m model) closeMCP() {
	if m.mcp != nil {
		m.mcp.Close()
	}
}

// execMCPCmd runs one approved MCP tool call off the UI goroutine.
//
// Two rules are enforced here and are the reason this is not part of
// execToolCmd:
//
//   - the tool-role message contains ONLY text written by this CLI. Everything
//     the server produced — content, error text, stderr quoted into an error —
//     goes into a separate fenced user message. The tool-role result is
//     summarised into WORKING STATE, which is sent in the *system* role
//     (update.go), and .ai/SECURITY.md:33 forbids untrusted text there.
//   - the call is resolved through the policy registry by Manager.Call, so a
//     well-formed name for a tool that was never allow-listed cannot reach a
//     server.
func execMCPCmd(ctx context.Context, mgr mcpCaller, c client.ToolCall) tea.Cmd {
	name := c.Function.Name
	args := json.RawMessage(c.Function.Arguments)
	return func() tea.Msg {
		server := tools.ServerOf(name)
		if server == "" {
			// Not registered: either the model invented the name or the server is
			// gone. Refusing is the whole point of resolving through the registry.
			// The name is model output, so it is display-sanitised before being
			// echoed into a message that reaches the terminal (.ai/RULES.md:22).
			return toolDoneMsg{call: c, result: "error: " + clientcfg.DisplaySafe(name) +
				" is not available in this session (not configured, not allow-listed, or its server is not running)"}
		}
		if mgr == nil {
			return toolDoneMsg{call: c, result: "error: no MCP server is connected in this session"}
		}
		if ctx == nil {
			ctx = context.Background()
		}

		res, err := mgr.Call(ctx, name, args)
		if err != nil {
			return toolDoneMsg{
				call:      c,
				result:    tools.ToolResultNote(server) + " The call did not complete; the failure is described in that block.",
				untrusted: fenceMCPPayload(server, err.Error()),
			}
		}

		text, _ := res.Text()
		note := tools.ToolResultNote(server)
		if res.IsError {
			// An execution error is a result, not a transport failure: the
			// specification has it handed to the model so it can correct the call.
			note += " The tool reported an execution error; its message is in that block."
		}
		return toolDoneMsg{call: c, result: note, untrusted: fenceMCPPayload(server, text)}
	}
}

// fenceMCPPayload wraps server output as an untrusted-data user message.
func fenceMCPPayload(server, body string) string {
	if len(body) > maxMCPUntrustedChars {
		body = truncateChars(body, maxMCPUntrustedChars) + "\n[truncated by the client]"
	}
	return tools.FenceUntrusted(tools.MCPSource(server), tools.TrustTierMCP, body)
}

// truncateChars cuts s to at most n bytes without leaving a partial rune.
func truncateChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
