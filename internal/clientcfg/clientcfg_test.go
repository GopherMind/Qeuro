package clientcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func isolateConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("QEURO_API_URL", "")
	t.Setenv("QEURO_CONSOLE_URL", "")
	t.Setenv("QEURO_TOKEN", "")
	t.Setenv("AppData", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestSaveDoesNotWritePlainTokenToConfig(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("non-Windows keeps the owner-only config-file fallback")
	}
	isolateConfigDir(t)

	cfg := Config{
		BaseURL:          "http://api.local",
		ConsoleURL:       "http://console.local",
		OnboardingOpened: true,
	}
	cfg.SetToken("qeuro_live_secret")
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	p, err := path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "qeuro_live_secret") {
		t.Fatalf("config leaked raw token: %s", data)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["token"]; ok {
		t.Fatalf("config should omit token field, got %v", raw)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Secret(), not .Token: the field carries only what config.json holds, and on
	// this platform that is deliberately nothing. The token comes back through
	// the resolver, on demand (roadmap §8 "Startup").
	if got := loaded.Secret(); got != "qeuro_live_secret" {
		t.Fatalf("Load token = %q, want %q", got, "qeuro_live_secret")
	}
	if !loaded.LoggedIn() {
		t.Error("LoggedIn() = false after Save stored a token")
	}
}

func TestLoadMigratesLegacyPlainTokenOnSave(t *testing.T) {
	isolateConfigDir(t)

	d, err := dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(d, "config.json")
	if err := os.WriteFile(legacy, []byte(`{"base_url":"http://api","token":"legacy-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Secret(); got != "legacy-token" {
		t.Fatalf("legacy token not loaded: %q", got)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && strings.Contains(string(data), "legacy-token") {
		t.Fatalf("legacy token should be removed from config after Save: %s", data)
	}
}
