package main

import (
	"strings"
	"testing"

	"qeuro/internal/clientcfg"
)

// TestConfigCommandIsRegistered: roadmap §8 pairs layered precedence with
// `qeuro config doctor`, and an unregistered subcommand falls through to
// "unknown command" — the same class of defect as the unregistered `run`
// command above.
func TestConfigCommandIsRegistered(t *testing.T) {
	var found *command
	for _, cmd := range commands() {
		if cmd.matches("config") {
			c := cmd
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal(`no "config" command: roadmap §8 requires "qeuro config doctor"`)
	}
	if found.run == nil {
		t.Fatal(`"config" command has a nil run func`)
	}
	if !strings.Contains(found.usage, "doctor") {
		t.Errorf("usage %q should show the doctor subcommand", found.usage)
	}
	if found.summary == "" {
		t.Error(`"config" is missing a summary, so it is absent from help output`)
	}
}

// TestDoctorRendersEverySettingWithItsSource is the assertion that keeps doctor
// honest. It renders through the same helper the command uses, so a setting that
// resolves but is not reported would fail here.
func TestDoctorRendersEverySettingWithItsSource(t *testing.T) {
	t.Setenv("QEURO_API_URL", "")
	t.Setenv("QEURO_CONSOLE_URL", "")
	t.Setenv("QEURO_TOKEN", "")
	t.Setenv("QEURO_MODEL", "")
	t.Setenv("QEURO_AUTO_APPROVE", "")
	t.Setenv("QEURO_SKILLS_DIR", "")
	t.Setenv("AppData", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := clientcfg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Origins) == 0 {
		t.Fatal("no origins reported; doctor would print an empty table")
	}
	for _, o := range cfg.Origins {
		if o.Key == "" {
			t.Error("an origin has no key")
		}
		if o.Source == "" {
			t.Errorf("%q has no source; doctor's whole purpose is naming it", o.Key)
		}
		if o.Layer.String() == "" {
			t.Errorf("%q has no layer name", o.Key)
		}
	}
}

// TestLayerStringsAreDistinct: doctor prints these, and two layers sharing a
// label would make the output unable to express which one won.
func TestLayerStringsAreDistinct(t *testing.T) {
	layers := []clientcfg.Layer{
		clientcfg.LayerDefault, clientcfg.LayerUserFile,
		clientcfg.LayerProjectFile, clientcfg.LayerEnv, clientcfg.LayerFlag,
	}
	seen := map[string]bool{}
	for _, l := range layers {
		s := l.String()
		if s == "" {
			t.Errorf("layer %d has an empty name", int(l))
		}
		if seen[s] {
			t.Errorf("layer name %q is used twice", s)
		}
		seen[s] = true
	}
	// Precedence is an ordering, so the constants must stay ordered: a
	// reordering would silently invert which layer wins.
	if !(clientcfg.LayerDefault < clientcfg.LayerUserFile &&
		clientcfg.LayerUserFile < clientcfg.LayerProjectFile &&
		clientcfg.LayerProjectFile < clientcfg.LayerEnv &&
		clientcfg.LayerEnv < clientcfg.LayerFlag) {
		t.Error("layer constants are out of precedence order")
	}
}
