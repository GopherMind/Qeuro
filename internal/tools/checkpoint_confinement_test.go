package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointRejectsSymlinkMetadataEscape(t *testing.T) {
	tests := []struct {
		name string
		link func(root, outside string) error
	}{
		{
			name: "checkpoint root",
			link: func(root, outside string) error {
				if err := os.MkdirAll(filepath.Join(root, ".infinity"), 0o700); err != nil {
					return err
				}
				return os.Symlink(outside, filepath.Join(root, ".infinity", "checkpoints"))
			},
		},
		{
			name: "object directory",
			link: func(root, outside string) error {
				if err := os.MkdirAll(filepath.Join(root, ".infinity", "checkpoints"), 0o700); err != nil {
					return err
				}
				return os.Symlink(outside, filepath.Join(root, ".infinity", "checkpoints", "objects"))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			write(t, root, "f.txt", "old")
			if err := tc.link(root, outside); err != nil {
				t.Skipf("directory symlink unavailable: %v", err)
			}

			r, err := NewRunner(root)
			if err != nil {
				t.Fatal(err)
			}
			out, mutated := r.Execute(ToolPatchFile, `{"path":"f.txt","old_content":"old","new_content":"new"}`)
			if mutated || !strings.Contains(out, "checkpoint error") {
				t.Fatalf("metadata symlink escape = (%q, %t), want checkpoint refusal", out, mutated)
			}
			if got, err := os.ReadFile(filepath.Join(root, "f.txt")); err != nil || string(got) != "old" {
				t.Fatalf("refused checkpoint changed source: %q, %v", got, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("checkpoint wrote outside workspace through symlink: %v", entries)
			}
		})
	}
}
