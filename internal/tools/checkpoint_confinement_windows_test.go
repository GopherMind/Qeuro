//go:build windows

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointRejectsWindowsJunctionMetadataEscape(t *testing.T) {
	tests := []struct {
		name                   string
		linkParent             func(string) string
		linkName               string
		checkpointRootMustFail bool
	}{
		{
			name: "checkpoint root",
			linkParent: func(root string) string {
				return filepath.Join(root, ".infinity")
			},
			linkName:               "checkpoints",
			checkpointRootMustFail: true,
		},
		{
			name: "object directory",
			linkParent: func(root string) string {
				return filepath.Join(root, ".infinity", "checkpoints")
			},
			linkName: "objects",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			write(t, root, "f.txt", "old")
			parent := tc.linkParent(root)
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(parent, tc.linkName)
			if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
				t.Skipf("junction unavailable: %v: %s", err, output)
			}
			if tc.checkpointRootMustFail {
				checkpoint, err := newCheckpointStore(root).openCheckpointRoot()
				if err == nil {
					if closeErr := checkpoint.Close(); closeErr != nil {
						t.Fatalf("escaped checkpoint root opened and close failed: %v", closeErr)
					}
					t.Fatal("checkpoint root opened through an outside junction")
				}
			}

			r, err := NewRunner(root)
			if err != nil {
				t.Fatal(err)
			}
			out, mutated := r.Execute(ToolPatchFile, `{"path":"f.txt","old_content":"old","new_content":"new"}`)
			if mutated || !strings.Contains(out, "checkpoint error") {
				t.Fatalf("metadata junction escape = (%q, %t), want checkpoint refusal", out, mutated)
			}
			if got, err := os.ReadFile(filepath.Join(root, "f.txt")); err != nil || string(got) != "old" {
				t.Fatalf("refused checkpoint changed source: %q, %v", got, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("checkpoint wrote outside workspace through junction: %v", entries)
			}
		})
	}
}
