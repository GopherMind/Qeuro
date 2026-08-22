package clientcfg

import (
	"strings"
	"testing"

	"qeuro/internal/client"
)

// Offline mode (roadmap §8 row "Offline") makes a negative promise: with --local
// set, nothing goes to the backend. These tests hold the two halves of that —
// which provider a config builds, and which layers are allowed to choose it.

// TestLocalModeBuildsALocalProvider is the promise itself. If the factory returns
// the backend client here, every caller is silently online while the status line
// says otherwise.
func TestLocalModeBuildsALocalProvider(t *testing.T) {
	cfg := Config{BaseURL: "https://api.qeuro.dev", Local: true}
	if _, ok := cfg.Provider().(*client.LocalProvider); !ok {
		t.Fatalf("Provider() = %T, want *client.LocalProvider", cfg.Provider())
	}
	// The lazy variant is what the TUI startup path uses; it must not be the one
	// place the guarantee is missing.
	if _, ok := cfg.LazyProvider().(*client.LocalProvider); !ok {
		t.Fatalf("LazyProvider() = %T, want *client.LocalProvider", cfg.LazyProvider())
	}
}

// TestWithoutLocalModeTheBackendIsUsed is the other direction: offline mode is
// opt-in, and a default session must keep using the backend.
func TestWithoutLocalModeTheBackendIsUsed(t *testing.T) {
	cfg := Config{BaseURL: "https://api.qeuro.dev"}
	if _, ok := cfg.Provider().(*client.Client); !ok {
		t.Fatalf("Provider() = %T, want *client.Client", cfg.Provider())
	}
}

// TestLocalEndpointDefaults keeps `config doctor` and the status line honest: an
// empty cell would read as "offline mode has nowhere to connect", when in fact
// the default applies.
func TestLocalEndpointDefaults(t *testing.T) {
	if got := (Config{}).LocalEndpoint(); got != client.DefaultLocalURL {
		t.Errorf("LocalEndpoint() = %q, want the default %q", got, client.DefaultLocalURL)
	}
	if got := (Config{LocalURL: "http://10.0.0.5:8080"}).LocalEndpoint(); got != "http://10.0.0.5:8080" {
		t.Errorf("LocalEndpoint() = %q, want the configured value", got)
	}
}

// TestProjectFileCannotChooseTheInferenceEndpoint is the security half of the
// row. `./.qeuro.toml` arrives with a cloned repository, and local_url is where
// prompts and file contents are sent — a repo that could set it would exfiltrate
// them to a host of its choosing, exactly like base_url. `local` matters too: a
// repo silently switching the session to a local model changes which model
// reviewed the user's code without saying so.
func TestProjectFileCannotChooseTheInferenceEndpoint(t *testing.T) {
	for _, key := range []string{"local", "local_url", "local_model"} {
		spec, ok := settingByKey(key)
		if !ok {
			t.Fatalf("setting %q is missing from the registry", key)
		}
		if spec.projectSafe {
			t.Errorf("%q is projectSafe: a cloned repository could redirect inference", key)
		}
	}
}

// TestUnusableLocalURLWarnsAndKeepsTheDefault: offline mode exists for machines
// with no fallback, so a typo in an env var must neither lock the user out nor
// quietly point the session somewhere else.
func TestUnusableLocalURLWarnsAndKeepsTheDefault(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)
	t.Setenv("QEURO_LOCAL", "1")
	t.Setenv("QEURO_LOCAL_URL", "file:///etc/passwd")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalURL != "" {
		t.Errorf("LocalURL = %q, want the bad value rejected", cfg.LocalURL)
	}
	if cfg.LocalEndpoint() != client.DefaultLocalURL {
		t.Errorf("LocalEndpoint() = %q, want the default", cfg.LocalEndpoint())
	}
	var warned bool
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "file:///etc/passwd") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning names the rejected value; warnings = %v", cfg.Warnings)
	}
}

func settingByKey(key string) (setting, bool) {
	for _, s := range settingSpecs {
		if s.key == key {
			return s, true
		}
	}
	return setting{}, false
}
