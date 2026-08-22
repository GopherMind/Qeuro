package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"

	"qeuro/internal/client"
	"qeuro/internal/state"
)

var errTest = errors.New("boom")

func newAccountTestModel() model {
	in := textarea.New()
	in.SetWidth(72)
	return model{
		app:        state.New(),
		width:      80,
		input:      in,
		baseURL:    "http://backend.invalid",
		consoleURL: "http://console.invalid",
	}
}

func TestLoginWithoutTokenOpensInstructions(t *testing.T) {
	m := newAccountTestModel()
	next, cmd := m.runCommand("login")
	nm := next.(model)
	if cmd != nil {
		t.Fatalf("bare /login must not fire an async command")
	}
	if nm.infoView == "" || !strings.Contains(nm.infoView, "/login") {
		t.Fatalf("bare /login should open sign-in instructions mentioning /login <token>")
	}
}

func TestLoginWithTokenStartsVerification(t *testing.T) {
	m := newAccountTestModel()
	next, cmd := m.runCommand("login", "qeuro_live_test_token")
	nm := next.(model)
	if cmd == nil {
		t.Fatalf("/login <token> must start async verification")
	}
	if nm.notice == "" {
		t.Fatalf("/login <token> should show a progress notice")
	}
}

func TestLoginDoneUpdatesSession(t *testing.T) {
	m := newAccountTestModel()
	me := client.MeResponse{Tier: "pro"}
	next, cmd := m.Update(loginDoneMsg{token: "qeuro_live_test_token", me: &me})
	nm := next.(model)
	if !nm.loggedIn || nm.cli == nil {
		t.Fatalf("successful login must mark the session as signed in")
	}
	if nm.app.Conn != state.Online {
		t.Fatalf("connection state should flip to online after login")
	}
	if cmd == nil {
		t.Fatalf("login should refresh account info and providers")
	}
	if !strings.Contains(nm.notice, "pro") {
		t.Fatalf("login notice should mention the plan, got %q", nm.notice)
	}
}

func TestLoginFailureKeepsSignedOut(t *testing.T) {
	m := newAccountTestModel()
	next, _ := m.Update(loginDoneMsg{err: errTest})
	nm := next.(model)
	if nm.loggedIn {
		t.Fatalf("failed login must not mark the session as signed in")
	}
	if !strings.Contains(nm.notice, "login failed") {
		t.Fatalf("failed login should surface an error notice, got %q", nm.notice)
	}
}

func TestLogoutWithoutSessionIsNoop(t *testing.T) {
	m := newAccountTestModel()
	next, cmd := m.runCommand("logout")
	nm := next.(model)
	if cmd != nil {
		t.Fatalf("/logout while signed out must not fire an async command")
	}
	if !strings.Contains(nm.notice, "/login") {
		t.Fatalf("/logout while signed out should point at /login, got %q", nm.notice)
	}
}

func TestLogoutClearsSessionState(t *testing.T) {
	m := newAccountTestModel()
	m.loggedIn = true
	m.cli = client.New("http://backend.invalid", "tok")
	m.providers = []client.ProviderConfig{
		{Name: "My OpenRouter", Provider: "openrouter", Kind: "chat", Enabled: true},
	}
	next, cmd := m.runCommand("logout")
	nm := next.(model)
	if nm.loggedIn || nm.providers != nil {
		t.Fatalf("/logout must clear the session and linked providers immediately")
	}
	if nm.app.Conn != state.Offline {
		t.Fatalf("/logout should flip the connection state to offline")
	}
	if cmd == nil {
		t.Fatalf("/logout should fire the async revoke+persist command")
	}
}

func TestProvidersRequiresSession(t *testing.T) {
	m := newAccountTestModel()
	next, cmd := m.runCommand("providers")
	nm := next.(model)
	if cmd != nil || !strings.Contains(nm.notice, "/login") {
		t.Fatalf("/providers while signed out should point at /login")
	}
}

func TestProvidersMsgOpensPanelAndAttachesToRequests(t *testing.T) {
	m := newAccountTestModel()
	m.loggedIn = true
	list := []client.ProviderConfig{
		{Name: "My OpenRouter", Provider: "openrouter", Kind: "chat", Enabled: true},
		{Name: "Local Ollama", Provider: "ollama", Kind: "chat", Enabled: false},
	}
	next, _ := m.Update(providersMsg{providers: list})
	nm := next.(model)
	if len(nm.providers) != 2 {
		t.Fatalf("providers from the console must be kept on the model, got %d", len(nm.providers))
	}
	if nm.infoView == "" || !strings.Contains(nm.infoView, "OpenRouter") {
		t.Fatalf("/providers should render the linked provider list")
	}
}

func TestQuietProviderSyncStaysSilent(t *testing.T) {
	m := newAccountTestModel()
	m.loggedIn = true
	next, _ := m.Update(providersMsg{quiet: true, err: errTest})
	nm := next.(model)
	if nm.notice != "" || nm.infoView != "" {
		t.Fatalf("startup provider sync failures must stay silent")
	}
}
