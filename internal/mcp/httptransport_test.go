package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The streamable HTTP transport is tested against httptest servers rather than a
// stub http.RoundTripper. The behaviours that matter here — a redirect, a
// half-closed stream, a session the server forgets, a body that never ends — only
// exist at the level of a real server and a real connection, and a fake
// RoundTripper would let each of them pass by construction.

// httpFake is a recording MCP server over streamable HTTP.
type httpFake struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request, req request)
}

type recordedRequest struct {
	Method  string
	Headers http.Header
	Body    string
}

// newHTTPFake starts a TLS-free loopback server. Loopback is what makes plain
// http legal for this transport, so the tests exercise the same path a user with
// a local server would.
func newHTTPFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, req request)) *httpFake {
	t.Helper()
	f := &httpFake{t: t, handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *httpFake) serve(w http.ResponseWriter, r *http.Request) {
	body, _, _ := readBounded(r.Body, 1<<20)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method:  r.Method,
		Headers: r.Header.Clone(),
		Body:    string(body),
	})
	f.mu.Unlock()

	var req request
	_ = json.Unmarshal(body, &req)
	f.handler(w, r, req)
}

func (f *httpFake) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// transportFor builds a transport pointed at the fake, and closes it afterwards.
func transportFor(t *testing.T, url, bearer string) Transport {
	t.Helper()
	tr, err := StartHTTP(HTTPConfig{URL: url, Bearer: bearer})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// writeJSONRPC answers with a single JSON body, which is the shape a server uses
// when it has nothing to stream.
func writeJSONRPC(w http.ResponseWriter, id *int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	raw, _ := json.Marshal(result)
	_ = json.NewEncoder(w).Encode(response{JSONRPC: "2.0", ID: id, Result: raw})
}

// writeSSE answers with an event stream carrying the given payloads in order.
func writeSSE(w http.ResponseWriter, payloads ...any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, p := range payloads {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func toolsListResult(names ...string) map[string]any {
	list := make([]map[string]any, 0, len(names))
	for _, n := range names {
		list = append(list, map[string]any{
			"name":        n,
			"description": "a tool",
			"inputSchema": map[string]any{"type": "object"},
		})
	}
	return map[string]any{"tools": list}
}

func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- URL validation ------------------------------------------------------

// TestHTTPRefusesCleartextToRemoteHost: the token and every tool result would
// travel readable. Loopback is the exception because there is no network hop.
func TestHTTPRefusesCleartextToRemoteHost(t *testing.T) {
	for _, url := range []string{
		"http://mcp.example.com/mcp",
		"http://10.0.0.5:8080/mcp",
		"http://[2001:db8::1]/mcp",
	} {
		if _, err := StartHTTP(HTTPConfig{URL: url}); err == nil {
			t.Errorf("StartHTTP(%q) accepted cleartext to a remote host", url)
		} else if !strings.Contains(err.Error(), "cleartext") {
			t.Errorf("StartHTTP(%q) error = %v, want it to name cleartext", url, err)
		}
	}
}

func TestHTTPAcceptsLoopbackCleartextAndHTTPS(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:9000/mcp",
		"http://localhost:9000/mcp",
		"http://[::1]:9000/mcp",
		"https://mcp.example.com/mcp",
	} {
		tr, err := StartHTTP(HTTPConfig{URL: url})
		if err != nil {
			t.Errorf("StartHTTP(%q): %v", url, err)
			continue
		}
		_ = tr.Close()
	}
}

// TestHTTPRefusesUnusableURLs covers the shapes that would either send a
// credential somewhere it can be read back, or silently drop part of what the
// user wrote.
func TestHTTPRefusesUnusableURLs(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"", "empty"},
		{"ftp://example.com/mcp", "must use https"},
		{"https://user:pass@example.com/mcp", "must not embed credentials"},
		{"https://example.com/mcp#frag", "fragment"},
		{"https://", "no host"},
	} {
		_, err := StartHTTP(HTTPConfig{URL: tc.url})
		if err == nil {
			t.Errorf("StartHTTP(%q) succeeded", tc.url)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("StartHTTP(%q) error = %v, want it to mention %q", tc.url, err, tc.want)
		}
	}
}

