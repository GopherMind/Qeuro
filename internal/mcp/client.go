package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Timeouts and limits. A third-party process gets a bounded share of the user's
// time and context; .ai/RULES.md:40 requires cancellation, timeout, size limit
// and failure classification for any network or long-running work.
const (
	// discoverTimeout bounds startup. A server that cannot answer server/discover
	// this quickly is broken, and startup must not block the CLI.
	discoverTimeout = 10 * time.Second

	// listTimeout bounds tool discovery.
	listTimeout = 15 * time.Second

	// DefaultCallTimeout bounds one tool call. Deliberately shorter than
	// commandTimeout (120s) for local commands: a remote call the user is waiting
	// on with no output is a worse experience than a local build.
	DefaultCallTimeout = 60 * time.Second

	// maxToolsPages bounds pagination so a server cannot hold discovery open
	// forever by always returning a nextCursor.
	maxToolsPages = 20

	// maxToolsPerServer bounds how many tools one server may advertise. The
	// allow-list is what decides which are usable; this only stops discovery from
	// growing without limit.
	maxToolsPerServer = 500
)

// Client is a connection to one MCP server.
type Client struct {
	server    string
	transport Transport

	// callsPerMinute limits how often the model may invoke this server's tools.
	// Zero means unlimited.
	callsPerMinute int

	mu       sync.Mutex
	calls    []time.Time // sliding window of call times
	info     ServerInfo
	discover *DiscoverResult
}

// ServerInfo is what the client learned about a server at connect time.
type ServerInfo struct {
	Name    string
	Version string
	// Instructions is server-authored text. It is retained for display only, and
	// deliberately never injected into a system prompt: the specification calls
	// it a hint for the model, but .ai/RULES.md:22 forbids putting untrusted text
	// into a system instruction, and a server's "instructions" field is the most
	// obvious place to attempt exactly that.
	Instructions string
}

// ErrRateLimited is returned when a server's per-minute call budget is spent.
var ErrRateLimited = errors.New("mcp: per-minute call limit reached for this server")

// Connect probes a server and returns a ready client.
//
// server/discover is sent even though this client speaks one revision. The point
// is deterministic failure against a legacy server: a pre-2026-07-28 server that
// does not implement discover answers MethodNotFound, and we stop there rather
// than proceeding to tools/call, which some legacy servers would execute under
// legacy semantics without noticing the missing initialize handshake.
func Connect(ctx context.Context, server string, transport Transport, callsPerMinute int) (*Client, error) {
	c := &Client{server: server, transport: transport, callsPerMinute: callsPerMinute}

	dctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	raw, err := transport.Call(dctx, MethodDiscover, nil)
	if err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			switch rpcErr.Code {
			case CodeMethodNotFound, CodeInvalidRequest:
				return nil, fmt.Errorf("mcp: server %q does not implement %s, so it predates revision %s; "+
					"this client does not speak the legacy initialize handshake", server, MethodDiscover, ProtocolVersion)
			case CodeUnsupportedProtocolVersion:
				return nil, fmt.Errorf("mcp: server %q supports %v, this client speaks %s",
					server, rpcErr.supportedVersions(), ProtocolVersion)
			case CodeMissingClientCapability:
				return nil, fmt.Errorf("mcp: server %q requires a client capability this CLI does not implement: %s",
					server, rpcErr.Message)
			}
		}
		return nil, fmt.Errorf("mcp: server %q failed to start: %w%s", server, err, diagSuffix(transport))
	}

	var d DiscoverResult
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("mcp: server %q returned an unreadable %s response: %w", server, MethodDiscover, err)
	}
	if len(d.SupportedVersions) > 0 && !contains(d.SupportedVersions, ProtocolVersion) {
		return nil, fmt.Errorf("mcp: server %q supports %v, this client speaks %s",
			server, d.SupportedVersions, ProtocolVersion)
	}
	c.discover = &d
	c.info = ServerInfo{
		Name:         d.Meta.ServerInfo.Name,
		Version:      d.Meta.ServerInfo.Version,
		Instructions: d.Instructions,
	}
	return c, nil
}

