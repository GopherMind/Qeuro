package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qeuro/internal/mcp"
)

// The tests below drive `qeuro mcp tools` and `qeuro mcp call` against a real
// child process rather than a stub, and the child is this test binary running
// our own `mcp serve` loop. That makes the pair a round trip: the command under
// test speaks to the server under test through actual pipes, so a framing or
// _meta mismatch between the two halves of §4.8 fails here instead of in a
// user's terminal.
//
// The mode is selected by an argv flag, not an environment variable. That is not
// a style choice: BaseEnv builds the child's environment from an allow-list and
// validateEnv rejects every QEURO_-prefixed key, so a QEURO_*-named marker is
// silently dropped by design (config.go's deniedEnvPrefixes) — and a child that
// does not recognise itself as a server runs the test suite instead, spawning
// more children. argv reaches the child untouched.
const (
	serveMarkerArg   = "-qeuro-mcp-serve"
	echoEnvArg       = "-qeuro-mcp-echo-env"
	serveDepthEnvVar = "MCP_SELF_TEST_DEPTH" // not QEURO_-prefixed, so it survives
)

func TestMain(m *testing.M) {
	var serve, echoEnv bool
	for _, a := range os.Args[1:] {
		switch a {
		case serveMarkerArg:
			serve = true
		case echoEnvArg:
			echoEnv = true
		}
	}
	if serve {
		if echoEnv {
			// A one-tool server whose result is its own environment. Nothing else can
			// answer "did that variable actually cross the process boundary".
			os.Exit(runEnvEchoServer())
		}
		os.Exit(mcp.Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	// A spawned child that reached this line did not recognise the marker. Running
	// the suite here is what turns one regression into a fork bomb, so the depth
	// sentinel costs one wasted process instead.
	if os.Getenv(serveDepthEnvVar) != "" {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// runEnvEchoServer is a minimal MCP server advertising one tool, "env", whose
// result is the environment the process was started with.
//
// It is hand-rolled rather than built from internal/mcp's own types so the wire
// format is written out here: a test that asserts a security property through the
// same code it is testing proves less than one that speaks the protocol directly.
func runEnvEchoServer() int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "server/discover":
			result = map[string]any{
				"supportedVersions": []string{mcp.ProtocolVersion},
				"capabilities":      map[string]any{"tools": map[string]any{}},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "env",
				"description": "report this process's environment",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			text := "no such tool"
			if req.Params.Name == "env" {
				text = strings.Join(os.Environ(), "\n")
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			result = map[string]any{}
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			return 1
		}
	}
	return 0
}

// writeMCPConfig points the CLI's config directory at a temp dir holding the
// given mcp.json, and returns that directory.
func writeMCPConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	// Both, because os.UserConfigDir reads a different variable per platform and
	// the test must not depend on which one this machine uses.
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	qeuroDir := filepath.Join(dir, "qeuro")
	if err := os.MkdirAll(qeuroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(qeuroDir, mcp.ConfigFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// mcp.LoadConfig warns when a stray mcp.json sits in the working directory.
	// Run from a clean one so that warning does not colour unrelated assertions.
	t.Chdir(t.TempDir())
	return qeuroDir
}

// selfServerConfig is an mcp.json whose single server is this test binary in
// serve mode.
func selfServerConfig(t *testing.T, allow []string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"servers": map[string]any{
			"self": map[string]any{
				"enabled":    true,
				"command":    exe,
				"args":       []string{serveMarkerArg},
				"env":        map[string]string{serveDepthEnvVar: "1"},
				"allowTools": allow,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type capture struct {
	out  strings.Builder
	err  strings.Builder
	code int
}

func (c *capture) text() string { return c.out.String() + c.err.String() }

func runList(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	c.code = mcpList(&c.out, &c.err)
	return c
}

func runTools(t *testing.T, args ...string) *capture {
	t.Helper()
	c := &capture{}
	c.code = mcpTools(&c.out, &c.err, args)
	return c
}

func runCall(t *testing.T, args ...string) *capture {
	t.Helper()
	c := &capture{}
	c.code = mcpCall(&c.out, &c.err, args)
	return c
}

// --- registration ---------------------------------------------------------

func TestMCPCommandIsRegistered(t *testing.T) {
	// An unregistered subcommand falls through to "unknown command", which reads
	// as a missing feature rather than a wiring mistake.
	var found *command
	for _, cmd := range commands() {
		if cmd.matches("mcp") {
			c := cmd
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal(`no "mcp" command registered: roadmap §4.8 requires "qeuro mcp"`)
	}
	if found.run == nil {
		t.Fatal(`"mcp" has a nil run func`)
	}
	if found.summary == "" {
		t.Error(`"mcp" has no summary, so it never appears in help`)
	}
	for _, sub := range []string{"list", "tools", "call", "serve"} {
		if !strings.Contains(found.usage, sub) {
			t.Errorf("usage does not mention the %q subcommand: %q", sub, found.usage)
		}
	}
}

// --- list -----------------------------------------------------------------

func TestListWorksWithoutStartingAnything(t *testing.T) {
	// The command must answer "what is configured" for a server that cannot run at
	// all, because that is exactly when the question gets asked.
	writeMCPConfig(t, `{"servers":{"broken":{"enabled":true,"command":"definitely-not-a-real-binary-xyz",
		"allowTools":["read"],"callsPerMinute":5}}}`)
	c := runList(t)
	if c.code != 0 {
		t.Fatalf("exit %d for a valid config: %s", c.code, c.text())
	}
	for _, want := range []string{"broken", "definitely-not-a-real-binary-xyz", "read", "5 calls/minute"} {
		if !strings.Contains(c.text(), want) {
			t.Errorf("list output is missing %q:\n%s", want, c.text())
		}
	}
}

func TestListNamesEnvFromWithoutReadingTheValues(t *testing.T) {
	// This output gets pasted into bug reports. envFrom entries name credentials,
	// so the names are the useful part and the values must never be resolved here.
	const secret = "ghp_dummy_not_a_real_token_1234567890"
	t.Setenv("MY_TOKEN", secret)
	writeMCPConfig(t, `{"servers":{"gh":{"enabled":true,"command":"echo","envFrom":["MY_TOKEN"],"allowTools":["x"]}}}`)
	c := runList(t)
	if !strings.Contains(c.text(), "MY_TOKEN") {
		t.Errorf("the env-from name is not shown, so a missing variable is undiagnosable:\n%s", c.text())
	}
	if strings.Contains(c.text(), secret) {
		t.Fatalf("list printed the value of an env-from variable:\n%s", c.text())
	}
}

func TestListReportsAnEmptyConfigAsEmptyNotAsFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Chdir(t.TempDir())
	c := runList(t)
	if c.code != 0 {
		t.Fatalf("exit %d with no mcp.json at all; most users have none: %s", c.code, c.text())
	}
	if !strings.Contains(c.text(), "no enabled MCP servers") {
		t.Errorf("the empty case is not explained:\n%s", c.text())
	}
}

func TestListExitsNonZeroOnAConfigWarning(t *testing.T) {
	// The non-zero status is the contract a script gates on: "is my MCP config
	// clean". A disabled-by-typo server that prints a warning and exits 0 is the
	// case that made this worth asserting.
	writeMCPConfig(t, `{"servers":{"a":{"enabled":true,"command":"echo","allowTools":[]}}}`)
	c := runList(t)
	if c.code == 0 {
		t.Fatalf("a server with an empty allow-list produced no warning status:\n%s", c.text())
	}
	if !strings.Contains(c.err.String(), "warning") {
		t.Errorf("the warning was not written to stderr:\n%s", c.text())
	}
}

func TestListAlwaysStatesTheApprovalPolicy(t *testing.T) {
	// The policy is the thing a user most needs to know before adding a server,
	// and it is not inferable from the file.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runList(t)
	if !strings.Contains(c.text(), "asks for approval") {
		t.Errorf("list does not state that every MCP call asks for approval:\n%s", c.text())
	}
}

func TestListShowsTheEndpointAndTheTransportForAURLServer(t *testing.T) {
	// A url entry has no command, so the first row would be blank without the
	// endpoint — which reads as a broken config rather than a server of the other
	// kind. And the transport is worth its own line: a stdio server runs here, an
	// HTTP one sends the tool arguments to another host.
	t.Setenv("TRACKER_TOKEN", "dummy-not-a-real-token")
	writeMCPConfig(t, `{"servers":{"tracker":{"enabled":true,"url":"https://mcp.example.com/mcp",
		"authFrom":"TRACKER_TOKEN","allowTools":["list_tickets"]}}}`)
	c := runList(t)
	if c.code != 0 {
		t.Fatalf("exit %d for a valid url server: %s", c.code, c.text())
	}
	for _, want := range []string{"https://mcp.example.com/mcp", "streamable http", "TRACKER_TOKEN", "list_tickets"} {
		if !strings.Contains(c.text(), want) {
			t.Errorf("list output is missing %q:\n%s", want, c.text())
		}
	}
	// The name of the variable, never its value — this output is pasted into issues.
	if strings.Contains(c.text(), "dummy-not-a-real-token") {
		t.Fatalf("list printed the bearer token:\n%s", c.text())
	}
}

func TestTransportLabelDistinguishesTheTwoTransports(t *testing.T) {
	stdio := mcp.ServerConfig{Command: "npx", Args: []string{"-y", "server"}}
	if got := transportLabel(stdio); !strings.Contains(got, "stdio") {
		t.Errorf("transportLabel(stdio) = %q", got)
	}
	if got := commandLine(stdio); got != "npx -y server" {
		t.Errorf("commandLine(stdio) = %q", got)
	}

	withToken := mcp.ServerConfig{URL: "https://h/mcp", AuthFrom: "TOK"}
	if got := transportLabel(withToken); !strings.Contains(got, "TOK") || !strings.Contains(got, "http") {
		t.Errorf("transportLabel(url with token) = %q", got)
	}
	// "no token" has to be visible: an unauthenticated remote server is a thing the
	// user should notice in the listing rather than discover from a 401.
	if got := transportLabel(mcp.ServerConfig{URL: "https://h/mcp"}); !strings.Contains(got, "no token") {
		t.Errorf("transportLabel(url without token) = %q", got)
	}
	if got := commandLine(mcp.ServerConfig{URL: "https://h/mcp"}); got != "https://h/mcp" {
		t.Errorf("commandLine(url) = %q", got)
	}
}

// --- tools ----------------------------------------------------------------

func TestToolsListsTheServersRealToolSet(t *testing.T) {
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runTools(t, "self")
	if c.code != 0 {
		t.Fatalf("exit %d: %s", c.code, c.text())
	}
	// Every tool our own server advertises must appear, allowed or not.
	for _, want := range []string{"qeuro.plan", "qeuro.diff", "qeuro.cost", "qeuro.run_task"} {
		if !strings.Contains(c.out.String(), want) {
			t.Errorf("tool %q is missing from the listing:\n%s", want, c.out.String())
		}
	}
}

func TestToolsShowsBlockedToolsAndWhy(t *testing.T) {
	// "Why can the model not use this tool" is the question, and the two answers —
	// misspelled name, or filtered by the allow-list — are only distinguishable if
	// the filtered ones are still printed.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runTools(t, "self")
	if !strings.Contains(c.out.String(), "not in allowTools") {
		t.Errorf("blocked tools are not marked as blocked:\n%s", c.out.String())
	}
	if !strings.Contains(c.out.String(), "mcp__self__qeuro.plan") {
		t.Errorf("the allowed tool's callable name is not shown:\n%s", c.out.String())
	}
}

func TestToolsNamesTheLocalPrefixNotTheServersOwnName(t *testing.T) {
	// The model addresses tools as mcp__<local name>__<tool>. A listing that
	// showed the server's self-reported name as the prefix would hand the model a
	// name the gate rejects.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.cost"}))
	c := runTools(t, "self")
	if strings.Contains(c.out.String(), "mcp__qeuro-cli__") {
		t.Fatalf("the listing used the server's self-reported name as the prefix:\n%s", c.out.String())
	}
}

// TestToolsAndCallWorkOverHTTP: these two commands choose a transport, and a
// command that only knows about stdio is the failure this closes — an HTTP server
// that works in chat and is absent from `qeuro mcp tools` (or the reverse) would
// make the diagnostic commands lie about the configuration.
func TestToolsAndCallWorkOverHTTP(t *testing.T) {
	const payload = "ticket 41: the login button is blue"
	var sawBearer string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			sawBearer = a
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(body, &req)

		var result any
		switch req.Method {
		case "server/discover":
			result = map[string]any{
				"supportedVersions": []string{mcp.ProtocolVersion},
				"capabilities":      map[string]any{"tools": map[string]any{}},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "list_tickets", "description": "List tickets.", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "close_ticket", "description": "Close one.", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			text := "no such tool"
			if req.Params.Name == "list_tickets" {
				text = payload
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()

	t.Setenv("TRACKER_TOKEN", "dummy-bearer-value")
	cfg, err := json.Marshal(map[string]any{
		"servers": map[string]any{
			"tracker": map[string]any{
				"enabled":    true,
				"url":        srv.URL, // loopback, so cleartext http is allowed
				"authFrom":   "TRACKER_TOKEN",
				"allowTools": []string{"list_tickets"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeMCPConfig(t, string(cfg))

	listed := runTools(t, "tracker")
	if listed.code != 0 {
		t.Fatalf("mcp tools over http exited %d: %s", listed.code, listed.text())
	}
	if !strings.Contains(listed.out.String(), "list_tickets") {
		t.Errorf("the allowed tool is missing from the listing:\n%s", listed.out.String())
	}
	if !strings.Contains(listed.out.String(), "not in allowTools") {
		t.Errorf("the blocked tool is not marked as blocked:\n%s", listed.out.String())
	}

	call := runCall(t, "tracker", "list_tickets", "{}")
	if call.code != 0 {
		t.Fatalf("mcp call over http exited %d: %s", call.code, call.text())
	}
	if !strings.Contains(call.out.String(), payload) {
		t.Errorf("the remote result did not reach stdout:\n%s", call.out.String())
	}
	if !strings.Contains(call.out.String(), "UNTRUSTED") {
		t.Errorf("a remote result was printed outside the fence:\n%s", call.out.String())
	}
	// The token came from the named variable rather than from the file, which is the
	// whole point of authFrom.
	if sawBearer != "Bearer dummy-bearer-value" {
		t.Errorf("Authorization header = %q, want the token from TRACKER_TOKEN", sawBearer)
	}

	// And the allow-list still applies to a human caller over HTTP.
	blocked := runCall(t, "tracker", "close_ticket", "{}")
	if blocked.code == 0 {
		t.Fatalf("a tool outside allowTools was called over http:\n%s", blocked.text())
	}
}

func TestToolsRefusesAnUnknownServer(t *testing.T) {
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runTools(t, "nope")
	if c.code == 0 {
		t.Fatalf("an unknown server name exited 0:\n%s", c.text())
	}
	if !strings.Contains(c.text(), "self") {
		t.Errorf("the error does not list what is configured:\n%s", c.text())
	}
}

func TestToolsSanitisesTheServerNameItEchoes(t *testing.T) {
	// The name comes from argv and lands in a terminal. Control characters would
	// let it repaint the line it is reported on.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runTools(t, "evil\x1b[2Kname\nsecond line")
	if strings.ContainsAny(c.text(), "\x1b") {
		t.Fatalf("an escape sequence from argv reached the terminal:\n%q", c.text())
	}
}

func TestToolsRequiresAServerArgument(t *testing.T) {
	c := runTools(t)
	if c.code != 2 {
		t.Errorf("exit %d for a missing argument, want 2 (usage error)", c.code)
	}
	if !strings.Contains(c.err.String(), "usage") {
		t.Errorf("no usage line on a usage error:\n%s", c.text())
	}
}

func TestToolsReportsAServerThatCannotStart(t *testing.T) {
	writeMCPConfig(t, `{"servers":{"broken":{"enabled":true,"command":"definitely-not-a-real-binary-xyz","allowTools":["x"]}}}`)
	c := runTools(t, "broken")
	if c.code == 0 {
		t.Fatal("a server that cannot start exited 0")
	}
	if !strings.Contains(c.err.String(), "cannot start") && !strings.Contains(c.err.String(), "cannot connect") {
		t.Errorf("the failure is not explained:\n%s", c.text())
	}
}

// --- call -----------------------------------------------------------------

func TestCallRoundTripsThroughARealProcess(t *testing.T) {
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runCall(t, "self", "qeuro.plan", "{}")
	if c.code != 0 {
		t.Fatalf("exit %d: %s", c.code, c.text())
	}
	if !strings.Contains(c.out.String(), "working directory:") {
		t.Errorf("the tool result did not reach stdout:\n%s", c.out.String())
	}
}

func TestCallPrintsTheResultInsideTheSameFenceTheModelWouldSee(t *testing.T) {
	// The value of this command is that a human and the model inspect the same
	// artefact. A bare print here would hide a payload that tries to pass itself
	// off as an instruction — the exact thing the fence makes visible.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runCall(t, "self", "qeuro.plan", "{}")
	out := c.out.String()
	if !strings.Contains(out, "UNTRUSTED") {
		t.Fatalf("the result was printed outside the untrusted fence:\n%s", out)
	}
	if !strings.Contains(out, "mcp:self") {
		t.Errorf("the fence does not record which server produced the text:\n%s", out)
	}
}

func TestCallEnforcesTheAllowListForAHumanCallerToo(t *testing.T) {
	// A human typing the command is an approval, but it is not a change to the
	// declared surface: if this bypassed allowTools, mcp.json would stop
	// describing what the server can be asked to do.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runCall(t, "self", "qeuro.cost", "{}")
	if c.code == 0 {
		t.Fatalf("a tool outside allowTools was called:\n%s", c.text())
	}
	if !strings.Contains(c.err.String(), "not in allowTools") {
		t.Errorf("the refusal does not say why:\n%s", c.text())
	}
	if strings.Contains(c.out.String(), "credits") {
		t.Fatalf("the blocked tool still produced output:\n%s", c.out.String())
	}
}

func TestCallRejectsInvalidJSONBeforeStartingAServer(t *testing.T) {
	// Validating first means a typo in the payload does not spawn a process, and
	// the message says "your JSON" rather than whatever the server made of it.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runCall(t, "self", "qeuro.plan", "{not json")
	if c.code != 2 {
		t.Errorf("exit %d for malformed arguments, want 2 (usage error)", c.code)
	}
	if !strings.Contains(c.err.String(), "not valid JSON") {
		t.Errorf("the message does not point at the arguments:\n%s", c.text())
	}
}

func TestCallDefaultsToAnEmptyObject(t *testing.T) {
	// Every tool of ours takes no arguments; requiring `{}` on the command line
	// would be ceremony.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runCall(t, "self", "qeuro.plan")
	if c.code != 0 {
		t.Fatalf("exit %d with no argument payload: %s", c.code, c.text())
	}
}

func TestCallExitsNonZeroOnAnExecutionError(t *testing.T) {
	// isError is a result, not a transport failure — but a shell caller only
	// notices a status code.
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.run_task"}))
	c := runCall(t, "self", "qeuro.run_task", `{"task":"x"}`)
	if c.code == 0 {
		t.Fatalf("an isError result exited 0:\n%s", c.text())
	}
	if !strings.Contains(c.out.String(), "approval channel") {
		t.Errorf("the tool's own message was not shown:\n%s", c.out.String())
	}
	if !strings.Contains(c.err.String(), "execution error") {
		t.Errorf("stderr does not distinguish an execution error from a crash:\n%s", c.err.String())
	}
}

func TestCallRefusesAnUnknownServer(t *testing.T) {
	writeMCPConfig(t, selfServerConfig(t, []string{"qeuro.plan"}))
	c := runCall(t, "ghost", "qeuro.plan", "{}")
	if c.code == 0 {
		t.Fatalf("an unknown server exited 0:\n%s", c.text())
	}
}

func TestCallRequiresAServerAndATool(t *testing.T) {
	for _, args := range [][]string{{}, {"self"}} {
		c := runCall(t, args...)
		if c.code != 2 {
			t.Errorf("args %v exited %d, want 2 (usage error)", args, c.code)
		}
	}
}

func TestProviderCredentialsNeverReachAServerStartedByThisCommand(t *testing.T) {
	// `qeuro mcp tools` and `qeuro mcp call` spawn the server themselves, so they
	// are a second place the "no provider keys in an MCP process" rule can be
	// broken (.ai/AI.md:49, roadmap.txt:333). The child here reports its own
	// environment, so the assertion is about what actually crossed the process
	// boundary rather than about what the config said.
	// "dummy" is in the value on purpose: this is a token-shaped literal in tracked
	// source, and .gitleaks.toml's stopword list is what keeps the secret scan green
	// without a per-line waiver.
	const leak = "sk-dummy-provider-key-must-not-cross"
	t.Setenv("QEURO_OPENROUTER_KEY", leak)
	t.Setenv("OPENAI_API_KEY", leak)
	t.Setenv("MY_TOKEN", "harmless-but-requested")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(map[string]any{
		"servers": map[string]any{
			"self": map[string]any{
				"enabled": true,
				"command": exe,
				"args":    []string{serveMarkerArg, echoEnvArg},
				"env":     map[string]string{serveDepthEnvVar: "1"},
				// Asking for them explicitly is the strongest version of the test: the
				// user's own file requests the keys, and they must still not arrive.
				"envFrom":    []string{"QEURO_OPENROUTER_KEY", "OPENAI_API_KEY", "MY_TOKEN"},
				"allowTools": []string{"env"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeMCPConfig(t, string(cfg))

	c := runCall(t, "self", "env", "{}")
	if c.code != 0 {
		t.Fatalf("exit %d: %s", c.code, c.text())
	}
	if strings.Contains(c.out.String(), leak) {
		t.Fatalf("a provider credential reached the server process:\n%s", c.out.String())
	}
	if !strings.Contains(c.out.String(), "MY_TOKEN") {
		t.Errorf("the server's own requested variable did not arrive, so this test proves nothing:\n%s", c.out.String())
	}
	if !strings.Contains(c.err.String(), "never passed to an MCP server") {
		t.Errorf("the user was not told their envFrom entries were dropped:\n%s", c.err.String())
	}
}

// --- helpers --------------------------------------------------------------

func TestLimitTextSpellsOutUnlimited(t *testing.T) {
	// A negative callsPerMinute is the documented way to say "no limit". Printing
	// it as "-1 calls/minute" would read as a broken config.
	if got := limitText(0); got != "unlimited" {
		t.Errorf("limitText(0) = %q", got)
	}
	if got := limitText(30); !strings.Contains(got, "30") {
		t.Errorf("limitText(30) = %q", got)
	}
}

func TestOneLineStripsControlCharacters(t *testing.T) {
	got := oneLine("a\x1b[31mb\nc\td\x00e")
	for _, bad := range []string{"\x1b", "\n", "\x00"} {
		if strings.Contains(got, bad) {
			t.Errorf("oneLine kept %q: %q", bad, got)
		}
	}
}

func TestClipKeepsValidUTF8(t *testing.T) {
	// Cutting mid-rune would emit replacement characters into the terminal, and
	// server-supplied names are where multibyte text arrives.
	got := clip("日本語のツール名です", 7)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clip did not mark the truncation: %q", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("clip cut mid-rune: %q", got)
	}
}
