package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"qeuro/internal/clientcfg"
)

// Roadmap §8 "Startup" asks for «без сетевых вызовов на старте» — no network call
// before the prompt. Nothing kept that true: the probe these tests forbid was in
// front of the prompt for months, cost up to 1.5 s, and no test noticed. So the
// property is pinned by counting the effect (a dial) rather than the call site,
// which means a probe reintroduced anywhere on this path fails the test however
// it is written.

// isolateHome points the config layers at a temp dir so a developer's real
// config, and a real stored token, cannot decide the outcome of a test.
func isolateHome(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	// Both, as elsewhere in this suite: os.UserConfigDir reads %AppData% on
	// Windows and $XDG_CONFIG_HOME elsewhere, and the test binary runs on both.
	t.Setenv("AppData", d)
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("QEURO_TOKEN", "")
	t.Setenv("QEURO_API_URL", "")
	t.Setenv("QEURO_CONSOLE_URL", "")
	t.Setenv("QEURO_LOCAL", "")
	t.Setenv("QEURO_LOCAL_URL", "")
	t.Setenv("QEURO_LOCAL_MODEL", "")
	if clientcfg.ConfigDir() == "" {
		t.Skip("no config dir available on this platform")
	}
	return d
}

var errNoNetworkInTest = errors.New("network is not available in this test")

// countDials replaces the onboarding dialer, returning the counter. Every
// attempt is refused, so a test that accidentally reaches the network fails on
// the count rather than on the developer's local console answering.
func countDials(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	prev := onboardingDial
	onboardingDial = func(context.Context, string, string) (net.Conn, error) {
		n.Add(1)
		return nil, errNoNetworkInTest
	}
	t.Cleanup(func() { onboardingDial = prev })
	return &n
}

// captureBrowser replaces the browser launch, returning the URLs it was asked to
// open. Without it a passing test opens a browser window per run.
func captureBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	prev := openBrowserFn
	openBrowserFn = func(u string) error {
		opened = append(opened, u)
		return nil
	}
	t.Cleanup(func() { openBrowserFn = prev })
	return &opened
}

func TestFirstRunDoesNotProbeTheNetwork(t *testing.T) {
	isolateHome(t)
	dials := countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader("1\n"), &out)

	if n := dials.Load(); n != 0 {
		t.Errorf("first run opened %d connections; roadmap §8 requires none before the prompt", n)
	}
	// Laziness must not have become "silently does nothing": the prompt still
	// performs the action the user explicitly selected.
	if len(*opened) != 1 {
		t.Fatalf("registration opened %d times, want 1: %v", len(*opened), *opened)
	}
	if !strings.Contains(out.String(), "Create a Qeuro account") || !strings.Contains(out.String(), "Connect an AI provider") {
		t.Fatalf("first-run choice did not show both setup paths:\n%s", out.String())
	}
}

// OnboardingOpened came from the old auto-browser flow. It records only that a
// tab was opened, not that the user registered or connected a provider, so it
// must not suppress the new decision on a later signed-out launch.
func TestLegacyOnboardingFlagDoesNotSuppressSignedOutChoice(t *testing.T) {
	isolateHome(t)
	countDials(t)
	opened := captureBrowser(t)
	cfg, err := clientcfg.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OnboardingOpened = true
	if err := clientcfg.Save(cfg); err != nil {
		t.Fatal(err)
	}

	maybePromptFirstRun(strings.NewReader("1\n"), &bytes.Buffer{})

	if len(*opened) != 1 {
		t.Errorf("legacy flag suppressed the signed-out choice; registration opens = %d, want 1: %v", len(*opened), *opened)
	}
}

// Without the probe the configured console URL is authoritative, so it has to be
// honoured exactly — a wrong URL here sends a new user to a page that does not
// exist, which is worse than the latency the probe cost.
func TestFirstRunUsesTheConfiguredConsoleURL(t *testing.T) {
	isolateHome(t)
	t.Setenv("QEURO_CONSOLE_URL", "https://console.example.test")
	countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader("1\n"), &out)

	if len(*opened) != 1 {
		t.Fatalf("registration opened %d times, want 1: %v", len(*opened), *opened)
	}
	got := (*opened)[0]
	if !strings.HasPrefix(got, "https://console.example.test/register") {
		t.Errorf("opened %q, want the configured console URL", got)
	}
	if !strings.Contains(got, "from=cli") {
		t.Errorf("opened %q, want the from=cli attribution the console reads", got)
	}
}

// A trailing slash in the configured URL must not produce "//register": the
// console routes on the exact path, and this is the shape a user pasting from a
// browser address bar actually writes.
func TestFirstRunNormalisesATrailingSlash(t *testing.T) {
	isolateHome(t)
	t.Setenv("QEURO_CONSOLE_URL", "https://console.example.test/")
	countDials(t)
	opened := captureBrowser(t)

	maybePromptFirstRun(strings.NewReader("1\n"), &bytes.Buffer{})

	if len(*opened) != 1 {
		t.Fatalf("registration opened %d times, want 1: %v", len(*opened), *opened)
	}
	if got := (*opened)[0]; got != "https://console.example.test/register?from=cli" {
		t.Errorf("opened %q, want no doubled slash", got)
	}
}

