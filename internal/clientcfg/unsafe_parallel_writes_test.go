package clientcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roadmap-v3 §4.1 rollout flag. The default matters more than usual here: with the
// flag off, each writer in a parallel step gets its own tree, and with it on they
// share one — where two agents can patch the same file, both be told "ok", and one
// edit is discarded. So "absent means off" is the property, not an implementation
// detail.
func TestUnsafeParallelWritesDefaultsOff(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UnsafeParallelWrites {
		t.Fatal("UnsafeParallelWrites is on with no configuration; concurrent writers must be opt-in")
	}
}

// The security half, and the same argument as local_url: `./.qeuro.toml` arrives
// with a cloned repository. A repo that could set this would make the agent
// discard its own edits on the machine that cloned it — silently, since the tool
// reports success either way. The value must be reported as ignored rather than
// dropped, so a user who tried it learns it is not in effect.
func TestProjectFileCannotEnableUnsafeParallelWrites(t *testing.T) {
	spec, ok := settingByKey("unsafe_parallel_writes")
	if !ok {
		t.Fatal("unsafe_parallel_writes is missing from the registry")
	}
	if spec.projectSafe {
		t.Fatal("unsafe_parallel_writes is projectSafe: a cloned repository could re-enable losing edits")
	}

	isolateConfigDir(t)
	dir := isolateWorkDir(t)
	if err := os.WriteFile(filepath.Join(dir, ProjectFileName), []byte("unsafe_parallel_writes = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UnsafeParallelWrites {
		t.Fatal("a project file turned on concurrent writers")
	}
	var warned bool
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "unsafe_parallel_writes") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the ignored project setting was not reported; warnings = %v", cfg.Warnings)
	}
}

// The flag has to actually work, or it is not a rollout flag: a behaviour change
// with no way out is a hard-coded one, and §0.3 asks for an owner and a rollback
// criterion.
func TestUnsafeParallelWritesCanBeEnabledFromTheEnvironment(t *testing.T) {
	isolateConfigDir(t)
	isolateWorkDir(t)
	t.Setenv("QEURO_UNSAFE_PARALLEL_WRITES", "1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UnsafeParallelWrites {
		t.Fatal("QEURO_UNSAFE_PARALLEL_WRITES=1 did not reach the config")
	}
}

// `config doctor` iterates the registry, so a setting the CLI honours but doctor
// does not report is invisible precedence. That is worse for this key than for
// most: someone debugging a vanished edit needs the flag to be visible and to see
// which layer set it.
func TestDoctorReportsUnsafeParallelWrites(t *testing.T) {
	res := resolve(resolveOptions{
		homeDir: t.TempDir(), workDir: t.TempDir(),
		getenv: env(map[string]string{"QEURO_UNSAFE_PARALLEL_WRITES": "true"}),
	})

	var found bool
	for _, o := range res.Origins() {
		if o.Key != "unsafe_parallel_writes" {
			continue
		}
		found = true
		if !o.Set {
			t.Error("doctor reports unsafe_parallel_writes as unset while the environment sets it")
		}
		if o.Layer != LayerEnv {
			t.Errorf("doctor attributes unsafe_parallel_writes to layer %v, want the environment", o.Layer)
		}
	}
	if !found {
		t.Fatal("doctor does not report unsafe_parallel_writes at all")
	}
}
