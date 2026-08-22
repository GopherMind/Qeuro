package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// meta is the _meta block the revision requires on every request.
const testMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// serve runs the server over the given input lines and returns the responses, one
// per element, in the order written. Diagnostics are captured separately so the
// stdout-is-MCP-only rule can be asserted rather than assumed.
func serve(t *testing.T, lines ...string) ([]response, string) {
	t.Helper()
	var out, logw strings.Builder
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	if code := Serve(context.Background(), in, &out, &logw); code != 0 {
		t.Fatalf("Serve returned %d, want 0; log: %s", code, logw.String())
	}
	var got []response
	for _, l := range splitLines(strings.TrimRight(out.String(), "\n")) {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var r response
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("response line is not JSON: %q (%v)", l, err)
		}
		got = append(got, r)
	}
	return got, logw.String()
}

func req(id int, method, params string) string {
	if params == "" {
		params = "{" + testMeta + "}"
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func callReq(id int, name, args string) string {
	return req(id, MethodToolsCall, `{"name":"`+name+`","arguments":`+args+`,`+testMeta+`}`)
}

func only(t *testing.T, rs []response) response {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("want exactly 1 response, got %d: %+v", len(rs), rs)
	}
	return rs[0]
}

func resultText(t *testing.T, r response) (string, bool) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("want a result, got error %d %q", r.Error.Code, r.Error.Message)
	}
	var cr CallResult
	if err := json.Unmarshal(r.Result, &cr); err != nil {
		t.Fatalf("result is not a CallResult: %v", err)
	}
	text, _ := cr.Text()
	return text, cr.IsError
}

// --- the protocol contract ------------------------------------------------

func TestDiscoverAnswersWithoutAHandshake(t *testing.T) {
	// The point of revision 2026-07-28 is that there is no initialize step. A
	// server that only answers after one would deadlock every conforming client,
	// so the very first message here is a real request.
	rs, _ := serve(t, req(1, MethodDiscover, ""))
	r := only(t, rs)
	if r.Error != nil {
		t.Fatalf("discover failed: %d %q", r.Error.Code, r.Error.Message)
	}
	var p DiscoverResult
	if err := json.Unmarshal(r.Result, &p); err != nil {
		t.Fatalf("discover result does not decode into DiscoverResult: %v", err)
	}
	if len(p.SupportedVersions) == 0 || p.SupportedVersions[0] != ProtocolVersion {
		t.Fatalf("supportedVersions = %v, want [%s]", p.SupportedVersions, ProtocolVersion)
	}
}

func TestToolsCallWorksAsTheFirstMessage(t *testing.T) {
	rs, _ := serve(t, callReq(1, "qeuro.plan", "{}"))
	if _, isErr := resultText(t, only(t, rs)); isErr {
		t.Fatal("qeuro.plan reported an execution error in a real repository")
	}
}

func TestUnsupportedProtocolVersionIsRejectedBeforeDispatch(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"qeuro.plan","arguments":{},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2024-11-05","io.modelcontextprotocol/clientCapabilities":{}}}}`
	r := only(t, mustErrors(t, line))
	if r.Error.Code != CodeUnsupportedProtocolVersion {
		t.Fatalf("code = %d, want %d", r.Error.Code, CodeUnsupportedProtocolVersion)
	}
	// data.supported is what lets a client renegotiate instead of giving up.
	var d struct{ Supported []string }
	if err := json.Unmarshal(r.Error.Data, &d); err != nil {
		t.Fatalf("error data does not decode: %v", err)
	}
	if len(d.Supported) == 0 || d.Supported[0] != ProtocolVersion {
		t.Fatalf("data.supported = %v, want [%s]", d.Supported, ProtocolVersion)
	}
	// And no result may have been computed: the whole reason for checking before
	// dispatch is that a mismatched client must not receive tool output.
	if r.Result != nil {
		t.Fatalf("a rejected request still carried a result: %s", r.Result)
	}
}