// A signed-in user must not be sent to registration, and must not pay for the
// check either. This is the case the presence probe exists for: on Windows the
// token is a DPAPI file, so LoggedIn answers from a stat.
func TestFirstRunSkipsASignedInUser(t *testing.T) {
	isolateHome(t)
	t.Setenv("QEURO_TOKEN", "qeuro_live_env_token")
	dials := countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader("1\n"), &out)

	if len(*opened) != 0 {
		t.Errorf("registration opened for a signed-in user: %v", *opened)
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("signed-in startup opened %d connections, want 0", n)
	}
	if out.Len() != 0 {
		t.Errorf("signed-in startup rendered onboarding:\n%s", out.String())
	}
}

func TestFirstRunProviderChoiceOpensProviderSetup(t *testing.T) {
	isolateHome(t)
	t.Setenv("QEURO_CONSOLE_URL", "https://console.example.test/")
	dials := countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader("2\n"), &out)

	if n := dials.Load(); n != 0 {
		t.Fatalf("provider choice probed the network %d times", n)
	}
	if len(*opened) != 1 || (*opened)[0] != "https://console.example.test/providers?from=cli" {
		t.Fatalf("provider choice opened %v, want the configured Providers page", *opened)
	}
	if !strings.Contains(out.String(), "opened provider setup") || !strings.Contains(out.String(), "qeuro login <CLI_TOKEN>") {
		t.Fatalf("provider instructions are incomplete:\n%s", out.String())
	}
}

func TestFirstRunInvalidChoiceReprompts(t *testing.T) {
	isolateHome(t)
	countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader("banana\nbyok\n"), &out)

	if len(*opened) != 1 || !strings.Contains((*opened)[0], "/providers?") {
		t.Fatalf("invalid answer then byok opened %v", *opened)
	}
	if strings.Count(out.String(), "Choose 1 or 2") != 2 {
		t.Fatalf("invalid answer did not re-prompt exactly once:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "enter 1 to register or 2 to connect a provider") {
		t.Fatalf("invalid answer has no actionable error:\n%s", out.String())
	}
}

func TestFirstRunEOFPrintsBothURLsWithoutOpeningBrowser(t *testing.T) {
	isolateHome(t)
	t.Setenv("QEURO_CONSOLE_URL", "https://console.example.test")
	countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader(""), &out)

	if len(*opened) != 0 {
		t.Fatalf("EOF chose an action implicitly: %v", *opened)
	}
	for _, want := range []string{
		"https://console.example.test/register?from=cli",
		"https://console.example.test/providers?from=cli",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("EOF fallback missing %q:\n%s", want, out.String())
		}
	}
}

func TestFirstRunSkipsConfiguredLocalProvider(t *testing.T) {
	isolateHome(t)
	t.Setenv("QEURO_LOCAL", "1")
	dials := countDials(t)
	opened := captureBrowser(t)
	var out bytes.Buffer

	maybePromptFirstRun(strings.NewReader("1\n"), &out)

	if len(*opened) != 0 || out.Len() != 0 || dials.Load() != 0 {
		t.Fatalf("configured local provider entered cloud onboarding: opened=%v dials=%d out=%q", *opened, dials.Load(), out.String())
	}
}

func TestDefaultLaunchPromptsBeforeHooksAndTUI(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	prompt := strings.Index(text, "maybePromptFirstRun(os.Stdin, os.Stdout)")
	hook := strings.Index(text, "hooks.RunPreRunHook")
	tui := strings.Index(text, "runTUI()")
	if prompt < 0 || hook < 0 || tui < 0 || !(prompt < hook && hook < tui) {
		t.Fatalf("default launch order must be onboarding -> hook -> TUI; indexes prompt=%d hook=%d tui=%d", prompt, hook, tui)
	}
}

// The manual path is allowed to probe: `qeuro login` with no token is an explicit
// command that already blocks on a browser, and guessing a local dev port is
// where the probe earns its cost. Asserting this keeps the distinction real —
// otherwise a later cleanup would "simplify" the probe away entirely, or restore
// it on both paths.
func TestManualSignupStillProbes(t *testing.T) {
	isolateHome(t)
	dials := countDials(t)
	captureBrowser(t)

	openSignup(clientcfg.Config{ConsoleURL: "http://localhost:3000"}, true)

	if dials.Load() == 0 {
		t.Error("manual signup made no probe; the dev-port guess is its reason to exist")
	}
}

// A first run that cannot write its own config dir must still hand the user a
// CLI. The old order made this worse than it looks: the probe ran first, so a
// failure here also meant paying for it.
func TestFirstRunSurvivesAnUnwritableConfigDir(t *testing.T) {
	d := isolateHome(t)
	// A file where the directory has to be, so MkdirAll inside Save fails.
	if err := os.WriteFile(filepath.Join(d, "qeuro"), []byte("not a directory"), 0o600); err != nil {
		t.Skipf("could not stage an unwritable config dir: %v", err)
	}
	dials := countDials(t)
	captureBrowser(t)

	// The assertion is that this returns, without a panic and without a dial.
	maybePromptFirstRun(strings.NewReader("1\n"), &bytes.Buffer{})

	if n := dials.Load(); n != 0 {
		t.Errorf("broken-config startup opened %d connections, want 0", n)
	}
}
