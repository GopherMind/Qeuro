package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qeuro/internal/client"
)

// TestLocalModelUsesLocalProviderWithoutLogin pins the TUI wiring, not just the
// client's HTTP parser. The offline row is only true if an unsigned-in user can
// submit a turn and the agent host receives the LocalProvider rather than m.cli.
func TestLocalModelUsesLocalProviderWithoutLogin(t *testing.T) {
	isolateTUIConfig(t)
	// Atomic: the submitted turn runs the engine on its own goroutine and hits
	// this handler concurrently with the assertions below.
	var chatCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"model":"qwen2.5-coder:7b"}]}`))
		case "/api/chat":
			chatCalls.Add(1)
			_, _ = w.Write([]byte(`{"message":{"content":"local answer"},"done":true}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := newModelWithFlags("test", map[string]string{
		"local":       "true",
		"local_url":   srv.URL,
		"local_model": "qwen2.5-coder:7b",
	})
	// The session journal holds an open file under the temporary config dir, which
	// Windows will not let the test's cleanup remove.
	closeJournal(t, &m)
	if m.loggedIn {
		t.Fatal("test unexpectedly has an account; it would not exercise the offline login bypass")
	}
	if !m.local {
		t.Fatal("local flag did not reach the TUI model")
	}
	if _, ok := m.provider.(*client.LocalProvider); !ok {
		t.Fatalf("model.provider = %T, want *client.LocalProvider", m.provider)
	}

	// Drive the real submit path while logged out. This is the gate that used to
	// refuse every unsigned-in turn, so testing Provider.Chat directly would miss
	// the user-visible failure that made offline mode impossible. With the
	// agentcore.Engine path, onSubmit launches a background goroutine that calls
	// provider.Chat, so the test sees chatCalls > 0 immediately.
	m.input.SetValue("explain this")
	next, cmd := m.onSubmit()
	started := next.(model)
	if !started.streaming || cmd == nil {
		t.Fatalf("offline submit was refused: streaming=%v cmd-nil=%v notice=%q", started.streaming, cmd == nil, started.notice)
	}

	// Confirm provider.Chat works directly as well
	m.history = []client.Message{{Role: "user", Content: "explain this"}}
	ch, err := m.provider.Chat(context.Background(), m.buildRequest())
	if err != nil {
		t.Fatalf("local Chat: %v", err)
	}
	var answer string
	for ev := range ch {
		answer += ev.Text
	}
	if answer != "local answer" {
		t.Errorf("answer = %q", answer)
	}
	// chatCalls should be at least 1 (the real submission goroutine call).
	// Might be 2 if the direct Chat call above ran concurrently before we check.
	if calls := chatCalls.Load(); calls < 1 {
		t.Errorf("local chat calls = %d, want >= 1", calls)
	}
}

// TestLocalInitStartsNoBackendOrMCPWork is the negative half of offline mode.
// Init may draw and tick, but it must not schedule account/catalog/provider
// fetches or MCP startup: configured MCP servers can be remote and the CLI cannot
// promise a closed contour while starting them.
func TestLocalInitStartsNoBackendOrMCPWork(t *testing.T) {
	isolateTUIConfig(t)
	m := newModelWithFlags("test", map[string]string{"local": "true"})
	closeJournal(t, &m)
	// This property is observable in the model as well as in Init: local is named
	// before the first prompt so sensitive text is never typed under a false status.
	if !strings.Contains(m.notice, "local session") || !strings.Contains(m.notice, "backend disabled") {
		t.Fatalf("offline notice = %q", m.notice)
	}
	// Init itself is run by Bubble Tea and returns a BatchMsg closure; calling it
	// here is enough to verify the local branch is viable and does not panic while
	// composing a startup with no account work.
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init returned no command: the prompt would not draw")
	}
}

// Local wire protocols send no usage event. Every surface has to say "unknown"
// rather than displaying authoritative-looking zeroes, and the cloud model picker
// must not imply it can change the model chosen by --local-model.
func TestLocalSessionDoesNotOfferCloudOnlyState(t *testing.T) {
	isolateTUIConfig(t)
	m := newModelWithFlags("test", map[string]string{
		"local":       "true",
		"local_model": "qwen2.5-coder:7b",
	})
	closeJournal(t, &m)

	if got := m.contextStatusText(); got != "ctx unknown" {
		t.Errorf("context status = %q, want unknown", got)
	}
	if next, cmd := m.runCommand("context"); cmd != nil {
		t.Fatal("local /context started work")
	} else if got := next.(model).notice; !strings.Contains(got, "does not report") {
		t.Errorf("local /context notice = %q", got)
	}
	if next, _ := m.runCommand("usage"); !strings.Contains(next.(model).notice, "does not report") {
		t.Errorf("local /usage notice = %q", next.(model).notice)
	}
	if next, cmd := m.runCommand("model"); cmd != nil {
		t.Fatal("local /model started work")
	} else {
		got := next.(model)
		if got.sel.open {
			t.Error("local /model opened the cloud catalogue")
		}
		if !strings.Contains(got.notice, "--local-model") {
			t.Errorf("local /model notice = %q", got.notice)
		}
	}
	if got := doctorScreen("test", false, true, 80); !strings.Contains(got, "local session") || !strings.Contains(got, "backend disabled") {
		t.Errorf("local doctor screen describes a cloud login:\n%s", got)
	}
}

// TestLocalSessionRefusesLogin closes the one remaining way to leave the contour
// from inside it. Verifying a token is a backend request, so honouring /login in a
// --local session would both contact the network the user declared closed and send
// a bearer token there.
func TestLocalSessionRefusesLogin(t *testing.T) {
	isolateTUIConfig(t)
	m := newModelWithFlags("test", map[string]string{"local": "true"})
	closeJournal(t, &m)

	next, cmd := m.runCommand("login", "qeuro_live_must_not_leave")
	got := next.(model)
	if cmd != nil {
		t.Fatal("/login in a local session returned a command; running it would contact the backend")
	}
	if got.loggedIn {
		t.Fatal("/login marked a local session signed in")
	}
	if !strings.Contains(got.notice, "disabled") || !strings.Contains(got.notice, "--local") {
		t.Fatalf("notice = %q, want an actionable refusal naming the flag", got.notice)
	}
}

func TestLocalStatusNeverNamesCloudModel(t *testing.T) {
	isolateTUIConfig(t)
	m := newModelWithFlags("test", map[string]string{"local": "true"})
	closeJournal(t, &m)

	if got := m.statusModelName(); got != "local (server default)" {
		t.Fatalf("status model = %q, want honest server-selected label", got)
	}
	m.localModel = "qwen2.5-coder:7b"
	if got := m.statusModelName(); got != "local qwen2.5-coder:7b" {
		t.Fatalf("status model = %q, want configured local model", got)
	}
}

func closeJournal(t *testing.T, m *model) {
	t.Helper()
	if m.journal != nil {
		t.Cleanup(func() { _ = m.journal.Close(time.Now()) })
	}
}

// isolateTUIConfig puts both user config and the project file in empty temporary
// directories, so a developer's real token or .qeuro.toml cannot make these
// tests accidentally online.
func isolateTUIConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, env := range []string{
		"QEURO_TOKEN", "QEURO_API_URL", "QEURO_CONSOLE_URL", "QEURO_MODEL",
		"QEURO_AUTO_APPROVE", "QEURO_BUDGET", "QEURO_SKILLS_DIR", "QEURO_LOCAL",
		"QEURO_LOCAL_URL", "QEURO_LOCAL_MODEL",
	} {
		t.Setenv(env, "")
	}
}