func TestMissingClientCapabilitiesIsItsOwnError(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	r := only(t, mustErrors(t, line))
	if r.Error.Code != CodeMissingClientCapability {
		t.Fatalf("code = %d, want %d", r.Error.Code, CodeMissingClientCapability)
	}
}

func TestMissingProtocolVersionIsInvalidParams(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`
	r := only(t, mustErrors(t, line))
	if r.Error.Code != CodeInvalidParams {
		t.Fatalf("code = %d, want %d", r.Error.Code, CodeInvalidParams)
	}
}

func TestUnparseableLineGetsANullIDParseError(t *testing.T) {
	// Dropping it silently would leave a client blocked on a response forever, so
	// the id must be present and explicitly null.
	var out, logw strings.Builder
	Serve(context.Background(), strings.NewReader("not json\n"), &out, &logw)
	line := strings.TrimSpace(out.String())
	if !strings.Contains(line, `"id":null`) {
		t.Fatalf("parse error must carry an explicit null id, got %s", line)
	}
	var r response
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if r.Error == nil || r.Error.Code != CodeParseError {
		t.Fatalf("want a parse error, got %+v", r.Error)
	}
}

func TestNotificationsAreNotAnswered(t *testing.T) {
	// A response to a notification is a protocol violation, and an error response
	// to one is worse: the client has nothing to correlate it with.
	rs, _ := serve(t,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		req(2, MethodToolsList, ""),
	)
	if len(rs) != 1 {
		t.Fatalf("want 1 response (the tools/list), got %d: %+v", len(rs), rs)
	}
}

func TestWrongJSONRPCVersionIsRejected(t *testing.T) {
	r := only(t, mustErrors(t, `{"jsonrpc":"1.0","id":1,"method":"tools/list","params":{`+testMeta+`}}`))
	if r.Error.Code != CodeInvalidRequest {
		t.Fatalf("code = %d, want %d", r.Error.Code, CodeInvalidRequest)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	r := only(t, mustErrors(t, req(1, "resources/list", "")))
	if r.Error.Code != CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", r.Error.Code, CodeMethodNotFound)
	}
}

func TestUnknownToolIsAProtocolErrorNotAToolError(t *testing.T) {
	// The distinction is what the caller does next: an isError result invites the
	// model to retry with better arguments, while "no such tool" tells it to stop
	// using that name. Reporting the second as the first makes the model loop.
	r := only(t, mustErrors(t, callReq(1, "qeuro.delete_everything", "{}")))
	if r.Error.Code != CodeInvalidParams {
		t.Fatalf("code = %d, want %d", r.Error.Code, CodeInvalidParams)
	}
}

func TestEveryAdvertisedToolHasAnImplementation(t *testing.T) {
	// tools/list is a promise. A spec with no function behind it answers every
	// call with "unknown tool", which reads to the caller as a lying catalogue.
	for _, tl := range serverTools() {
		if _, ok := serverToolFuncs[tl.Name]; !ok {
			t.Errorf("tool %q is advertised but has no implementation", tl.Name)
		}
		if tl.Description == "" || tl.InputSchema == nil {
			t.Errorf("tool %q is advertised without a description or schema", tl.Name)
		}
	}
	for name := range serverToolFuncs {
		if _, ok := serverToolSpecs[name]; !ok {
			t.Errorf("tool %q is implemented but never advertised", name)
		}
	}
}

func TestToolsAreListedInAStableOrder(t *testing.T) {
	// Comparing successive calls to each other only proves they agree — Go's map
	// iteration order would fail that most of the time, but a reversed sort would
	// pass it forever. The assertion has to be against a fixed expectation.
	want := []string{"qeuro.cost", "qeuro.diff", "qeuro.plan", "qeuro.run_task"}
	got := serverTools()
	if len(got) != len(want) {
		t.Fatalf("got %d tools, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("tool %d is %s, want %s: the list is not in ascending name order",
				i, got[i].Name, want[i])
		}
	}
}

// --- stdout discipline ----------------------------------------------------

func TestDiagnosticsNeverReachTheMessageStream(t *testing.T) {
	// stdout is the transport. A banner on it desynchronises the framing for the
	// client, which then reports our own startup line as a malformed message.
	rs, log := serve(t, req(1, MethodToolsList, ""))
	if len(rs) != 1 {
		t.Fatalf("stdout carried %d lines, want exactly the 1 response", len(rs))
	}
	if !strings.Contains(log, "qeuro mcp serve") {
		t.Fatalf("the startup diagnostic went somewhere other than the log writer: %q", log)
	}
}

func TestEachResponseIsExactlyOneLine(t *testing.T) {
	// The framing is newline-delimited, and a tool result contains newlines. It
	// stays one line only because json.Marshal escapes them.
	var out, logw strings.Builder
	in := strings.NewReader(callReq(1, "qeuro.plan", "{}") + "\n")
	Serve(context.Background(), in, &out, &logw)
	if n := strings.Count(strings.TrimRight(out.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("one response spanned %d extra lines", n+1)
	}
}

// --- the tools ------------------------------------------------------------

func TestRunTaskFailsHonestly(t *testing.T) {
	// It must be an execution error inside result, not a protocol error: the tool
	// exists and the contract is real. What is missing is an approval channel —
	// this transport's stdin already carries JSON-RPC, so a request for the
	// user's decision has nobody to answer it. The refusal must name that reason
	// rather than a stale one: a caller told "not implemented yet" waits for a
	// release, while a caller told "no approval channel here" switches transport.
	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.run_task", `{"task":"refactor"}`))))
	if !isErr {
		t.Fatal("run_task claimed success while it cannot obtain approvals")
	}
	if !strings.Contains(text, "approval channel") {
		t.Fatalf("the failure does not say what is wrong: %q", text)
	}
	// And it must point at a transport that can: a refusal without an
	// alternative reads as "this product cannot do it".
	if !strings.Contains(text, "--headless") {
		t.Errorf("the refusal offers no working path: %q", text)
	}
}

func TestRunTaskRequiresATask(t *testing.T) {
	// Both this and the unsupported-transport case are isError results mentioning
	// the word "task", so the assertion has to be on which of the two conditions
	// is reported: "you omitted an argument" is fixable by the caller and "this
	// transport cannot ask you for approval" is not, and a caller told the wrong
	// one retries forever.
	for _, args := range []string{`{}`, `{"task":"   "}`, `{"task":""}`, ``} {
		line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"qeuro.run_task",` +
			argumentsField(args) + testMeta + `}}`
		text, isErr := resultText(t, only(t, mustResults(t, line)))
		if !isErr {
			t.Fatalf("args %q were accepted", args)
		}
		if !strings.Contains(text, "required") {
			t.Errorf("args %q were reported as %q, which does not say the argument is missing", args, text)
		}
		if strings.Contains(text, "approval channel") {
			t.Errorf("args %q were reported as an unsupported transport rather than a missing argument: %q", args, text)
		}
	}
}