// A hostname that merely resolves to loopback is not loopback: resolution can
// change between the check and the connection, so the cleartext exemption must
// be a property of the literal the user wrote.
func TestHTTPLoopbackExemptionIsNotByResolution(t *testing.T) {
	for _, host := range []string{"localhost.evil.example", "127.0.0.1.nip.io", "notlocalhost"} {
		if _, err := StartHTTP(HTTPConfig{URL: "http://" + host + "/mcp"}); err == nil {
			t.Errorf("http://%s/mcp was accepted as loopback", host)
		}
	}
}

// ---- headers and credentials --------------------------------------------

// TestHTTPSendsRequiredHeaders pins the four headers the revision and this
// transport's policy require, and that _meta agrees with the header version.
func TestHTTPSendsRequiredHeaders(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		writeJSONRPC(w, req.ID, toolsListResult("search"))
	})
	tr := transportFor(t, f.srv.URL, "tok-abc")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	rec := f.recorded()
	if len(rec) != 1 {
		t.Fatalf("got %d requests, want 1", len(rec))
	}
	h := rec[0].Headers
	if got := h.Get(protocolHeader); got != ProtocolVersion {
		t.Errorf("%s = %q, want %q", protocolHeader, got, ProtocolVersion)
	}
	if got := h.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", got)
	}
	accept := h.Get("Accept")
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		t.Errorf("Accept = %q, want both response shapes", accept)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}

	// The header and the body must not be able to disagree: a server comparing
	// them answers CodeHeaderMismatch, and this client builds both from one
	// constant. Assert it on the wire rather than trusting that.
	var sent struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(rec[0].Body), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if got := sent.Params.Meta[metaProtocolVersion]; got != ProtocolVersion {
		t.Errorf("_meta protocol version = %v, want %q (must match the header)", got, ProtocolVersion)
	}
}

// An empty token means the header is omitted, not sent empty: a server that
// distinguishes "no credential" from "malformed credential" then behaves
// predictably.
func TestHTTPOmitsAuthorizationWhenNoToken(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := f.recorded()[0].Headers["Authorization"]; ok {
		t.Error("Authorization header present with no token configured")
	}
}

// A variable set to whitespace is the same situation as an unset one, and it
// reaches here as a blank string. "Bearer    " is malformed rather than anonymous,
// and it would also make the 401 message claim a configured token was rejected.
func TestHTTPTreatsABlankTokenAsAbsent(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr := transportFor(t, f.srv.URL, "  \t ")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := f.recorded()[0].Headers["Authorization"]; ok {
		t.Error("a blank token was sent as an Authorization header")
	}
}

// A token with surrounding whitespace — `export TOK="$(cat file)"` picks up a
// trailing newline — is trimmed rather than sent verbatim. An untrimmed value
// with a newline in it would be rejected by net/http when the header is written,
// failing every call with a message about the header rather than the token.
func TestHTTPTrimsTheToken(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr := transportFor(t, f.srv.URL, " tok-abc\n")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := f.recorded()[0].Headers.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want the trimmed token", got)
	}
}

// The HTTP counterpart of BaseEnv: an MCP server reached over the network must
// not receive anything from the CLI's environment beyond the one bearer token the
// user named.
func TestHTTPSendsNoProviderCredentials(t *testing.T) {
	t.Setenv("QEURO_OPENROUTER_KEY", "sk-or-dummy-must-not-leak")
	t.Setenv("QEURO_TOKEN", "qeuro_live_dummy_must_not_leak")

	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr := transportFor(t, f.srv.URL, "server-token")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	rec := f.recorded()[0]
	var dump strings.Builder
	rec.Headers.Write(&dump)
	dump.WriteString(rec.Body)
	for _, secret := range []string{"sk-or-dummy", "qeuro_live_dummy", "QEURO_"} {
		if strings.Contains(dump.String(), secret) {
			t.Errorf("%q reached the HTTP server", secret)
		}
	}
	if !strings.Contains(dump.String(), "server-token") {
		t.Fatal("the configured bearer token was not sent, so the assertions above prove nothing")
	}
}

