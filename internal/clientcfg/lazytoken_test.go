package clientcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qeuro/internal/client"
)

// Roadmap §8 "Startup" asks for lazy keyring initialisation. Everything below is
// about one property: nothing on the path to the prompt may consult the OS secret
// store, and every caller that genuinely needs the secret must still get it.
//
// The tests are written against behaviour rather than against the shape of the
// code, so the same file characterises the eager version and the lazy one. That
// is deliberate: a refactor whose tests were written afterwards proves only that
// the new code does what the new code does.

// countingStore replaces the platform secret store for one test and records how
// many times it was read.
type countingStore struct {
	token string
	err   error
	reads int
}

func (c *countingStore) load() (string, error) {
	c.reads++
	return c.token, c.err
}

// withStore installs a fake secret store and an isolated config dir, returning
// the store so a test can assert on read counts.
//
// Both seams are replaced together, and the probe reports presence from the same
// fake — otherwise the platform probe would be answering about the developer's
// real token file while the reads came from the fake, and every presence
// assertion would be about the wrong machine.
func withStore(t *testing.T, token string) *countingStore {
	t.Helper()
	isolateConfigDir(t)
	st := &countingStore{token: token}
	prevRead, prevProbe := readStoredToken, probeStoredToken
	readStoredToken = st.load
	probeStoredToken = func() bool { return st.token != "" && st.err == nil }
	t.Cleanup(func() { readStoredToken, probeStoredToken = prevRead, prevProbe })
	return st
}

// This is the whole point of the row. Load() is on the startup path (main.go
// calls it before the TUI is built), and on Linux a keychain read is a D-Bus
// round trip to a service that may not be running — a wait, not a lookup.
func TestLoadDoesNotTouchTheSecretStore(t *testing.T) {
	st := withStore(t, "qeuro_live_stored")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.reads != 0 {
		t.Errorf("Load consulted the secret store %d times; the startup path must not", st.reads)
	}
	// And the value is still reachable, or laziness would just be data loss.
	if got := cfg.Secret(); got != "qeuro_live_stored" {
		t.Errorf("Secret() = %q, want the stored token", got)
	}
	if st.reads != 1 {
		t.Errorf("Secret() caused %d reads, want exactly 1", st.reads)
	}
}

// Offline inference must not merely omit the token header; constructing its
// provider must never resolve the token in the first place. A keyring lookup can
// block, prompt, or fail on an air-gapped workstation and has no role in a local
// request.
func TestLocalProviderDoesNotTouchTheSecretStore(t *testing.T) {
	st := withStore(t, "qeuro_live_must_not_be_read")
	cfg, err := LoadWithFlags(map[string]string{
		"local":       "true",
		"local_url":   "http://127.0.0.1:11434",
		"local_model": "qwen2.5-coder:7b",
	})
	if err != nil {
		t.Fatalf("LoadWithFlags: %v", err)
	}

	if _, ok := cfg.Provider().(*client.LocalProvider); !ok {
		t.Fatalf("Provider() = %T, want *client.LocalProvider", cfg.Provider())
	}
	if _, ok := cfg.LazyProvider().(*client.LocalProvider); !ok {
		t.Fatalf("LazyProvider() = %T, want *client.LocalProvider", cfg.LazyProvider())
	}
	if st.reads != 0 {
		t.Fatalf("constructing local providers read the secret store %d times", st.reads)
	}
}

// A resolved token is memoised: a turn that makes three requests must not make
// three D-Bus calls.
func TestSecretIsResolvedOnce(t *testing.T) {
	st := withStore(t, "qeuro_live_stored")

	cfg, _ := Load()
	for i := 0; i < 5; i++ {
		if got := cfg.Secret(); got != "qeuro_live_stored" {
			t.Fatalf("read %d: Secret() = %q", i, got)
		}
	}
	if st.reads != 1 {
		t.Errorf("Secret() read the store %d times across 5 calls, want 1", st.reads)
	}
}

// LoggedIn decides whether to print "offline session", whether to attempt a
// server-side revoke, and what the status bar says. None of those needs the
// secret, so none of them may pay for it.
func TestLoggedInDoesNotResolveTheSecret(t *testing.T) {
	st := withStore(t, "qeuro_live_stored")

	cfg, _ := Load()
	if !cfg.LoggedIn() {
		t.Error("LoggedIn() = false with a token in the store")
	}
	if st.reads != 0 {
		t.Errorf("LoggedIn() read the secret store %d times; presence must be answerable without the value", st.reads)
	}
}

// The inverse, and the one that would silently log everybody out if presence were
// implemented by guessing.
func TestLoggedInIsFalseWithNothingStored(t *testing.T) {
	withStore(t, "")

	cfg, _ := Load()
	if cfg.LoggedIn() {
		t.Error("LoggedIn() = true with an empty store and no config.json")
	}
	if got := cfg.Secret(); got != "" {
		t.Errorf("Secret() = %q, want empty", got)
	}
}

