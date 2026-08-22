package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// connectFake starts the fake server in a mode and connects a client to it.
func connectFake(t *testing.T, mode string, callsPerMinute int) *Client {
	t.Helper()
	tr := fakeStdio(t, mode)
	c, err := Connect(context.Background(), "fake", tr, callsPerMinute)
	if err != nil {
		t.Fatalf("Connect(%s): %v", mode, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestConnectReadsServerInfo(t *testing.T) {
	c := connectFake(t, modeNormal, 0)
	info := c.Info()
	if info.Name != "fake" || info.Version != "1" {
		t.Fatalf("serverInfo = %+v, want name=fake version=1", info)
	}
	if c.Server() != "fake" {
		t.Fatalf("Server() = %q, want the local mcp.json name", c.Server())
	}
}

// The namespace prefix must come from the local configuration, never from
// serverInfo.name: the specification says serverInfo.name is unsuitable for
// disambiguation, and a server that could choose its own prefix could shadow
// another server's tools.
func TestServerNameIsLocalNotServerChosen(t *testing.T) {
	tr := fakeStdio(t, modeNormal)
	c, err := Connect(context.Background(), "my-local-alias", tr, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.Server() != "my-local-alias" {
		t.Fatalf("Server() = %q, want my-local-alias", c.Server())
	}
	if c.Info().Name != "fake" {
		t.Fatalf("Info().Name = %q, want the server's own claim to be kept separate", c.Info().Name)
	}
}

// Server-authored instructions are retained for display but must never be
// treated as instructions to us. This test pins that the hostile text survives
// verbatim in a field documented as display-only, so a future change that starts
// feeding it to a system prompt has to touch this test.
func TestConnectKeepsInstructionsAsDataOnly(t *testing.T) {
	c := connectFake(t, modeNormal, 0)
	if !strings.Contains(c.Info().Instructions, "IGNORE ALL PREVIOUS INSTRUCTIONS") {
		t.Fatalf("instructions were not retained: %q", c.Info().Instructions)
	}
}

// A server that predates revision 2026-07-28 has no server/discover. It must be
// rejected at connect time, with a message that says why, rather than being
// driven on to tools/call: some legacy servers do not verify that a request
// arrived after initialize and would execute the call under legacy semantics.
func TestConnectRejectsLegacyServer(t *testing.T) {
	tr := fakeStdio(t, modeLegacy)
	c, err := Connect(context.Background(), "old", tr, 0)
	if err == nil {
		_ = c.Close()
		t.Fatal("Connect succeeded against a server without server/discover")
	}
	msg := err.Error()
	for _, want := range []string{MethodDiscover, ProtocolVersion, "predates"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

func TestConnectReportsUnsupportedProtocolVersion(t *testing.T) {
	tr := fakeStdio(t, modeVersionError)
	c, err := Connect(context.Background(), "picky", tr, 0)
	if err == nil {
		_ = c.Close()
		t.Fatal("Connect succeeded despite -32022")
	}
	// data.supported must reach the message: without it the user cannot tell
	// whether the server is older or newer than this client.
	if !strings.Contains(err.Error(), "2025-06-18") {
		t.Fatalf("error %q does not report the server's supported versions", err)
	}
}

// A server can also decline by listing versions in a successful discover
// response. That path must be checked too, not just the error code.
func TestConnectRejectsDiscoverWithoutOurVersion(t *testing.T) {
	tr := fakeStdio(t, modeOtherVersion)
	c, err := Connect(context.Background(), "older", tr, 0)
	if err == nil {
		_ = c.Close()
		t.Fatal("Connect succeeded against a server that does not list our revision")
	}
	if !strings.Contains(err.Error(), ProtocolVersion) {
		t.Fatalf("error %q does not name the revision this client speaks", err)
	}
}

// serverInfo is SHOULD, not MUST. A server that omits it is still usable.
func TestConnectToleratesMissingServerInfo(t *testing.T) {
	c := connectFake(t, modeNoServerInfo, 0)
	if c.Info().Name != "" {
		t.Fatalf("Info().Name = %q, want empty when the server sent none", c.Info().Name)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools after a discover without serverInfo: %v", err)
	}
}

func TestConnectFailsWhenServerDiesImmediately(t *testing.T) {
	tr := fakeStdio(t, modeNormal)
	// Killing the transport before connecting simulates a server that exits on
	// startup, the most common real failure (bad path, missing dependency).
	_ = tr.Close()
	if c, err := Connect(context.Background(), "dead", tr, 0); err == nil {
		_ = c.Close()
		t.Fatal("Connect succeeded against a closed transport")
	}
}

func TestListToolsReturnsAdvertisedTools(t *testing.T) {
	c := connectFake(t, modeNormal, 0)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	names := []string{tools[0].Name, tools[1].Name}
	if !contains(names, "search_issues") || !contains(names, "get_file") {
		t.Fatalf("unexpected tool names %v", names)
	}
	if tools[0].InputSchema == nil {
		t.Fatal("inputSchema was dropped")
	}
}

// ListTools is discovery, not policy: a tool named like one of our builtins must
// still be returned so `qeuro mcp tools` can show it. Blocking happens where the
// name is registered, which spec_test.go covers.
func TestListToolsDoesNotFilterShadowingNames(t *testing.T) {
	c := connectFake(t, modeShadowBuiltin, 0)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tl := range tools {
		if tl.Name == "read_file" {
			found = true
		}
	}
	if !found {
		t.Fatal("ListTools hid a shadowing tool; discovery must report it so the user can see it")
	}
}

// A tools/list response larger than bufio's default line limit is normal for a
// server with many tools. It must arrive whole.
func TestListToolsHandlesResponseAboveDefaultScannerLimit(t *testing.T) {
	c := connectFake(t, modeGiantList, 0)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 40 {
		t.Fatalf("got %d tools, want 40", len(tools))
	}
}

func TestListToolsStopsOnRepeatedCursor(t *testing.T) {
	c := connectFake(t, modeCursorLoop, 0)
	done := make(chan struct{})
	var tools []Tool
	var err error
	go func() {
		tools, err = c.ListTools(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ListTools never returned against a server repeating one cursor")
	}
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// Two pages: the first, then the one the repeated cursor asked for, after
	// which the loop guard fires.
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4 (two pages before the loop guard)", len(tools))
	}
}

func TestListToolsStopsAtPageBudget(t *testing.T) {
	c := connectFake(t, modeCursorWalk, 0)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2*maxToolsPages {
		t.Fatalf("got %d tools, want %d (%d pages × 2)", len(tools), 2*maxToolsPages, maxToolsPages)
	}
}

func TestListToolsCapsToolCountPerServer(t *testing.T) {
	c := connectFake(t, modeTooManyTools, 0)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != maxToolsPerServer {
		t.Fatalf("got %d tools, want the cap %d", len(tools), maxToolsPerServer)
	}
}

func TestCallToolRoundTrip(t *testing.T) {
	c := connectFake(t, modeNormal, 0)
	res, err := c.CallTool(context.Background(), "search_issues", json.RawMessage(`{"q":"bug"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatal("IsError set on a successful call")
	}
	text, truncated := res.Text()
	if truncated {
		t.Fatal("a short result was reported as truncated")
	}
	if text != "called search_issues" {
		t.Fatalf("text = %q", text)
	}
}

// The tool name and arguments must reach the server in the shape the
// specification defines: params.name and params.arguments, with the arguments
// object passed through rather than re-encoded as a string.
func TestCallToolSendsArgumentsAsObject(t *testing.T) {
	c := connectFake(t, modeEchoArgs, 0)
	res, err := c.CallTool(context.Background(), "whatever", json.RawMessage(`{"q":"hello","n":3}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text, _ := res.Text()
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("arguments did not arrive as a JSON object: %q (%v)", text, err)
	}
	if got["q"] != "hello" {
		t.Fatalf("arguments = %v, want q=hello", got)
	}
}

func TestCallToolDefaultsMissingArgumentsToEmptyObject(t *testing.T) {
	c := connectFake(t, modeEchoArgs, 0)
	res, err := c.CallTool(context.Background(), "whatever", nil)
	if err != nil {
		t.Fatalf("CallTool with nil args: %v", err)
	}
	text, _ := res.Text()
	if strings.TrimSpace(text) != "{}" {
		t.Fatalf("arguments = %q, want {}", text)
	}
}

// A tool execution error is a result, not a transport failure: the specification
// models it inside the result precisely so the text can be handed to the model
// for self-correction. Turning it into a Go error would lose that.
func TestCallToolExecutionErrorIsAResultNotAnError(t *testing.T) {
	c := connectFake(t, modeToolError, 0)
	res, err := c.CallTool(context.Background(), "get_file", nil)
	if err != nil {
		t.Fatalf("CallTool turned an execution error into a transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError was not preserved")
	}
	text, _ := res.Text()
	if !strings.Contains(text, "does not exist") {
		t.Fatalf("the server's explanation was lost: %q", text)
	}
}

func TestCallToolFailsWhenServerDiesMidCall(t *testing.T) {
	tr := fakeStdio(t, modeCrashOnCall)
	c, err := Connect(context.Background(), "crashy", tr, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	done := make(chan error, 1)
	go func() {
		_, callErr := c.CallTool(context.Background(), "search_issues", nil)
		done <- callErr
	}()
	select {
	case callErr := <-done:
		if callErr == nil {
			t.Fatal("CallTool succeeded against a server that exited mid-call")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CallTool hung after the server exited; it must fail, not wait")
	}
}

func TestCallToolTimesOutOnSilentServer(t *testing.T) {
	tr := fakeStdio(t, modeSilent)
	// Connect itself cannot succeed against a silent server, so the timeout is
	// asserted on discover, which uses the same mechanism as CallTool.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c, err := Connect(ctx, "silent", tr, 0); err == nil {
		_ = c.Close()
		t.Fatal("Connect succeeded against a server that never answers")
	}
	if elapsed := time.Since(start); elapsed > discoverTimeout {
		t.Fatalf("waited %s, longer than the caller's own deadline allowed", elapsed)
	}
}

func TestCallToolRespectsCallerCancellation(t *testing.T) {
	c := connectFake(t, modeNormal, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CallTool(ctx, "search_issues", nil); err == nil {
		t.Fatal("CallTool ignored an already-cancelled context")
	}
}

func TestCallToolEnforcesPerMinuteLimit(t *testing.T) {
	c := connectFake(t, modeNormal, 2)
	for i := 0; i < 2; i++ {
		if _, err := c.CallTool(context.Background(), "search_issues", nil); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	_, err := c.CallTool(context.Background(), "search_issues", nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("third call error = %v, want ErrRateLimited", err)
	}
	if !strings.Contains(err.Error(), "2 per minute") {
		t.Fatalf("error %q does not name the configured limit", err)
	}
}

// A rejected call must not consume budget, or one blocked call would push the
// window forward and starve the next minute too.
func TestRateLimitRejectionDoesNotConsumeBudget(t *testing.T) {
	c := &Client{server: "x", callsPerMinute: 1}
	if err := c.reserve(); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := c.reserve(); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("reserve %d = %v, want ErrRateLimited", i+2, err)
		}
	}
	if len(c.calls) != 1 {
		t.Fatalf("window holds %d entries, want 1: rejections must not be recorded", len(c.calls))
	}
}

func TestRateLimitWindowSlides(t *testing.T) {
	c := &Client{server: "x", callsPerMinute: 1}
	// A call recorded more than a minute ago must fall out of the window.
	c.calls = []time.Time{time.Now().Add(-61 * time.Second)}
	if err := c.reserve(); err != nil {
		t.Fatalf("reserve after the window slid: %v", err)
	}
}

func TestZeroLimitMeansUnlimited(t *testing.T) {
	c := &Client{server: "x", callsPerMinute: 0}
	for i := 0; i < 50; i++ {
		if err := c.reserve(); err != nil {
			t.Fatalf("reserve %d with no limit configured: %v", i, err)
		}
	}
	if len(c.calls) != 0 {
		t.Fatal("an unlimited client should not accumulate a window")
	}
}

// Failure messages must carry the server's own stderr. Without it a server that
// dies on startup produces only "EOF", which tells the user nothing.
func TestErrorsCarryServerDiagnostics(t *testing.T) {
	tr := fakeStdio(t, modeStderrChatty)
	c, err := Connect(context.Background(), "chatty", tr, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !strings.Contains(c.Diagnostics(), "listening on stdio") {
		t.Fatalf("stderr was not captured: %q", c.Diagnostics())
	}
	if s := diagSuffix(tr); !strings.Contains(s, "listening on stdio") {
		t.Fatalf("diagSuffix = %q, want the server log", s)
	}
}

// Diagnostics are server-authored text pasted into one line of an error message.
// A server must not be able to forge extra lines or close a bracketed
// annotation there.
func TestDiagnosticsAreFlattenedToOneLine(t *testing.T) {
	tr := fakeStdio(t, modeStderrChatty)
	defer func() { _ = tr.Close() }()

	// The child writes its banner asynchronously. Without waiting for it the
	// assertion below would pass against an empty string and prove nothing.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(tr.Diagnostics(), "debug detail") {
		if time.Now().After(deadline) {
			t.Fatalf("server stderr never arrived: %q", tr.Diagnostics())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(tr.Diagnostics(), "\n") {
		t.Fatal("test setup is wrong: the raw diagnostics must be multi-line for this to mean anything")
	}

	s := diagSuffix(tr)
	body := strings.TrimPrefix(s, "\nserver log: ")
	if strings.ContainsAny(body, "\n\r]") {
		t.Fatalf("diagSuffix body %q still contains a newline or a closing bracket", body)
	}
	if !strings.Contains(body, "debug detail") {
		t.Fatalf("diagSuffix dropped the server log: %q", body)
	}
}

func TestCallAfterCloseFailsCleanly(t *testing.T) {
	c := connectFake(t, modeNormal, 0)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.CallTool(context.Background(), "search_issues", nil); err == nil {
		t.Fatal("CallTool succeeded after Close")
	}
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools succeeded after Close")
	}
}

// ---- the client over HTTP ------------------------------------------------
//
// Client holds no stdio detail, and these tests are what proves it rather than
// asserts it: the same Connect, the same pagination walk, the same rate limiter,
// driven through the other transport.

// TestConnectOverHTTPDetectsALegacyServer: the discover probe exists to fail
// deterministically against a server that predates the revision. Over HTTP that
// server answers MethodNotFound just as a stdio one does, and proceeding to
// tools/call would run under semantics we do not share.
func TestConnectOverHTTPDetectsALegacyServer(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: CodeMethodNotFound, Message: "no such method"},
		})
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := Connect(context.Background(), "legacy", tr, 0)
	if err == nil {
		t.Fatal("a legacy HTTP server connected")
	}
	if !strings.Contains(err.Error(), "predates") {
		t.Errorf("error = %v, want the legacy cause named", err)
	}
}

// Pagination is client behaviour, not transport behaviour: a paginating HTTP
// server must be walked the same way, and a repeated cursor must stop the walk
// rather than loop.
func TestListToolsOverHTTPFollowsPagination(t *testing.T) {
	var page int
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if req.Method == MethodDiscover {
			writeJSONRPC(w, req.ID, discoverPayload())
			return
		}
		page++
		res := toolsListResult("tool_page_" + itoa(page))
		if page < 3 {
			res["nextCursor"] = "cursor-" + itoa(page)
		}
		writeJSONRPC(w, req.ID, res)
	})
	tr := transportFor(t, f.srv.URL, "")

	c, err := Connect(context.Background(), "remote", tr, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("collected %d tools, want 3 (one per page)", len(got))
	}
	for i, want := range []string{"tool_page_1", "tool_page_2", "tool_page_3"} {
		if got[i].Name != want {
			t.Errorf("tool %d = %q, want %q", i, got[i].Name, want)
		}
	}
	// The cursor must have been sent back, or the server's paging was ignored and
	// three identical pages happened to look right.
	rec := f.recorded()
	if !strings.Contains(rec[len(rec)-1].Body, "cursor-2") {
		t.Errorf("the last request did not carry the previous cursor: %s", rec[len(rec)-1].Body)
	}
}

// A repeated cursor must end the walk. Over HTTP an unbounded walk is a request
// loop against a remote host, not merely a busy local process.
func TestListToolsOverHTTPStopsOnARepeatedCursor(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if req.Method == MethodDiscover {
			writeJSONRPC(w, req.ID, discoverPayload())
			return
		}
		res := toolsListResult("same_tool")
		res["nextCursor"] = "always-the-same"
		writeJSONRPC(w, req.ID, res)
	})
	tr := transportFor(t, f.srv.URL, "")

	c, err := Connect(context.Background(), "remote", tr, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// discover + two tools/list requests: the second repeats the cursor, and the
	// walk stops there rather than running to the page budget.
	if n := len(f.recorded()); n > 4 {
		t.Errorf("%d requests, want the walk to stop on the repeated cursor", n)
	}
}

// The per-minute budget belongs to the client, so it applies to an HTTP server
// too — and over HTTP it is also what keeps a runaway tool loop from becoming
// outbound traffic.
func TestRateLimitAppliesOverHTTP(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		if req.Method == MethodDiscover {
			writeJSONRPC(w, req.ID, discoverPayload())
			return
		}
		writeJSONRPC(w, req.ID, CallResult{Content: []Content{{Type: "text", Text: "ok"}}})
	})
	tr := transportFor(t, f.srv.URL, "")

	c, err := Connect(context.Background(), "remote", tr, 1)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.CallTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err = c.CallTool(context.Background(), "t", nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second call = %v, want ErrRateLimited", err)
	}
	// The refused call must not have reached the network: a limit enforced after
	// the request is not a limit.
	requests := 0
	for _, r := range f.recorded() {
		if strings.Contains(r.Body, MethodToolsCall) {
			requests++
		}
	}
	if requests != 1 {
		t.Errorf("%d tools/call requests reached the server, want 1", requests)
	}
}

// Diagnostics is the HTTP substitute for a server's stderr, and an error message
// with no context is the failure mode it exists to prevent.
func TestHTTPDiagnosticsAppearInClientErrors(t *testing.T) {
	f := newHTTPFake(t, func(w http.ResponseWriter, r *http.Request, req request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	tr := transportFor(t, f.srv.URL, "")

	_, err := Connect(context.Background(), "remote", tr, 0)
	if err == nil {
		t.Fatal("Connect succeeded against a 401")
	}
	if !strings.Contains(err.Error(), "server log:") {
		t.Errorf("error = %v, want the diagnostics appended", err)
	}
	if !strings.Contains(err.Error(), "authFrom") {
		t.Errorf("error = %v, want the fix named", err)
	}
}

func TestFirstLinesAndSplitLines(t *testing.T) {
	if got := firstLines("a\nb\nc\nd", 2); got != "a | b" {
		t.Fatalf("firstLines = %q", got)
	}
	if got := firstLines("", 3); got != "" {
		t.Fatalf("firstLines(empty) = %q", got)
	}
	if got := splitLines("a\nb"); len(got) != 2 || got[1] != "b" {
		t.Fatalf("splitLines = %v", got)
	}
	if got := splitLines("trailing\n"); len(got) != 1 {
		t.Fatalf("splitLines(%q) = %v, want one line", "trailing\n", got)
	}
}