// ---- response shapes ----------------------------------------------------

// A server may answer either shape for the same method, so both must work
// through the same call path.
func TestHTTPReadsBothResponseShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(w http.ResponseWriter, id *int64)
	}{
		{"json", func(w http.ResponseWriter, id *int64) { writeJSONRPC(w, id, toolsListResult("search")) }},
		{"sse", func(w http.ResponseWriter, id *int64) {
			raw, _ := json.Marshal(toolsListResult("search"))
			writeSSE(w, response{JSONRPC: "2.0", ID: id, Result: raw})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
				tc.write(w, req.ID)
			})
			tr := transportFor(t, f.srv.URL, "")

			raw, err := tr.Call(callCtx(t), MethodToolsList, nil)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			var res ToolsListResult
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(res.Tools) != 1 || res.Tools[0].Name != "search" {
				t.Fatalf("tools = %+v", res.Tools)
			}
		})
	}
}

// A stream may carry a server's own notifications and keep-alive comments before
// the answer. Skipping them is required; skipping the answer is not.
func TestHTTPSkipsUnrelatedStreamMessages(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(": keep-alive\n\n"))
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
		other := *req.ID + 500
		otherRaw, _ := json.Marshal(response{JSONRPC: "2.0", ID: &other, Result: json.RawMessage(`{"wrong":true}`)})
		fmt.Fprintf(w, "data: %s\n\n", otherRaw)
		raw, _ := json.Marshal(toolsListResult("right"))
		mine, _ := json.Marshal(response{JSONRPC: "2.0", ID: req.ID, Result: raw})
		fmt.Fprintf(w, "data: %s\n\n", mine)
	})
	tr := transportFor(t, f.srv.URL, "")

	raw, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var res ToolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "right" {
		t.Fatalf("the transport returned the wrong message: %+v", res.Tools)
	}
}

// A stream that ends without our answer must fail the call. The alternative is a
// caller waiting on its context deadline for a server that is already gone.
func TestHTTPStreamEndingWithoutAnswerFails(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
	})
	tr := transportFor(t, f.srv.URL, "")

	start := time.Now()
	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("a stream that never answered was treated as success")
	}
	if !strings.Contains(err.Error(), "without answering") {
		t.Errorf("error = %v, want it to say the stream ended unanswered", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s: the call waited on its deadline instead of noticing the stream ended", time.Since(start))
	}
}

// The transport must not accept a reply to a different request as its own. Two
// calls in flight would otherwise cross results.
func TestHTTPRejectsMismatchedID(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		other := *req.ID + 1
		writeJSONRPC(w, &other, toolsListResult("wrong"))
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err == nil {
		t.Fatal("a response for another request was accepted")
	}
}

// A malformed server sending both a result and an error must be read as the
// error: treating it as success hands the model a payload the server flagged as
// invalid.
func TestHTTPErrorWinsOverResultInOneResponse(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{
			JSONRPC: "2.0", ID: req.ID,
			Result: json.RawMessage(`{"tools":[]}`),
			Error:  &rpcError{Code: CodeInternalError, Message: "broken"},
		})
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		t.Fatalf("error = %v, want the JSON-RPC error", err)
	}
}

// A response with neither field is not a result. Returning nil,nil would make
// the caller decode an empty payload into a zero value and proceed.
func TestHTTPEmptyResponseIsAnError(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{JSONRPC: "2.0", ID: req.ID})
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err == nil {
		t.Fatal("a response with neither result nor error was accepted")
	}
}

