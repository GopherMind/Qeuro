//go:build windows

package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxAllowsOrdinaryWindowsRelativePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ordinary.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out := r.readFile(`{"path":"ordinary.txt"}`); out != "inside" {
		t.Fatalf("ordinary in-root file rejected: %q", out)
	}
}

func TestWindowsCanonicalFallbackIsPermissionOnly(t *testing.T) {
	if !canonicalPermissionFallback(os.ErrPermission) {
		t.Fatal("permission errors must use the Windows component-walk fallback")
	}
	if canonicalPermissionFallback(os.ErrNotExist) {
		t.Fatal("missing-path errors must remain denied")
	}
	if canonicalPermissionFallback(errors.New("other")) {
		t.Fatal("unexpected canonicalization errors must remain denied")
	}
}

func TestSandboxSearchSkipsWindowsJunctionEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("junction-search-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("junction unavailable: %v: %s", err, out)
	}

	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := r.searchCode(`{"query":"junction-search-secret"}`)
	if strings.Contains(filepath.ToSlash(out), "junction/secret.txt:") {
		t.Fatalf("search_code escaped project root through junction: %q", out)
	}
}

func TestSandboxRejectsDirectWindowsJunctionAccess(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("direct-junction-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("junction unavailable: %v: %s", err, out)
	}

	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out := r.readFile(`{"path":"junction/secret.txt"}`); strings.Contains(out, "direct-junction-secret") {
		t.Fatalf("read_file escaped project root through junction: %q", out)
	}
	if out, mutated := r.writeFile(`{"path":"junction/owned.txt","content":"outside"}`); mutated {
		t.Fatalf("write_file escaped project root through junction: %q", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "owned.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created through junction: %v", err)
	}
}