// argumentsField renders the "arguments" member, or omits it entirely for the
// empty string — a caller that sends no arguments at all is a real case and must
// not produce a JSON syntax error.
func argumentsField(args string) string {
	if args == "" {
		return ""
	}
	return `"arguments":` + args + `,`
}

func TestOversizedResultsAreTruncated(t *testing.T) {
	// The cap lives in handleCall, so a tool that returns more than the limit must
	// be truncated on the way out regardless of which tool it was. None of ours can
	// produce that much from a temp directory, so the test registers one that does.
	const name = "qeuro.test_oversized"
	serverToolFuncs[name] = func(context.Context, json.RawMessage) (string, bool) {
		return strings.Repeat("x", maxResultBytes*3), false
	}
	t.Cleanup(func() { delete(serverToolFuncs, name) })

	// Asserted on the wire bytes, not through CallResult.Text(): that helper
	// truncates on the reading side too, so a result read through it looks bounded
	// even when the server sent everything. What matters here is what left the
	// process.
	r := only(t, mustResults(t, callReq(1, name, "{}")))
	if len(r.Result) > maxResultBytes+512 {
		t.Fatalf("the server wrote %d bytes, over the %d cap: nothing bounds what leaves the process",
			len(r.Result), maxResultBytes)
	}
	text, _ := resultText(t, r)
	if !strings.Contains(text, "truncated") {
		t.Errorf("the result was cut without saying so, which reads as a complete answer: %q", tailOf(text))
	}
}