func TestHTTPRejectsUnexpectedContentType(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>login page</html>"))
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("error = %v, want it to name the content type", err)
	}
}

// A JSON-RPC error must arrive typed, because Connect switches on the code to
// tell a legacy server from a version mismatch from a missing capability.
func TestHTTPServerErrorIsTyped(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{
				Code:    CodeUnsupportedProtocolVersion,
				Message: "nope",
				Data:    json.RawMessage(`{"supported":["2025-06-18"]}`),
			},
		})
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := tr.Call(callCtx(t), MethodDiscover, nil)
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		t.Fatalf("error = %v, want *rpcError", err)
	}
	if rpc.Code != CodeUnsupportedProtocolVersion {
		t.Errorf("code = %d", rpc.Code)
	}
	if got := rpc.supportedVersions(); len(got) != 1 || got[0] != "2025-06-18" {
		t.Errorf("supportedVersions = %v", got)
	}
}

// ---- sessions ------------------------------------------------------------

// The session id is assigned once and echoed on every later request, which is how
// a stateful server correlates calls.
func TestHTTPEchoesTheSessionID(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if r.Header.Get(sessionHeader) == "" {
			w.Header().Set(sessionHeader, "sess-1")
		}
		writeJSONRPC(w, req.ID, toolsListResult("search"))
	})
	tr := transportFor(t, f.srv.URL, "")

	for i := 0; i < 3; i++ {
		if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	rec := f.recorded()
	if len(rec) != 3 {
		t.Fatalf("got %d requests, want 3", len(rec))
	}
	if got := rec[0].Headers.Get(sessionHeader); got != "" {
		t.Errorf("first request carried a session id %q", got)
	}
	for i, r := range rec[1:] {
		if got := r.Headers.Get(sessionHeader); got != "sess-1" {
			t.Errorf("request %d session = %q, want sess-1", i+2, got)
		}
	}
}

// A server-chosen header value with a newline in it would let the server append a
// second header to every later request. The id is the one piece of server-chosen
// data this transport stores and resends, so it is filtered on the way in.
func TestHTTPSanitisesTheSessionID(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if r.Header.Get(sessionHeader) == "" {
			// http.Header.Set would reject this outright, so it is written the way a
			// non-Go server on the wire could.
			w.Header()[sessionHeader] = []string{"sess\r\nX-Injected: yes\tand-a-tab"}
		}
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr := transportFor(t, f.srv.URL, "")

	for i := 0; i < 2; i++ {
		if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	rec := f.recorded()
	got := rec[1].Headers.Get(sessionHeader)
	if strings.ContainsAny(got, "\r\n\t ") {
		t.Errorf("session id %q still carries control characters", got)
	}
	if _, ok := rec[1].Headers["X-Injected"]; ok {
		t.Error("the server injected a header through the session id")
	}
	if got == "" {
		t.Fatal("the id was dropped entirely, so the assertions above prove nothing")
	}
}

// A server that forgets the session answers 404. The specified recovery is to
// retry once without the stale id; retrying repeatedly against a server that
// rejects every session would loop.
func TestHTTPRetriesOnceWhenSessionExpires(t *testing.T) {
	var calls int
	var mu sync.Mutex
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch {
		case n == 1:
			w.Header().Set(sessionHeader, "sess-old")
			writeJSONRPC(w, req.ID, toolsListResult("first"))
		case r.Header.Get(sessionHeader) != "":
			// The session the client holds is unknown to us now.
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Header().Set(sessionHeader, "sess-new")
			writeJSONRPC(w, req.ID, toolsListResult("second"))
		}
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	raw, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err != nil {
		t.Fatalf("second call did not recover from an expired session: %v", err)
	}
	var res ToolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "second" {
		t.Fatalf("tools = %+v, want the result from the retry", res.Tools)
	}
	if n := len(f.recorded()); n != 3 {
		t.Errorf("%d requests, want 3 (call, rejected call, retry)", n)
	}
	if diag := tr.Diagnostics(); !strings.Contains(diag, "session") {
		t.Errorf("diagnostics = %q, want a note about the session", diag)
	}
}

// A server that answers 404 to every request, session or not, must fail rather
// than being retried forever.
func TestHTTPDoesNotRetrySessionForever(t *testing.T) {
	var calls int
	var mu sync.Mutex
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set(sessionHeader, "sess-old")
			writeJSONRPC(w, req.ID, toolsListResult())
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err == nil {
		t.Fatal("a server answering 404 to everything was treated as success")
	}
	// One call, one rejected retry-triggering request, one retry. A loop would
	// show far more.
	if n := len(f.recorded()); n != 3 {
		t.Errorf("%d requests, want exactly 3", n)
	}
}

// A server that hands out a fresh session id on the very response that rejects
// one is the shape that breaks a naive expiry check. The id header is stored
// before the status is read, so "do we hold a session" is true again by the time
// the 404 is classified — and a retry decided on that basis would never stop. The
// classification uses the session the request actually carried instead, so the
// retry (which carries none) is reported as a wrong endpoint and the caller never
// sees the internal sentinel.
func TestHTTPDoesNotLoopWhenTheServerReissuesASession(t *testing.T) {
	var calls int
	var mu sync.Mutex
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set(sessionHeader, "sess-1")
			writeJSONRPC(w, req.ID, toolsListResult())
			return
		}
		// Every later response both issues an id and rejects the request.
		w.Header().Set(sessionHeader, "sess-"+itoa(n))
		w.WriteHeader(http.StatusNotFound)
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("a server rejecting every session was treated as success")
	}
	if errors.Is(err, errSessionExpired) {
		t.Errorf("the internal session sentinel reached the caller: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want the status named", err)
	}
	if n := len(f.recorded()); n != 3 {
		t.Errorf("%d requests, want exactly 3 (call, rejected, one retry)", n)
	}
}