// A higher layer wins, and when it does the keychain must not be consulted at
// all: resolution order already makes the store the lowest-precedence source of
// a token, so reading it first was pure waste.
func TestEnvTokenWinsWithoutReadingTheStore(t *testing.T) {
	st := withStore(t, "qeuro_live_stored")
	t.Setenv("QEURO_TOKEN", "qeuro_live_from_env")

	cfg, _ := Load()
	if got := cfg.Secret(); got != "qeuro_live_from_env" {
		t.Errorf("Secret() = %q, want the env token", got)
	}
	if st.reads != 0 {
		t.Errorf("a token from env still cost %d secret-store reads", st.reads)
	}
	if !cfg.LoggedIn() {
		t.Error("LoggedIn() = false with QEURO_TOKEN set")
	}
}

// config.json is a layer above the keychain too (it is what `qeuro login` wrote
// on a platform without a working store), so a token there also short-circuits.
func TestConfigFileTokenWinsWithoutReadingTheStore(t *testing.T) {
	st := withStore(t, "qeuro_live_stored")
	d := ConfigDir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"base_url":"https://example.invalid","token":"qeuro_live_from_file"}`
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Secret(); got != "qeuro_live_from_file" {
		t.Errorf("Secret() = %q, want the token from config.json", got)
	}
	if st.reads != 0 {
		t.Errorf("a token in config.json still cost %d secret-store reads", st.reads)
	}
}

// Doctor is the one caller that must force the read: its entire purpose is that
// "overridden" and "never read" look different. If it skipped the store, a
// logged-in user would see "(not set)" — the exact confusion the command exists
// to remove.
func TestOriginsForDisplayResolvesTheSecretAndRedactsIt(t *testing.T) {
	st := withStore(t, "qeuro_live_secret_value")

	cfg, _ := Load()
	origins := cfg.OriginsForDisplay()
	if st.reads != 1 {
		t.Fatalf("OriginsForDisplay caused %d reads, want 1: doctor must force the read", st.reads)
	}

	var tokenRow *Origin
	for i := range origins {
		if origins[i].Key == "token" {
			tokenRow = &origins[i]
			break
		}
	}
	if tokenRow == nil {
		t.Fatal("no token row in the doctor output")
	}
	if tokenRow.Value == "" {
		t.Error("token row is empty for a logged-in user: doctor would report (not set)")
	}
	if strings.Contains(tokenRow.Value, "secret_value") {
		t.Errorf("doctor would print the token: %q", tokenRow.Value)
	}
	if !tokenRow.Set {
		t.Error("token row is not marked Set, so doctor attributes it to nothing")
	}
}

// Save must not depend on a prior read having primed anything. On a platform
// where the store is unavailable, the token stays in config.json as the
// documented fallback — and that decision is made by saveStoredToken, which runs
// first, so laziness cannot break it.
func TestSaveRoundTripsThroughTheStore(t *testing.T) {
	withStore(t, "")

	cfg := Config{BaseURL: DefaultBaseURL}
	cfg.SetToken("qeuro_live_written")
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reading back goes through the real platform store, not the fake, so this
	// asserts the end-to-end path the user actually gets.
	prevRead, prevProbe := readStoredToken, probeStoredToken
	readStoredToken, probeStoredToken = loadStoredToken, storedTokenPresent
	t.Cleanup(func() { readStoredToken, probeStoredToken = prevRead, prevProbe })

	got, err := Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if got.Secret() != "qeuro_live_written" {
		t.Errorf("Secret() after Save = %q, want the written token", got.Secret())
	}
	if !got.LoggedIn() {
		t.Error("LoggedIn() = false after Save")
	}
}

// SetToken is how login and logout replace the token. It must make both the
// presence signal and the value agree immediately, with no store read.
func TestSetTokenOverridesEverything(t *testing.T) {
	st := withStore(t, "qeuro_live_stored")

	cfg, _ := Load()
	cfg.SetToken("qeuro_live_replaced")
	if got := cfg.Secret(); got != "qeuro_live_replaced" {
		t.Errorf("Secret() = %q after SetToken", got)
	}
	if !cfg.LoggedIn() {
		t.Error("LoggedIn() = false after SetToken with a value")
	}

	cfg.SetToken("")
	if got := cfg.Secret(); got != "" {
		t.Errorf("Secret() = %q after SetToken(\"\")", got)
	}
	if cfg.LoggedIn() {
		t.Error("LoggedIn() = true after SetToken(\"\")")
	}
	if st.reads != 0 {
		t.Errorf("SetToken caused %d secret-store reads", st.reads)
	}
}

// A store that is unreachable must not look like "logged out with an error": the
// fallback is config.json, and the failure is reported through Warnings so the
// user can see why the keychain did not answer.
func TestUnreachableStoreIsNotFatal(t *testing.T) {
	st := withStore(t, "")
	st.err = errTestStore

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned an error for an unreachable store: %v", err)
	}
	if got := cfg.Secret(); got != "" {
		t.Errorf("Secret() = %q, want empty", got)
	}
	if cfg.LoggedIn() {
		t.Error("LoggedIn() = true with an unreachable store and no token anywhere")
	}
}

type storeErr string

func (e storeErr) Error() string { return string(e) }

const errTestStore = storeErr("secret store unreachable")
