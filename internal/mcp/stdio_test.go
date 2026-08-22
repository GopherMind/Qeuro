package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStdioCallRoundTrip(t *testing.T) {
	tr := fakeStdio(t, modeNormal)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	raw, err := tr.Call(ctx, MethodToolsList, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var res ToolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("got %d tools, want 2: %+v", len(res.Tools), res.Tools)
	}
}

// TestStdioSendsRequiredMeta pins the one protocol requirement that is easy to
// get wrong and impossible to notice locally: in revision 2026-07-28 there is no
// initialize handshake, so protocolVersion and clientCapabilities must ride on
// every single request. Omitting either is InvalidParams from a strict server.
func TestStdioSendsRequiredMeta(t *testing.T) {
	raw, err := withMeta(map[string]any{"name": "x"})
	if err != nil {
		t.Fatalf("withMeta: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	meta, ok := got["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing: %s", raw)
	}
	if meta[metaProtocolVersion] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", meta[metaProtocolVersion], ProtocolVersion)
	}
	if _, ok := meta[metaClientCapabilities]; !ok {
		t.Error("clientCapabilities missing: a strict server answers -32602")
	}
	if got["name"] != "x" {
		t.Error("withMeta dropped the caller's params")
	}
	// A caller cannot override _meta: it is set last.
	raw2, err := withMeta(map[string]any{"_meta": map[string]any{"forged": true}})
	if err != nil {
		t.Fatalf("withMeta: %v", err)
	}
	var got2 map[string]any
	_ = json.Unmarshal(raw2, &got2)
	m2, _ := got2["_meta"].(map[string]any)
	if m2["forged"] != nil || m2[metaProtocolVersion] != ProtocolVersion {
		t.Errorf("caller overrode _meta: %s", raw2)
	}
}

// TestStdioIgnoresNonMCPStdout: servers do write banners and npm warnings to
// stdout despite the specification forbidding it. One bad line must not kill the
// session.
func TestStdioIgnoresNonMCPStdout(t *testing.T) {
	tr := fakeStdio(t, modeJunkStdout)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := tr.Call(ctx, MethodToolsList, nil); err != nil {
		t.Fatalf("junk on stdout broke the session: %v", err)
	}
}

// TestStdioCallTimesOutOnSilentServer: the failure mode that matters most in
// practice. A server that accepts the request and never answers must not hang the
// CLI.
func TestStdioCallTimesOutOnSilentServer(t *testing.T) {
	tr := fakeStdio(t, modeSilent)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tr.Call(ctx, MethodToolsList, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("call took %v: the deadline was not honoured", elapsed)
	}
}

// TestStdioMismatchedResponseIDIsIgnored: a response carrying an ID nobody waits
// on must be dropped, not mismatched onto a live call. Without the ID check a
// stray response would resolve an unrelated request with the wrong payload.
func TestStdioMismatchedResponseIDIsIgnored(t *testing.T) {
	tr := fakeStdio(t, modeBadID)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := tr.Call(ctx, MethodToolsList, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded (the answer had a foreign ID)", err)
	}
}

// TestStdioServerDeathFailsWaitingCall is the defect this transport was written
// to avoid: agentcore/host.go:23 never checks sc.Err() and never closes its
// channels, so a consumer waits forever on a dead child. Here the waiter must get
// an error.
func TestStdioServerDeathFailsWaitingCall(t *testing.T) {
	tr := fakeStdio(t, modeCrashOnCall)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := tr.Call(ctx, MethodToolsCall, map[string]any{"name": "x"})
	if err == nil {
		t.Fatal("call succeeded against a server that exited")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call hung until the deadline instead of failing on EOF: %v", err)
	}
	// Subsequent calls fail fast rather than waiting again.
	if _, err := tr.Call(context.Background(), MethodToolsList, nil); err == nil {
		t.Error("a call on a dead transport succeeded")
	}
}

// TestStdioLargeResponse: bufio's default 64 KiB limit would break tools/list for
// any server with real JSON Schemas.
func TestStdioLargeResponse(t *testing.T) {
	tr := fakeStdio(t, modeGiantList)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	raw, err := tr.Call(ctx, MethodToolsList, nil)
	if err != nil {
		t.Fatalf("large tools/list failed: %v", err)
	}
	if len(raw) < 64<<10 {
		t.Fatalf("response was only %d bytes; the test no longer exercises the limit", len(raw))
	}
	var res ToolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Tools) != 40 {
		t.Errorf("got %d tools, want 40", len(res.Tools))
	}
}

func TestStdioServerErrorIsReturnedTyped(t *testing.T) {
	tr := fakeStdio(t, modeVersionError)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := tr.Call(ctx, MethodDiscover, nil)
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v (%T), want *rpcError", err, err)
	}
	if rpcErr.Code != CodeUnsupportedProtocolVersion {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeUnsupportedProtocolVersion)
	}
	if got := rpcErr.supportedVersions(); len(got) != 2 || got[0] != ProtocolVersion {
		t.Errorf("supportedVersions = %v", got)
	}
	// The message is server-authored text; it must be reported as a quoted server
	// error and never as an instruction.
	if !strings.Contains(rpcErr.Error(), "mcp: server error") {
		t.Errorf("error text = %q", rpcErr.Error())
	}
}