// 404 with no session held is a wrong URL, not an expired session. Retrying it as
// a session problem would hide the actual cause.
func TestHTTPNotFoundWithoutSessionIsNotASessionError(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such endpoint"))
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("HTTP 404 was treated as success")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to name the status", err)
	}
	if n := len(f.recorded()); n != 1 {
		t.Errorf("%d requests, want 1 (no retry without a session)", n)
	}
}

// Close releases server-side state, and does it without delaying exit. The DELETE
// carries the session id, because that is the only thing identifying what to
// release.
func TestHTTPCloseDeletesTheSession(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set(sessionHeader, "sess-9")
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr, err := StartHTTP(HTTPConfig{URL: f.srv.URL, Bearer: "tok"})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := f.recorded()
	last := rec[len(rec)-1]
	if last.Method != http.MethodDelete {
		t.Fatalf("last request was %s, want DELETE", last.Method)
	}
	if got := last.Headers.Get(sessionHeader); got != "sess-9" {
		t.Errorf("DELETE session = %q", got)
	}
	if got := last.Headers.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("DELETE Authorization = %q, want the token (a server would refuse it otherwise)", got)
	}
	// Idempotent, and no call may proceed afterwards.
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := tr.Call(context.Background(), MethodToolsList, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after Close = %v, want ErrClosed", err)
	}
	if n := len(f.recorded()); n != len(rec) {
		t.Errorf("a request was sent after Close: %d, was %d", n, len(rec))
	}
}

// Close with no session must not send a DELETE: there is nothing to release, and
// a bare DELETE to an endpoint is a request the server has to interpret.
func TestHTTPCloseWithoutSessionSendsNothing(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr, err := StartHTTP(HTTPConfig{URL: f.srv.URL})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	before := len(f.recorded())
	_ = tr.Close()
	if got := len(f.recorded()); got != before {
		t.Errorf("Close sent %d extra request(s) with no session", got-before)
	}
}