func TestTruncationDoesNotSplitARune(t *testing.T) {
	// Cutting at a byte offset inside a multibyte character produces invalid UTF-8,
	// and json.Marshal silently replaces it — so the caller sees corruption with no
	// indication of where it came from.
	const name = "qeuro.test_multibyte"
	serverToolFuncs[name] = func(context.Context, json.RawMessage) (string, bool) {
		return strings.Repeat("日", maxResultBytes), false
	}
	t.Cleanup(func() { delete(serverToolFuncs, name) })

	text, _ := resultText(t, only(t, mustResults(t, callReq(1, name, "{}"))))
	if strings.ContainsRune(text, '�') {
		t.Fatal("truncation cut a rune in half")
	}
}

func TestPlanReportsTheRepositoryItRunsIn(t *testing.T) {
	dir := tempTree(t, map[string]string{
		"go.mod":       "module example\n",
		"package.json": "{}\n",
		"main.go":      "package main\n",
		"cmd/x.go":     "package cmd\n",
	})
	text, _ := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.plan", "{}"))))
	for _, want := range []string{"working directory:", dir, "top-level entries:", "main.go", "cmd/", "build files:", "go.mod", "package.json"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan output is missing %q:\n%s", want, text)
		}
	}
}

func TestPlanBoundsTheEntryListAndSaysSo(t *testing.T) {
	// A monorepo root can hold hundreds of entries. The listing exists to orient a
	// caller, not to be a complete inventory, and an unmarked cut would present a
	// partial tree as the whole one.
	files := map[string]string{"go.mod": "module example\n"}
	for i := 0; i < maxPlanEntries+40; i++ {
		files["pkg"+itoa(i)+"/x.go"] = "package x\n"
	}
	tempTree(t, files)

	text, _ := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.plan", "{}"))))
	entries := 0
	for _, l := range splitLines(text) {
		if strings.HasPrefix(l, "  pkg") {
			entries++
		}
	}
	if entries > maxPlanEntries {
		t.Errorf("plan listed %d entries, over the %d cap", entries, maxPlanEntries)
	}
	if !strings.Contains(text, "more") {
		t.Errorf("the omission is not reported, so a partial listing reads as complete:\n%s", tailOf(text))
	}
}

func TestPlanHidesDotfiles(t *testing.T) {
	// An .env is exactly the kind of name that must not be advertised to another
	// agent, and a layout summary does not need any dotfile to be useful.
	tempTree(t, map[string]string{
		"go.mod":              "module example\n",
		".env":                "QEURO_OPENROUTER_KEY=x\n",
		".npmrc":              "//registry:_authToken=x\n",
		".github/workflows/x": "on: push\n",
	})
	text, _ := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.plan", "{}"))))
	for _, leaked := range []string{".env", ".npmrc"} {
		if strings.Contains(text, leaked) {
			t.Errorf("plan output named %s:\n%s", leaked, text)
		}
	}
	if !strings.Contains(text, ".github") {
		t.Errorf("the one dotfile worth listing was dropped too:\n%s", text)
	}
}

