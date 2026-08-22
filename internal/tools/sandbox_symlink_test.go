package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxRejectsSymlinkReadEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := r.readFile(`{"path":"link.txt"}`)
	if strings.Contains(out, "top secret") {
		t.Fatalf("symlink escaped project root and read secret: %q", out)
	}
}

func TestSandboxRejectsWriteThroughSymlinkDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "linked-dir")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, mutated := r.writeFile(`{"path":"linked-dir/owned.txt","content":"oops"}`)
	if mutated {
		t.Fatalf("symlink dir write was marked successful: %q", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "owned.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created through symlink: %v", err)
	}
}

func TestSandboxSearchSkipsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("unique-search-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "linked-secret.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := r.searchCode(`{"query":"unique-search-secret"}`)
	if strings.Contains(out, "linked-secret.txt:") {
		t.Fatalf("search_code escaped project root through symlink: %q", out)
	}
}