// A server that hangs on DELETE must not hold the CLI's exit. deleteTimeout is
// what bounds it.
func TestHTTPCloseDoesNotWaitOnAHangingDelete(t *testing.T) {
	release := make(chan struct{})
	// defer, not t.Cleanup: httptest's Close waits for in-flight handlers, and
	// cleanups run in reverse registration order — so a t.Cleanup here would let
	// the server's own cleanup block on the handler this channel releases.
	defer close(release)
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if r.Method == http.MethodDelete {
			<-release
			return
		}
		w.Header().Set(sessionHeader, "sess-h")
		writeJSONRPC(w, req.ID, toolsListResult())
	})
	tr, err := StartHTTP(HTTPConfig{URL: f.srv.URL})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = tr.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(deleteTimeout + 5*time.Second):
		t.Fatal("Close blocked on a server that never answered DELETE")
	}
}

// ---- redirects, statuses and limits -------------------------------------

// A cross-origin redirect is refused, not followed: the result the model receives
// must come from the host the user configured, and no later check can tell that
// it did not.
func TestHTTPRefusesCrossOriginRedirect(t *testing.T) {
	elsewhere := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		writeJSONRPC(w, req.ID, toolsListResult("attacker_tool"))
	})
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		http.Redirect(w, r, elsewhere.srv.URL, http.StatusTemporaryRedirect)
	})
	tr := transportFor(t, f.srv.URL, "tok")

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("a cross-origin redirect was followed")
	}
	if !strings.Contains(err.Error(), "different origin") {
		t.Errorf("error = %v, want it to name the origin change", err)
	}
	if n := len(elsewhere.recorded()); n != 0 {
		t.Errorf("the other origin received %d request(s)", n)
	}
	if diag := tr.Diagnostics(); !strings.Contains(diag, "refused redirect") {
		t.Errorf("diagnostics = %q, want the refusal recorded", diag)
	}
}

// Same-origin redirects are followed, because real deployments redirect /mcp to
// /mcp/ and refusing that would fail against working servers.
func TestHTTPFollowsSameOriginRedirect(t *testing.T) {
	var f *httpFake
	f = newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, f.srv.URL+"/final", http.StatusTemporaryRedirect)
			return
		}
		writeJSONRPC(w, req.ID, toolsListResult("ok_tool"))
	})
	tr := transportFor(t, f.srv.URL+"/mcp", "tok")

	raw, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err != nil {
		t.Fatalf("a same-origin redirect was refused: %v", err)
	}
	var res ToolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "ok_tool" {
		t.Fatalf("tools = %+v", res.Tools)
	}
	// The redirected POST must still carry the credential and the body, or the
	// server sees an unauthenticated empty request.
	last := f.recorded()[len(f.recorded())-1]
	if got := last.Headers.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("redirected request Authorization = %q", got)
	}
	if !strings.Contains(last.Body, MethodToolsList) {
		t.Errorf("redirected request body = %q, want the original method", last.Body)
	}
}

// A redirect loop within one origin must stop. maxRedirects is the bound.
func TestHTTPStopsARedirectLoop(t *testing.T) {
	var f *httpFake
	f = newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		http.Redirect(w, r, f.srv.URL+"/again", http.StatusTemporaryRedirect)
	})
	tr := transportFor(t, f.srv.URL, "")

	if _, err := tr.Call(callCtx(t), MethodToolsList, nil); err == nil {
		t.Fatal("a same-origin redirect loop did not stop")
	}
	if n := len(f.recorded()); n > maxRedirects+1 {
		t.Errorf("%d requests, want at most %d", n, maxRedirects+1)
	}
}

