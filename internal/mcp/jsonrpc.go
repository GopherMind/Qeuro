// Package mcp implements a Model Context Protocol client (and, in serve.go, a
// server exposing this product's own tools). Only the standard library is used.
//
// The protocol revision is 2026-07-28, which is not the shape most MCP material
// describes. Two differences drive the design here:
//
//   - There is no initialize handshake. The protocol is stateless, and every
//     request carries the protocol version and the client's capabilities in
//     _meta. Omitting a required _meta field is an InvalidParams error.
//   - server/discover replaces the handshake. Servers MUST implement it. A
//     client that only speaks modern revisions still sends it — not to learn the
//     version, but to fail deterministically against a legacy server. Some
//     legacy servers do not check that a request arrived after initialize and
//     would happily execute tools/call under legacy semantics.
//
// Everything a server sends back — tool names, descriptions, schemas, result
// content — is untrusted data (.ai/SECURITY.md:33). This package validates
// structure and hands text to the caller for fencing; it never lets server text
// influence policy.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the revision this client speaks.
const ProtocolVersion = "2026-07-28"

// _meta keys are namespaced by the specification; the io.modelcontextprotocol/
// prefix is reserved for it.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// Method names used by this client and server.
const (
	MethodDiscover  = "server/discover"
	MethodToolsList = "tools/list"
	MethodToolsCall = "tools/call"
	MethodCancelled = "notifications/cancelled"
)

// JSON-RPC and MCP error codes. The three MCP-specific ones matter to the
// client: version mismatch is recoverable by retrying with a supported version,
// the other two are configuration faults worth naming precisely in the message.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeUnsupportedProtocolVersion = -32022
	CodeHeaderMismatch             = -32020
	CodeMissingClientCapability    = -32021
)

// request is a JSON-RPC 2.0 request or notification. ID is nil for
// notifications, which by specification must not be answered.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is a JSON-RPC 2.0 response. Exactly one of Result and Error is
// meaningful; a server that sets both is malformed, and readResponse rejects it
// rather than picking one.
type response struct {
	JSONRPC string `json:"jsonrpc"`
	// ID is not omitempty: JSON-RPC 2.0 requires an id member on every response,
	// and a parse error — where no id could be read — must still carry an explicit
	// null. Omitting the member instead makes a client that keys replies by id
	// unable to tell a malformed response from a reply to something else.
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements error. The server's message is included because it is the
// only diagnostic available, but callers must treat it as untrusted text: it is
// shown to the user as a quoted server message, never used as an instruction.
func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp: server error %d: %s", e.Code, e.Message)
}

// supportedVersions extracts data.supported from an UnsupportedProtocolVersion
// error, which the client uses for its single retry.
func (e *rpcError) supportedVersions() []string {
	if e.Code != CodeUnsupportedProtocolVersion || len(e.Data) == 0 {
		return nil
	}
	var d struct {
		Supported []string `json:"supported"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return nil
	}
	return d.Supported
}

// clientMeta builds the _meta object required on every request.
//
// Capabilities are declared honestly and minimally: this client does not offer
// sampling, roots or elicitation, so it claims none. Claiming a capability we do
// not implement would invite the server to use it and hang the call.
func clientMeta() map[string]any {
	return map[string]any{
		metaProtocolVersion:    ProtocolVersion,
		metaClientCapabilities: map[string]any{},
		metaClientInfo: map[string]any{
			"name":    "qeuro-cli",
			"version": "1",
		},
	}
}

// withMeta returns params with _meta attached. Params arrive as a typed struct
// from this package, so the round-trip through a map is safe; a caller cannot
// smuggle a conflicting _meta because it is set last.
func withMeta(params any) (json.RawMessage, error) {
	var m map[string]any
	if params == nil {
		m = map[string]any{}
	} else {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
	}
	m["_meta"] = clientMeta()
	return json.Marshal(m)
}

// ---- typed payloads ------------------------------------------------------

// DiscoverResult is the server/discover response.
type DiscoverResult struct {
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      map[string]any `json:"capabilities"`
	Instructions      string         `json:"instructions"`
	TTLMs             int64          `json:"ttlMs"`
	CacheScope        string         `json:"cacheScope"`
	Meta              struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"io.modelcontextprotocol/serverInfo"`
	} `json:"_meta"`
}

// Tool is one tool as advertised by a server. Description, Title and Annotations
// are author-supplied text from a third party: untrusted, never shown where the
// user would read it as ours, never consulted for policy.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// ToolsListResult is the tools/list response. Cursor drives pagination.
type ToolsListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallResult is the tools/call response.
//
// IsError is not a transport failure: the specification models tool execution
// errors inside the result so the text can be handed to the model for
// self-correction. Only protocol-level failures come back as JSON-RPC errors.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	ResultType        string          `json:"resultType,omitempty"`
}

// Content is one item of a tool result. Only text is forwarded to the model;
// see Text() for why the others are named but not passed through.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}
