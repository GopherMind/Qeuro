package clientcfg

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// isolateWorkDir moves the process into a temp directory so a real `.qeuro.toml`
// in the repository cannot influence the test, and restores it afterwards.
func isolateWorkDir(t *testing.T) string {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	// os.TempDir may be a symlink (/var → /private/var on macOS); compare against
	// what the OS reports so path assertions do not fail on the alias.
	resolved, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestLoadAppliesTOMLLayersOverConfigJSON pins the placement of config.json in
// the chain: it is state the CLI wrote, so a file the user hand-edited has to
// beat it, otherwise editing the TOML would appear to do nothing.
func TestLoadAppliesTOMLLayersOverConfigJSON(t *testing.T) {
	isolateConfigDir(t)
	work := isolateWorkDir(t)

	d, err := dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"),
		[]byte(`{"base_url":"http://from-json","console_url":"http://console-json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, UserFileName),
		[]byte("base_url = \"http://from-user-toml\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseURL != "http://from-user-toml" {
		t.Errorf("BaseURL = %q, want the TOML value to beat config.json", cfg.BaseURL)
	}
	// A key the TOML did not mention must keep the config.json value rather than
	// being blanked by an empty higher layer.
	if cfg.ConsoleURL != "http://console-json" {
		t.Errorf("ConsoleURL = %q, want the config.json value preserved", cfg.ConsoleURL)
	}
	if work == "" {
		t.Fatal("working directory not isolated")
	}
}

// TestLoadWithFlagsPutsFlagsOnTop covers the seam commands use: a flag must go
// through resolution so doctor reports it, not be assigned over the result.
func TestLoadWithFlagsPutsFlagsOnTop(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)
	t.Setenv("QEURO_MODEL", "env-model")

	cfg, err := LoadWithFlags(map[string]string{"model": "flag-model"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "flag-model" {
		t.Fatalf("Model = %q, want the flag to win", cfg.Model)
	}
	var found bool
	for _, o := range cfg.Origins {
		if o.Key == "model" {
			found = true
			if o.Layer != LayerFlag {
				t.Errorf("model origin layer = %v, want flag", o.Layer)
			}
		}
	}
	if !found {
		t.Error("model missing from Origins; doctor could not report it")
	}
}

// TestLoadReportsEffectiveValuesForUnsetSettings: doctor showing an empty cell
// for a setting that does have an effective value reads as "not configured",
// which sends the user looking for the wrong problem.
func TestLoadReportsEffectiveValuesForUnsetSettings(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, o := range cfg.Origins {
		if o.Key != "base_url" {
			continue
		}
		if o.Value != DefaultBaseURL {
			t.Errorf("base_url doctor value = %q, want the built-in default %q", o.Value, DefaultBaseURL)
		}
		if o.Source != "built-in" {
			t.Errorf("base_url source = %q; a fresh install must not claim a file it never wrote", o.Source)
		}
	}
}

// TestLoadNeverPutsATokenInOrigins is the leak guard on the whole Origins
// pathway, independent of how doctor formats it.
func TestLoadNeverPutsATokenInOrigins(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)
	const token = "qeuro_live_abcdef0123456789"
	t.Setenv("QEURO_TOKEN", token)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != token {
		t.Fatalf("Token = %q, want the real value for authentication", cfg.Token)
	}
	for _, o := range cfg.Origins {
		if strings.Contains(o.Value, "abcdef0123456789") {
			t.Fatalf("origin %q exposed the token: %q", o.Key, o.Value)
		}
	}
}

// TestProjectFileInWorkingDirectoryIsRead is the end-to-end version of the
// project layer: resolve() is unit-tested with an injected directory, so this
// checks the wiring that finds the real working directory.
func TestProjectFileInWorkingDirectoryIsRead(t *testing.T) {
	isolateConfigDir(t)
	work := isolateWorkDir(t)

	if err := os.WriteFile(filepath.Join(work, ProjectFileName),
		[]byte("model = \"openai/gpt-5.5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "openai/gpt-5.5" {
		t.Fatalf("Model = %q, want the project file to be read", cfg.Model)
	}
	if got := ProjectFilePath(); got != filepath.Join(work, ProjectFileName) {
		t.Errorf("ProjectFilePath() = %q, want %q", got, filepath.Join(work, ProjectFileName))
	}
}

// TestEverySettingReachesTheFieldThatUsesIt binds the registry to its consumers.
//
// Found by mutation testing: deleting a setting from settingSpecs left every
// other test green, because the ones that count settings compare the registry
// against itself. The real consequence of that deletion is that the env var
// stops working — silently, which is precisely the failure this roadmap row
// exists to remove. So each key is driven through a layer here and asserted to
// land in the field the CLI actually reads.
func TestEverySettingReachesTheFieldThatUsesIt(t *testing.T) {
	cases := []struct {
		key, env, value string
		read            func(Config) string
	}{
		{"base_url", "QEURO_API_URL", "http://layered-base", func(c Config) string { return c.BaseURL }},
		{"console_url", "QEURO_CONSOLE_URL", "http://layered-console", func(c Config) string { return c.ConsoleURL }},
		{"token", "QEURO_TOKEN", "qeuro_live_layered", func(c Config) string { return c.Token }},
		{"model", "QEURO_MODEL", "vendor/layered-model", func(c Config) string { return c.Model }},
		{"skills_dir", "QEURO_SKILLS_DIR", "/layered/skills", func(c Config) string { return c.SkillsDir }},
		{"auto_approve", "QEURO_AUTO_APPROVE", "1", func(c Config) string {
			if c.AutoApprove {
				return "1"
			}
			return ""
		}},
		{"budget", "QEURO_BUDGET", "25", func(c Config) string {
			if c.Budget == 0 {
				return ""
			}
			return strconv.FormatFloat(c.Budget, 'f', -1, 64)
		}},
		{"local", "QEURO_LOCAL", "1", func(c Config) string {
			if c.Local {
				return "1"
			}
			return ""
		}},
		{"local_url", "QEURO_LOCAL_URL", "http://127.0.0.1:8081", func(c Config) string { return c.LocalURL }},
		{"local_model", "QEURO_LOCAL_MODEL", "qwen2.5-coder:7b", func(c Config) string { return c.LocalModel }},
		{"unsafe_parallel_writes", "QEURO_UNSAFE_PARALLEL_WRITES", "1", func(c Config) string {
			if c.UnsafeParallelWrites {
				return "1"
			}
			return ""
		}},
	}
	if len(cases) != len(settingSpecs) {
		t.Fatalf("%d settings are wired to a field, registry has %d: a new setting needs a case here",
			len(cases), len(settingSpecs))
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			isolateConfigDir(t)
			isolateWorkDir(t)
			t.Setenv(tc.env, tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.read(cfg); got != tc.value {
				t.Errorf("%s set via %s did not reach its Config field: got %q, want %q",
					tc.key, tc.env, got, tc.value)
			}
			var reported bool
			for _, o := range cfg.Origins {
				if o.Key == tc.key {
					reported = true
					if o.Layer != LayerEnv {
						t.Errorf("%s origin layer = %v, want env", tc.key, o.Layer)
					}
				}
			}
			if !reported {
				t.Errorf("%s is honoured but absent from Origins; doctor could not report it", tc.key)
			}
		})
	}
}

// TestCorruptTOMLDoesNotBlockEnvOverrides keeps the recovery path open: a broken
// file must not stop someone from pointing the CLI somewhere with an env var,
// which is how they would work around it.
func TestCorruptTOMLDoesNotBlockEnvOverrides(t *testing.T) {
	isolateConfigDir(t)
	work := isolateWorkDir(t)

	if err := os.WriteFile(filepath.Join(work, ProjectFileName), []byte("[oops]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QEURO_API_URL", "http://rescue")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned a hard error for a broken TOML: %v", err)
	}
	if cfg.BaseURL != "http://rescue" {
		t.Fatalf("BaseURL = %q, want the env override to still apply", cfg.BaseURL)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("a broken config file must produce a warning, not pass silently")
	}
}

// TestBudgetIsNotProjectSafe: a cloned repository must not be able to set — or
// clear — a ceiling on the user's own money. Only `model` is project-safe.
func TestBudgetIsNotProjectSafe(t *testing.T) {
	isolateConfigDir(t)
	work := isolateWorkDir(t)
	t.Setenv("QEURO_BUDGET", "50")

	if err := os.WriteFile(filepath.Join(work, ProjectFileName),
		[]byte("budget = \"0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Budget != 50 {
		t.Fatalf("Budget = %v, want 50: a project file overrode the user's ceiling", cfg.Budget)
	}
}

// A ceiling the CLI cannot use must warn rather than be silently dropped: the
// user who typed it believes a limit is in force. It must not be a hard failure
// either, or a typo in a file locks them out of their own CLI.
func TestUnusableBudgetWarnsAndLeavesNoCeiling(t *testing.T) {
	for _, v := range []string{"banana", "-5", "nan", "NaN", "inf", "+Inf", "-inf"} {
		t.Run(v, func(t *testing.T) {
			isolateConfigDir(t)
			isolateWorkDir(t)
			t.Setenv("QEURO_BUDGET", v)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load returned a hard error for budget=%q: %v", v, err)
			}
			if cfg.Budget != 0 {
				t.Fatalf("Budget = %v for %q, want 0 (no ceiling) — a NaN or Inf ceiling never fires",
					cfg.Budget, v)
			}
			if len(cfg.Warnings) == 0 {
				t.Fatalf("budget=%q was dropped with no warning; the user believes a limit is in force", v)
			}
		})
	}
}

func TestValidBudgetReachesTheConfig(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)
	t.Setenv("QEURO_BUDGET", "12.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Budget != 12.5 {
		t.Fatalf("Budget = %v, want 12.5", cfg.Budget)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("a valid budget produced warnings: %v", cfg.Warnings)
	}
}
