package tui

import (
	"strings"
	"testing"

	"qeuro/internal/catalog"
)

// TestResolveConfiguredModelPinsAKnownID: roadmap §8 makes the config layers
// govern every entry point, so a model set in a file or env has to reach the
// interactive session too, not only `qeuro run`.
func TestResolveConfiguredModelPinsAKnownID(t *testing.T) {
	current := catalog.Model{ID: "fallback/model", Label: "Fallback"}
	brands := catalog.Brands()
	if len(brands) == 0 || len(brands[0].Models) == 0 {
		t.Fatal("catalogue is empty; nothing to pin")
	}
	want, _, ok := catalog.FindModel(brands[0].Models[0].ID)
	if !ok {
		t.Fatalf("FindModel could not resolve its own catalogue entry %q", brands[0].Models[0].ID)
	}

	got, notice, pinned := resolveConfiguredModel(want.ID, current)
	if !pinned {
		t.Fatalf("configured id %q was not applied", want.ID)
	}
	if got.ID != want.ID {
		t.Errorf("model = %q, want %q", got.ID, want.ID)
	}
	if notice != "" {
		t.Errorf("notice = %q, want none for a valid id", notice)
	}
	// Leading and trailing space is what a hand-edited file produces.
	if got, _, ok := resolveConfiguredModel("  "+want.ID+"  ", current); !ok || got.ID != want.ID {
		t.Errorf("a padded id was not trimmed: got %q, ok=%v", got.ID, ok)
	}
}

// TestResolveConfiguredModelReportsAnUnknownID: starting on a different model
// than the one asked for, without saying so, is the failure this roadmap row
// exists to prevent.
func TestResolveConfiguredModelReportsAnUnknownID(t *testing.T) {
	current := catalog.Model{ID: "fallback/model", Label: "Fallback"}

	got, notice, pinned := resolveConfiguredModel("vendor/does-not-exist", current)
	if pinned {
		t.Fatal("an unknown id was applied as if it were real")
	}
	if got.ID != current.ID {
		t.Errorf("model = %q, want the session to keep %q", got.ID, current.ID)
	}
	if !strings.Contains(notice, "vendor/does-not-exist") {
		t.Errorf("notice = %q, must name the id the user configured", notice)
	}
	if !strings.Contains(notice, current.Label) {
		t.Errorf("notice = %q, must say which model is being used instead", notice)
	}
}

// TestResolveConfiguredModelEscapesTerminalSequences: the id is echoed into the
// terminal. `.qeuro.toml` cannot carry a control character — the TOML reader
// refuses it — but QEURO_MODEL and --model never pass through that reader, and an
// escape here could redraw the line so the notice claimed something else.
func TestResolveConfiguredModelEscapesTerminalSequences(t *testing.T) {
	current := catalog.Model{ID: "fallback/model", Label: "Fallback"}

	_, notice, pinned := resolveConfiguredModel("evil\x1b]0;PWNED\x07\rid", current)
	if pinned {
		t.Fatal("a hostile id was treated as a catalogue model")
	}
	for _, c := range notice {
		if c < 0x20 && c != '\t' {
			t.Fatalf("notice carries control byte 0x%02x and reaches the terminal raw: %q", c, notice)
		}
	}
	if !strings.Contains(notice, `\x1b`) {
		t.Errorf("notice = %q, want the escape shown visibly so the user can see it", notice)
	}
}

// TestResolveConfiguredModelIgnoresAnEmptyValue: no configured model means the
// auto-router decides, which must not be reported as a problem.
func TestResolveConfiguredModelIgnoresAnEmptyValue(t *testing.T) {
	current := catalog.Model{ID: "fallback/model", Label: "Fallback"}
	for _, v := range []string{"", "   ", "\t"} {
		got, notice, pinned := resolveConfiguredModel(v, current)
		if pinned || notice != "" {
			t.Errorf("%q produced pinned=%v notice=%q, want neither", v, pinned, notice)
		}
		if got.ID != current.ID {
			t.Errorf("%q changed the model to %q", v, got.ID)
		}
	}
}