func TestDiffNeverIncludesFileContents(t *testing.T) {
	// This is the load-bearing property of the tool: a half-finished credential
	// edit lives in a diff, and this server answers an agent we cannot audit.
	// The test therefore puts a secret-shaped string in the working tree and
	// asserts the tool reports the path without it.
	const secret = "sk-dummy-DO-NOT-LEAK-4f9a2b"
	dir := tempTree(t, map[string]string{
		"README.md":  "hello\n",
		"secret.env": "TOKEN=" + secret + "\n",
	})
	gitInit(t, dir)

	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.diff", "{}"))))
	if isErr {
		t.Fatalf("diff failed in a real git repository: %s", text)
	}
	if strings.Contains(text, secret) {
		t.Fatalf("diff output leaked file contents:\n%s", tailOf(text))
	}
	if !strings.Contains(text, "secret.env") {
		t.Fatalf("diff output does not name the changed file at all:\n%s", text)
	}
	for _, marker := range []string{"@@", "+++ ", "--- ", "diff --git"} {
		if strings.Contains(text, marker) {
			t.Errorf("diff output contains patch text (%q):\n%s", marker, tailOf(text))
		}
	}
}

func TestDiffReportsACleanTree(t *testing.T) {
	// "clean" and "I could not read the tree" must not look alike: one means stop
	// looking, the other means the answer is unknown.
	dir := tempTree(t, map[string]string{"README.md": "hello\n"})
	gitInit(t, dir)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "init")

	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.diff", "{}"))))
	if isErr {
		t.Fatalf("diff failed: %s", text)
	}
	if !strings.Contains(text, "clean") {
		t.Fatalf("a clean tree was not reported as clean: %q", text)
	}
}

func TestToolsOutsideARepositoryFailInsteadOfGuessing(t *testing.T) {
	// git is how both tools learn anything. Without it, diff has no answer and
	// must say so; plan still has a directory listing, so it degrades instead.
	tempTree(t, map[string]string{"note.txt": "x\n"})
	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.diff", "{}"))))
	if !isErr {
		t.Fatalf("diff claimed an answer outside a git repository: %q", text)
	}
	text, isErr = resultText(t, only(t, mustResults(t, callReq(1, "qeuro.plan", "{}"))))
	if isErr {
		t.Fatalf("plan failed although a directory listing was still possible: %q", text)
	}
	if !strings.Contains(text, "not a git repository") {
		t.Fatalf("plan did not say the branch is unknown: %q", text)
	}
}

func TestAPartialListSaysHowMuchWasOmitted(t *testing.T) {
	// A list cut at the cap with no marker is indistinguishable from a complete
	// one, and the caller then reasons about a repository that does not exist.
	files := map[string]string{"go.mod": "module example\n"}
	for i := 0; i < maxDiffEntries+25; i++ {
		files["f"+itoa(i)+".txt"] = "x\n"
	}
	dir := tempTree(t, files)
	gitInit(t, dir)

	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.diff", "{}"))))
	if isErr {
		t.Fatalf("diff failed: %s", text)
	}
	if !strings.Contains(text, "more") {
		t.Errorf("a truncated file list does not say anything was left out:\n%s", tailOf(text))
	}
	// The total is what makes the omission actionable: "226 changed paths, 200
	// shown" tells the caller to use its own tools, "200 changed paths" does not.
	if !strings.Contains(text, itoa(len(files))) {
		t.Errorf("the full count is not reported, so the caller cannot tell how much is missing:\n%s", tailOf(text))
	}
}

func TestResultsAreBounded(t *testing.T) {
	// A repository with a very large tree must not push a multi-megabyte message
	// into the caller's context window.
	for _, name := range []string{"qeuro.plan", "qeuro.diff"} {
		text, _ := resultText(t, only(t, mustResults(t, callReq(1, name, "{}"))))
		if len(text) > maxResultBytes {
			t.Errorf("%s returned %d bytes, over the %d cap", name, len(text), maxResultBytes)
		}
	}
}

