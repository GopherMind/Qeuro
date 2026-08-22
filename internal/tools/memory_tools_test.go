package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryToolsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRunner(dir)
	if out, _ := r.Execute(ToolRemember, `{"category":"stack","note":"Go 1.22 backend, Bubble Tea TUI"}`); !strings.Contains(out, "stack") {
		t.Fatalf("remember: %q", out)
	}
	// alias front -> frontend
	r.Execute(ToolRemember, `{"category":"front","note":"React + Vite in /frontend"}`)
	r.Execute(ToolRemember, `{"category":"changes","note":"Rebranded to Infinity CLI"}`)

	if _, err := os.Stat(filepath.Join(dir, ".infinity", "memory", "stack.md")); err != nil {
		t.Fatalf("stack.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infinity", "memory", "frontend.md")); err != nil {
		t.Fatalf("frontend.md (via 'front' alias) not written: %v", err)
	}
	digest, _ := r.Execute(ToolRecall, `{}`)
	for _, want := range []string{"stack", "Go 1.22", "frontend", "React", "changes", "Infinity"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest missing %q:\n%s", want, digest)
		}
	}
	t.Logf("digest:\n%s", digest)
}