// 401 and 403 are configuration faults, and the message has to say which of the
// two situations the user is in — no token sent, or a token the server rejected.
func TestHTTPAuthFailureNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		name, bearer, want string
		status             int
	}{
		{"no token", "", "no token was sent", http.StatusUnauthorized},
		{"bad token", "tok", "the configured token was rejected", http.StatusUnauthorized},
		{"forbidden", "tok", "the configured token was rejected", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
				w.WriteHeader(tc.status)
			})
			tr := transportFor(t, f.srv.URL, tc.bearer)

			_, err := tr.Call(callCtx(t), MethodToolsList, nil)
			if err == nil {
				t.Fatalf("HTTP %d was treated as success", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A 5xx body is the server's own explanation, so a short sanitised excerpt is
// worth keeping — the alternative is "HTTP 500" and nothing else.
func TestHTTPServerErrorBodyIsQuotedSafely(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream\nis\rdown"))
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("HTTP 502 was treated as success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "502") || !strings.Contains(msg, "upstream") {
		t.Errorf("error = %v, want the status and the body excerpt", msg)
	}
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("error message carries newlines from the server body: %q", msg)
	}
}

// An endless stream must be stopped by the client. Without the byte cap this is a
// memory-exhaustion vector reachable from the network.
func TestHTTPStopsAnEndlessStream(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop) // see TestHTTPCloseDoesNotWaitOnAHangingDelete
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		chunk := "data: " + strings.Repeat("x", 8<<10) + "\n\n"
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	})
	tr := transportFor(t, f.srv.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := tr.Call(ctx, MethodToolsList, nil)
	if err == nil {
		t.Fatal("an endless stream was read to completion")
	}
	if ctx.Err() != nil {
		t.Fatalf("the call ended on its deadline rather than on the byte cap: %v", err)
	}
	// Specifically the cap, not merely "some error": the two messages mean
	// different things to an operator, and accepting either would let this pass if
	// the stream had simply ended.
	if !strings.Contains(err.Error(), "streamed more than") {
		t.Errorf("error = %v, want the byte cap named", err)
	}
}

// An oversized single JSON body is refused rather than decoded from a truncated
// prefix, which would either fail confusingly or, worse, parse.
func TestHTTPRefusesAnOversizedJSONBody(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A valid prefix followed by more than the cap, so a client that read only
		// the first bytes could believe it had a complete message.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"padding":"`))
		pad := strings.Repeat("y", 64<<10)
		for written := 0; written < maxHTTPBodyBytes+(1<<20); written += len(pad) {
			if _, err := w.Write([]byte(pad)); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}}`))
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want the size limit named", err)
	}
}

// Cancelling a call must return promptly with the context's error, not wait for
// the server. This is the requirement in .ai/RULES.md:40 for networked work.
func TestHTTPCallHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // see TestHTTPCloseDoesNotWaitOnAHangingDelete
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-release
	})
	tr := transportFor(t, f.srv.URL, "")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := tr.Call(ctx, MethodToolsList, nil)
	if err == nil {
		t.Fatal("a cancelled call returned success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s", elapsed)
	}
}

// A notification carries no id, so there is nothing to read back — and a server
// answering 202 with an empty body must not be read as a failure.
func TestHTTPNotifyExpectsNoAnswer(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if req.ID != nil {
			t.Error("a notification carried an id")
		}
		w.WriteHeader(http.StatusAccepted)
	})
	tr := transportFor(t, f.srv.URL, "")

	if err := tr.Notify(callCtx(t), MethodCancelled, map[string]any{"requestId": 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if n := len(f.recorded()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// An unreachable endpoint must fail with a message naming the origin, and must
// not repeat a query string that could carry a server-side token.
func TestHTTPUnreachableEndpointNamesTheOrigin(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {})
	url := f.srv.URL + "/mcp?session_key=dummy-secret-in-query"
	tr := transportFor(t, url, "")
	f.srv.Close() // now nothing is listening

	_, err := tr.Call(callCtx(t), MethodToolsList, nil)
	if err == nil {
		t.Fatal("a call to a closed server succeeded")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") && !strings.Contains(err.Error(), "localhost") {
		t.Errorf("error = %v, want the origin named", err)
	}
	if strings.Contains(err.Error(), "dummy-secret-in-query") {
		t.Errorf("error message repeats the query string: %v", err)
	}
}