// TestStdioStderrIsDiagnosticNotFailure: the specification says a client SHOULD
// NOT treat stderr output as an error. Servers log there routinely.
func TestStdioStderrIsDiagnosticNotFailure(t *testing.T) {
	tr := fakeStdio(t, modeStderrChatty)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := tr.Call(ctx, MethodToolsList, nil); err != nil {
		t.Fatalf("stderr output was treated as failure: %v", err)
	}
	// Give the child a moment to flush, then confirm the log was retained for
	// diagnostics.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && tr.Diagnostics() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(tr.Diagnostics(), "listening on stdio") {
		t.Errorf("stderr not captured: %q", tr.Diagnostics())
	}
}

// TestStdioChildEnvironmentExcludesProviderKeys is the direct test of the
// promise in roadmap §4.8: a third-party server process never sees provider
// credentials. It works by construction (the environment is built from an
// allow-list, not filtered), and this asserts it end to end by having the server
// report its own environment.
func TestStdioChildEnvironmentExcludesProviderKeys(t *testing.T) {
	// Every value carries "dummy" deliberately. These are token-shaped literals in
	// tracked source, and .gitleaks.toml's stopword list is what keeps them from
	// failing the repository secret scan — a fingerprint in .gitleaksignore could
	// not waive them, because those are bound to a commit, path and line.
	secrets := map[string]string{
		"QEURO_TOKEN":           "qeuro_live_dummy_must_not_leak",
		"QEURO_OPENROUTER_KEY":  "sk-or-dummy-must-not-leak",
		"QEURO_NVIDIA_API_KEY":  "nvapi-dummy-must-not-leak",
		"QEURO_ADMIN_PASSWORD":  "dummy-must-not-leak",
		"STRIPE_SECRET_KEY":     "sk_live_dummy_must_not_leak",
		"AWS_SECRET_ACCESS_KEY": "dummy-must-not-leak",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	tr := fakeStdio(t, modeEchoEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	raw, err := tr.Call(ctx, MethodToolsCall, map[string]any{"name": "echo"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	childEnv, _ := res.Text()
	for name := range secrets {
		if strings.Contains(childEnv, name) {
			t.Errorf("%s reached the server process", name)
		}
	}
	// Sanity: the child did report an environment, so an empty result is not what
	// made the assertions pass.
	if !strings.Contains(childEnv, fakeServerEnv) {
		t.Fatalf("the server did not report its environment: %q", childEnv)
	}
}

func TestBaseEnvAllowListOnly(t *testing.T) {
	t.Setenv("QEURO_OPENROUTER_KEY", "sk-or-dummy-leak")
	t.Setenv("PATH", "/usr/bin")
	env := BaseEnv(map[string]string{"GITHUB_TOKEN": "ghp_dummy"})

	var hasPath, hasToken bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "QEURO_") {
			t.Errorf("provider variable leaked into child env: %s", strings.SplitN(kv, "=", 2)[0])
		}
		if kv == "PATH=/usr/bin" {
			hasPath = true
		}
		if kv == "GITHUB_TOKEN=ghp_dummy" {
			hasToken = true
		}
	}
	if !hasPath {
		t.Error("PATH missing: the server would not find its own interpreter")
	}
	if !hasToken {
		t.Error("envFrom value missing: the server would have no credential")
	}
}

// TestBaseEnvExtraDoesNotDuplicateAllowListed: an envFrom entry naming an
// allow-listed variable must override it, not appear twice — which of two
// duplicate entries wins is platform-dependent.
func TestBaseEnvExtraDoesNotDuplicateAllowListed(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := BaseEnv(map[string]string{"PATH": "/opt/custom/bin"})
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			count++
			if kv != "PATH=/opt/custom/bin" {
				t.Errorf("PATH = %q, want the override", kv)
			}
		}
	}
	if count != 1 {
		t.Errorf("PATH appears %d times, want 1", count)
	}
}

func TestStdioRejectsEmptyCommand(t *testing.T) {
	for _, cmd := range []string{"", "   "} {
		if _, err := StartStdio(StdioConfig{Command: cmd}); err == nil {
			t.Errorf("StartStdio(%q) succeeded", cmd)
		}
	}
}

func TestStdioCloseIsIdempotent(t *testing.T) {
	tr := fakeStdio(t, modeNormal)
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Close runs on every session teardown and on error paths; a second call must
	// not panic on a closed channel or a nil process.
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := tr.Call(context.Background(), MethodToolsList, nil); err == nil {
		t.Error("Call succeeded after Close")
	}
	if err := tr.Notify(context.Background(), MethodCancelled, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Notify after Close = %v, want ErrClosed", err)
	}
}

// TestStdioNotifyGetsNoResponse: notifications carry no ID and must not be
// answered. If the client allocated an ID for one, the reply would be delivered
// to a waiter that does not exist, or worse, to the next call's ID.
func TestStdioNotifyGetsNoResponse(t *testing.T) {
	tr := fakeStdio(t, modeNormal)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tr.Notify(ctx, MethodCancelled, map[string]any{"requestId": 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// The next call must still get its own answer.
	if _, err := tr.Call(ctx, MethodToolsList, nil); err != nil {
		t.Fatalf("call after notify: %v", err)
	}
}

func TestStdioConcurrentCalls(t *testing.T) {
	tr := fakeStdio(t, modeNormal)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const n = 12
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := tr.Call(ctx, MethodToolsList, nil)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent call %d: %v", i, err)
		}
	}
}