// Info returns what was learned at connect time.
func (c *Client) Info() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Server returns the local name the user gave this server in mcp.json. This is
// the name used for namespacing, not serverInfo.name — the specification warns
// that serverInfo.name is not suitable for disambiguation, and a server must not
// be able to choose the prefix under which its tools appear.
func (c *Client) Server() string { return c.server }

// ListTools returns every tool the server advertises, following pagination.
//
// Nothing here is filtered against the allow-list: this is discovery, and the
// user needs to see blocked tools in `qeuro mcp tools` in order to allow them.
// Filtering happens where policy is applied.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	cursor := ""
	seenCursors := map[string]bool{}

	for page := 0; page < maxToolsPages; page++ {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := c.transport.Call(lctx, MethodToolsList, params)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("mcp: %s on server %q: %w%s", MethodToolsList, c.server, err, diagSuffix(c.transport))
		}
		var res ToolsListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp: server %q returned an unreadable %s response: %w", c.server, MethodToolsList, err)
		}
		out = append(out, res.Tools...)
		if len(out) > maxToolsPerServer {
			return out[:maxToolsPerServer], nil
		}
		if res.NextCursor == "" {
			return out, nil
		}
		// A server repeating a cursor would loop forever within the page budget.
		if seenCursors[res.NextCursor] {
			return out, nil
		}
		seenCursors[res.NextCursor] = true
		cursor = res.NextCursor
	}
	return out, nil
}

// CallTool invokes one tool by its server-side name and returns the result.
//
// The returned CallResult may carry IsError: that is a tool execution error, not
// a transport failure, and the specification says to hand its text to the model
// so it can correct itself. Only protocol failures come back as a Go error.
func (c *Client) CallTool(ctx context.Context, tool string, args json.RawMessage) (*CallResult, error) {
	if err := c.reserve(); err != nil {
		return nil, err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	cctx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
	defer cancel()

	raw, err := c.transport.Call(cctx, MethodToolsCall, map[string]any{
		"name":      tool,
		"arguments": args,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp: tool %q on server %q did not answer within %s",
				tool, c.server, DefaultCallTimeout)
		}
		return nil, fmt.Errorf("mcp: tool %q on server %q: %w%s", tool, c.server, err, diagSuffix(c.transport))
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: server %q returned an unreadable result for %q: %w", c.server, tool, err)
	}
	return &res, nil
}

// reserve enforces the per-minute call budget with a sliding window.
func (c *Client) reserve() error {
	if c.callsPerMinute <= 0 {
		return nil
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := now.Add(-time.Minute)
	kept := c.calls[:0]
	for _, t := range c.calls {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.calls = kept
	if len(c.calls) >= c.callsPerMinute {
		return fmt.Errorf("%w (%d per minute)", ErrRateLimited, c.callsPerMinute)
	}
	c.calls = append(c.calls, now)
	return nil
}

// Close shuts the server down.
func (c *Client) Close() error { return c.transport.Close() }

// Diagnostics returns the server's own log output, for error messages.
func (c *Client) Diagnostics() string { return c.transport.Diagnostics() }

// diagSuffix appends the server's stderr to an error message when there is any.
// A server that fails to start usually explains why on stderr, and without this
// the user sees only "EOF".
func diagSuffix(t Transport) string {
	d := t.Diagnostics()
	if d == "" {
		return ""
	}
	return "\nserver log: " + sanitizeOneLine(firstLines(d, 5))
}

// firstLines returns at most n lines of s.
func firstLines(s string, n int) string {
	out := ""
	count := 0
	for _, line := range splitLines(s) {
		if count >= n {
			break
		}
		if out != "" {
			out += " | "
		}
		out += line
		count++
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