func TestCostReportsWhatTheBackendReturns(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tier":"pro","credits_balance":12.5,"credits_total":100,"saved_usd_month":3}`))
	}))
	defer srv.Close()
	t.Setenv("QEURO_API_URL", srv.URL)
	t.Setenv("QEURO_TOKEN", "qeuro_live_dummy_test")

	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.cost", "{}"))))
	if isErr {
		t.Fatalf("cost failed against a healthy backend: %s", text)
	}
	if gotPath != "/v1/me" {
		t.Errorf("cost called %q; there is no other usage endpoint to call", gotPath)
	}
	if gotAuth != "Bearer qeuro_live_dummy_test" {
		t.Errorf("Authorization = %q, want the saved token as a bearer", gotAuth)
	}
	for _, want := range []string{"pro", "12.50"} {
		if !strings.Contains(text, want) {
			t.Errorf("cost output is missing %q:\n%s", want, text)
		}
	}
}

func TestCostNeverEchoesTheToken(t *testing.T) {
	// The result is handed to a third-party agent. The balance is the answer; the
	// credential that fetched it is not part of it.
	const token = "qeuro_live_dummy_secret_value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tier":"pro","credits_balance":1}`))
	}))
	defer srv.Close()
	t.Setenv("QEURO_API_URL", srv.URL)
	t.Setenv("QEURO_TOKEN", token)

	text, _ := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.cost", "{}"))))
	if strings.Contains(text, token) {
		t.Fatalf("cost output contains the API token:\n%s", text)
	}
}

func TestCostReportsAnUnreachableBackendAsAnExecutionError(t *testing.T) {
	// A closed port is a real condition for a laptop on a train, and the caller
	// can act on it. Reporting a zero balance instead would be a wrong answer
	// dressed as a right one.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	t.Setenv("QEURO_API_URL", url)
	t.Setenv("QEURO_TOKEN", "qeuro_live_dummy_test")

	text, isErr := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.cost", "{}"))))
	if !isErr {
		t.Fatalf("cost reported success with no backend reachable: %q", text)
	}
	if !strings.Contains(text, "cannot reach") {
		t.Fatalf("the failure does not say what went wrong: %q", text)
	}
}

func TestCostDoesNotInventUsageData(t *testing.T) {
	// There is no GET /v1/usage in the backend, so any per-day breakdown here
	// would be fabricated. Whether we are signed in or not, no such numbers may
	// appear.
	text, _ := resultText(t, only(t, mustResults(t, callReq(1, "qeuro.cost", "{}"))))
	for _, forbidden := range []string{"per-day breakdown:", "yesterday", "last 7 days:"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("cost output claims usage data the backend does not expose (%q):\n%s", forbidden, text)
		}
	}
}

// --- helpers --------------------------------------------------------------

// mustErrors runs the lines and asserts every response is a JSON-RPC error.
func mustErrors(t *testing.T, lines ...string) []response {
	t.Helper()
	rs, _ := serve(t, lines...)
	for _, r := range rs {
		if r.Error == nil {
			t.Fatalf("want an error response, got result %s", r.Result)
		}
	}
	return rs
}

// mustResults runs the lines and asserts every response carries a result.
func mustResults(t *testing.T, lines ...string) []response {
	t.Helper()
	rs, _ := serve(t, lines...)
	for _, r := range rs {
		if r.Error != nil {
			t.Fatalf("want a result, got error %d %q", r.Error.Code, r.Error.Message)
		}
	}
	return rs
}

// tempTree writes the given files into a fresh directory and makes it the
// process working directory for the duration of the test.
//
// The tools report on "the repository the CLI was started in", which means the
// process working directory is an input to them. Pointing it at a tree this test
// controls is what lets the interesting cases — a secret in an uncommitted file,
// no git at all — be asserted rather than hoped for. t.Chdir is process-wide, so
// these tests must not be parallel.
func tempTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	// t.TempDir may hand back a symlinked path (/var vs /private/var on macOS);
	// compare against what the tool will actually report.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A developer's global git config must not decide whether this test passes.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git %v is unavailable here: %v: %s", args, err, out)
	}
}

func tailOf(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
