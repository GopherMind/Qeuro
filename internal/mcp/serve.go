package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Serve runs this CLI as an MCP server over stdio, exposing a small set of
// Qeuro's own capabilities to another agent (roadmap §4.8: "и наш сервер").
//
// Two framing rules from the specification govern everything here:
//
//   - stdout carries MCP messages and nothing else. Every diagnostic goes to the
//     writer passed as logw (stderr in production). A stray Println on stdout
//     would corrupt the stream for the client, which is why nothing in this file
//     writes to os.Stdout directly.
//   - the transport is newline-delimited JSON, and json.Marshal escapes newlines
//     inside strings, so one message is always one line by construction.
//
// The revision is stateless: there is no initialize handshake to wait for, so a
// tools/call may legitimately be the first message received.
func Serve(ctx context.Context, in io.Reader, out io.Writer, logw io.Writer) int {
	s := &server{out: out, log: logw}
	fmt.Fprintf(logw, "qeuro mcp serve: protocol %s, %d tools, reading stdin\n", ProtocolVersion, len(serverTools()))

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		s.handleLine(ctx, line)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(logw, "qeuro mcp serve: read error:", err)
		return 1
	}
	return 0
}

// server is one stdio session.
type server struct {
	mu  sync.Mutex // serialises writes: one message per line
	out io.Writer
	log io.Writer
}

// handleLine decodes one request and answers it.
//
// A request that cannot be parsed at all is answered with a null-id parse error,
// which is what the specification prescribes, rather than being ignored: a client
// waiting on a response it will never get is the failure mode this avoids.
func (s *server) handleLine(ctx context.Context, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(nil, CodeParseError, "invalid JSON")
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeError(req.ID, CodeInvalidRequest, "jsonrpc must be \"2.0\"")
		return
	}
	if req.ID == nil {
		// A notification. notifications/cancelled is the only one that matters, and
		// every handler here is fast enough that there is nothing to cancel — but it
		// must not be answered, and it must not be an error.
		return
	}

	// _meta.protocolVersion is required on every request in this revision. It is
	// checked before dispatch so a client that speaks an unsupported revision gets
	// the specified error rather than a tool result computed under assumptions it
	// does not share.
	if code, msg, data := checkRequestMeta(req.Params); code != 0 {
		s.writeErrorData(req.ID, code, msg, data)
		return
	}

	switch req.Method {
	case MethodDiscover:
		s.writeResult(req.ID, discoverPayload())
	case MethodToolsList:
		s.writeResult(req.ID, map[string]any{"tools": serverTools()})
	case MethodToolsCall:
		s.handleCall(ctx, req)
	default:
		s.writeError(req.ID, CodeMethodNotFound, "unknown method")
	}
}

// checkRequestMeta validates the _meta fields the revision requires.
func checkRequestMeta(params json.RawMessage) (int, string, any) {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return CodeInvalidParams, "params must be an object", nil
		}
	}
	raw, ok := p.Meta[metaProtocolVersion]
	if !ok {
		return CodeInvalidParams, "_meta." + metaProtocolVersion + " is required", nil
	}
	var version string
	if err := json.Unmarshal(raw, &version); err != nil {
		return CodeInvalidParams, "_meta." + metaProtocolVersion + " must be a string", nil
	}
	if version != ProtocolVersion {
		return CodeUnsupportedProtocolVersion,
			"this server speaks " + ProtocolVersion,
			map[string]any{"supported": []string{ProtocolVersion}}
	}
	if _, ok := p.Meta[metaClientCapabilities]; !ok {
		return CodeMissingClientCapability, "_meta." + metaClientCapabilities + " is required", nil
	}
	return 0, "", nil
}

// discoverPayload is the server/discover result.
func discoverPayload() map[string]any {
	return map[string]any{
		"supportedVersions": []string{ProtocolVersion},
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"instructions": "Qeuro CLI exposes read-only planning and inspection tools. " +
			"They report on the repository the CLI was started in and on the signed-in account.",
		"_meta": map[string]any{
			metaServerInfo: map[string]any{"name": "qeuro-cli", "version": "1"},
		},
	}
}

// handleCall dispatches tools/call.
func (s *server) handleCall(ctx context.Context, req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, CodeInvalidParams, "params must be an object")
		return
	}
	fn, ok := serverToolFuncs[p.Name]
	if !ok {
		// Unknown tool is a protocol error, not a tool error: there is no tool to
		// report a failure from.
		s.writeError(req.ID, CodeInvalidParams, "unknown tool")
		return
	}

	text, isErr := fn(ctx, p.Arguments)
	if len(text) > maxResultBytes {
		text = truncateUTF8(text, maxResultBytes) + "\n…[truncated]"
	}
	s.writeResult(req.ID, CallResult{
		Content: []Content{{Type: "text", Text: text}},
		IsError: isErr,
	})
}

// ---- writing -------------------------------------------------------------

func (s *server) writeResult(id *int64, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.writeError(id, CodeInternalError, "cannot encode result")
		return
	}
	s.write(response{JSONRPC: "2.0", ID: id, Result: raw})
}

func (s *server) writeError(id *int64, code int, msg string) {
	s.writeErrorData(id, code, msg, nil)
}

func (s *server) writeErrorData(id *int64, code int, msg string, data any) {
	e := &rpcError{Code: code, Message: msg}
	if data != nil {
		if raw, err := json.Marshal(data); err == nil {
			e.Data = raw
		}
	}
	s.write(response{JSONRPC: "2.0", ID: id, Error: e})
}

func (s *server) write(resp response) {
	b, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintln(s.log, "qeuro mcp serve: cannot encode response:", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.out.Write(append(b, '\n')); err != nil {
		fmt.Fprintln(s.log, "qeuro mcp serve: write failed:", err)
	}
}

// ---- the tools -----------------------------------------------------------

// serverToolFunc computes one tool's result. The bool is the specification's
// isError: an execution failure the calling model may correct, as opposed to a
// protocol error.
type serverToolFunc func(ctx context.Context, args json.RawMessage) (string, bool)

// serverTools returns the advertised tool list, in a stable order.
//
// All four are read-only. That is a deliberate limit rather than a first
// instalment: this server hands capabilities to an agent we did not write and
// cannot see the approval decisions of, so a writing tool here would be an
// unattended write path into the user's repository — exactly what
// .ai/RULES.md:24 forbids.
func serverTools() []Tool {
	names := make([]string, 0, len(serverToolSpecs))
	for name := range serverToolSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		out = append(out, serverToolSpecs[name])
	}
	return out
}

var serverToolSpecs = map[string]Tool{
	"qeuro.plan": {
		Name:        "qeuro.plan",
		Title:       "Describe the repository",
		Description: "Report the working directory and the project layout Qeuro would plan against: top-level entries, detected build files and the git branch. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	"qeuro.diff": {
		Name:        "qeuro.diff",
		Title:       "Uncommitted changes",
		Description: "Report the names and change status of files modified in the working tree, as `git status --porcelain` would. Contents are not included. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	"qeuro.cost": {
		Name:        "qeuro.cost",
		Title:       "Account balance",
		Description: "Report the signed-in plan and the remaining credit balance. Requires a saved token. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	"qeuro.run_task": {
		Name:        "qeuro.run_task",
		Title:       "Run an agent task",
		Description: "Run a Qeuro agent task in this repository. NOT AVAILABLE: the headless agent loop is not implemented, so this always fails. It is advertised so a caller discovers the contract rather than the absence.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string"}},"required":["task"],"additionalProperties":false}`),
	},
}
