package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// roadmap-v3 §4.1 (C-1). Each writer node gets its own working tree; read-only
// nodes may share a snapshot; the integration step applies patches in a controlled
// order rather than "whoever wrote first".

// baseTree builds a small project and returns its root plus a Runner on it.
func baseTree(t *testing.T, files map[string]string) (string, *Runner) {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, r
}

func mustIsolate(t *testing.T, r *Runner, name string) *Runner {
	t.Helper()
	iso, err := r.Isolated(name)
	if err != nil {
		t.Fatalf("Isolated(%q): %v", name, err)
	}
	return iso
}

// The gate's negative test, and the reason this increment exists: two independent
// writers must not share a mutable tree. Both patch the same file concurrently;
// before isolation this lost one edit in 12 of 12 runs (ledger §40.2) while
// reporting success twice.
func TestTwoWritersDoNotShareAMutableTree(t *testing.T) {
	base, r := baseTree(t, map[string]string{
		"a.go": "package p\n\nfunc A() {}\n\nfunc B() {}\n" + strings.Repeat("// filler\n", 40),
	})
	before, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}

	first := mustIsolate(t, r, "backend")
	second := mustIsolate(t, r, "frontend")

	var wg sync.WaitGroup
	results := make([]string, 2)
	writers := []*Runner{first, second}
	args := []string{
		`{"path":"a.go","old_content":"func A() {}","new_content":"func A() { println(1) }"}`,
		`{"path":"a.go","old_content":"func B() {}","new_content":"func B() { println(2) }"}`,
	}
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = writers[i].Execute(ToolPatchFile, args[i])
		}(i)
	}
	wg.Wait()

	for i, res := range results {
		if !strings.HasPrefix(res, "ok:") {
			t.Fatalf("writer %d failed: %q", i, res)
		}
	}

	// Neither writer's edit reached the project tree, and neither saw the other's.
	after, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("a writer changed the project tree directly; the trees are still shared")
	}
	body, err := os.ReadFile(filepath.Join(base, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "println") {
		t.Fatalf("the project file was modified during the parallel run:\n%s", body)
	}

	// And each writer kept exactly its own edit — the property the shared tree
	// destroyed.
	firstChanges, err := first.Changes()
	if err != nil {
		t.Fatal(err)
	}
	secondChanges, err := second.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(firstChanges) != 1 || len(secondChanges) != 1 {
		t.Fatalf("changes: first=%d second=%d, want 1 each", len(firstChanges), len(secondChanges))
	}
	if !strings.Contains(string(firstChanges[0].Content), "println(1)") ||
		strings.Contains(string(firstChanges[0].Content), "println(2)") {
		t.Error("the first writer's tree does not hold exactly its own edit")
	}
	if !strings.Contains(string(secondChanges[0].Content), "println(2)") ||
		strings.Contains(string(secondChanges[0].Content), "println(1)") {
		t.Error("the second writer's tree does not hold exactly its own edit")
	}
}

// A writer reads through to the base tree for everything it has not touched, and
// sees its own edits afterwards. Without the fall-through the overlay would look
// like an empty project and every read would fail.
func TestIsolatedWriterReadsThroughAndThenSeesItsOwnEdits(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n\nconst X = 1\n"})
	iso := mustIsolate(t, r, "backend")

	if got, _ := iso.Execute(ToolReadFile, `{"path":"a.go"}`); !strings.Contains(got, "const X = 1") {
		t.Fatalf("read-through failed: %q", got)
	}
	if res, _ := iso.Execute(ToolPatchFile, `{"path":"a.go","old_content":"const X = 1","new_content":"const X = 2"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("patch failed: %q", res)
	}
	got, _ := iso.Execute(ToolReadFile, `{"path":"a.go"}`)
	if !strings.Contains(got, "const X = 2") {
		t.Fatalf("the writer cannot see its own edit: %q", got)
	}
	// The base is untouched, so a second writer still reads the original.
	other := mustIsolate(t, r, "frontend")
	if got, _ := other.Execute(ToolReadFile, `{"path":"a.go"}`); !strings.Contains(got, "const X = 1") {
		t.Fatalf("a second writer saw the first writer's edit: %q", got)
	}
}

// list_dir and search_code must show the union, or a writer cannot find the file
// it just created and cannot see the project it is working on.
func TestIsolatedWriterListsAndSearchesBothTrees(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n// findme in base\n"})
	iso := mustIsolate(t, r, "backend")
	if res, _ := iso.Execute(ToolWriteFile, `{"path":"b.go","content":"package p\n// findme in overlay\n"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("write failed: %q", res)
	}

	listing, _ := iso.Execute(ToolListDir, `{"path":"."}`)
	for _, want := range []string{"a.go", "b.go"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing does not include %s:\n%s", want, listing)
		}
	}
	// The isolation directory is an implementation detail and must not be listed.
	if strings.Contains(listing, ".infinity") {
		t.Errorf("the listing exposes the isolation directory:\n%s", listing)
	}
	// A file must not appear twice just because it exists in both trees.
	if n := strings.Count(listing, "a.go"); n != 1 {
		t.Errorf("a.go appears %d times in the listing:\n%s", n, listing)
	}

	hits, _ := iso.Execute(ToolSearchCode, `{"query":"findme"}`)
	for _, want := range []string{"a.go", "b.go"} {
		if !strings.Contains(hits, want) {
			t.Errorf("search does not find %s:\n%s", want, hits)
		}
	}
}

// A changed file is reported from the writer's own copy, not twice with the base's
// stale version alongside it.
func TestSearchPrefersTheWritersCopy(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n// marker old\n"})
	iso := mustIsolate(t, r, "backend")
	if res, _ := iso.Execute(ToolPatchFile, `{"path":"a.go","old_content":"marker old","new_content":"marker new"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("patch failed: %q", res)
	}
	hits, _ := iso.Execute(ToolSearchCode, `{"query":"marker"}`)
	if strings.Contains(hits, "marker old") {
		t.Errorf("search reported the stale base copy:\n%s", hits)
	}
	if !strings.Contains(hits, "marker new") {
		t.Errorf("search did not report the writer's copy:\n%s", hits)
	}
}

// write_file must still refuse to replace an existing project file. The overlay
// starts empty, so a naive existence check would report every file as absent and
// turn isolation into a way around that rule.
func TestWriteFileStillRefusesExistingBaseFiles(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n"})
	iso := mustIsolate(t, r, "backend")
	res, mutated := iso.Execute(ToolWriteFile, `{"path":"a.go","content":"replaced\n"}`)
	if mutated {
		t.Fatal("write_file replaced an existing project file inside an overlay")
	}
	if !strings.Contains(res, "already exists") {
		t.Fatalf("refusal did not name the cause: %q", res)
	}
}

// Commands cannot be contained by a copy-on-write overlay, so an isolated writer
// does not get a shell (roadmap §4.2 widens the checkpoint boundary later).
func TestIsolatedWriterCannotRunCommands(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n"})
	iso := mustIsolate(t, r, "backend")
	res, mutated := iso.Execute(ToolRunCommand, `{"command":"go build ./..."}`)
	if mutated {
		t.Fatal("a command ran inside an isolated worktree")
	}
	if !strings.Contains(res, "isolated worktree") {
		t.Fatalf("refusal did not name the reason: %q", res)
	}
	// The ordinary Runner is unaffected: the tester still needs a shell.
	if res, _ := r.Execute(ToolRunCommand, `{"command":"git status"}`); strings.Contains(res, "isolated worktree") {
		t.Fatalf("the project Runner lost the ability to run commands: %q", res)
	}
}

// The integration step applies work in the order it is given, not in the order the
// writers finished. Two writers touching different files, integrated twice with
// their order swapped, must produce the same tree.
func TestIntegrationOrderIsTheCallersNotWhoFinishedFirst(t *testing.T) {
	run := func(reverse bool) string {
		_, r := baseTree(t, map[string]string{"a.go": "package p\n", "b.go": "package p\n"})
		first := mustIsolate(t, r, "backend")
		second := mustIsolate(t, r, "frontend")
		if res, _ := first.Execute(ToolWriteFile, `{"path":"new_a.go","content":"package p\n// a\n"}`); !strings.HasPrefix(res, "ok:") {
			t.Fatalf("first write: %q", res)
		}
		if res, _ := second.Execute(ToolWriteFile, `{"path":"new_b.go","content":"package p\n// b\n"}`); !strings.HasPrefix(res, "ok:") {
			t.Fatalf("second write: %q", res)
		}
		fc, err := first.Changes()
		if err != nil {
			t.Fatal(err)
		}
		sc, err := second.Changes()
		if err != nil {
			t.Fatal(err)
		}
		contribs := []Contribution{{Writer: "backend", Changes: fc}, {Writer: "frontend", Changes: sc}}
		if reverse {
			contribs[0], contribs[1] = contribs[1], contribs[0]
		}
		if _, err := r.Integrate(contribs); err != nil {
			t.Fatalf("Integrate: %v", err)
		}
		h, err := r.TreeHash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if forward, reverse := run(false), run(true); forward != reverse {
		t.Fatalf("integration is order-dependent: %s vs %s", forward, reverse)
	}
}

// Conflicting writers are refused, and nothing is applied. Applying one and
// dropping the other silently is the failure this whole row exists to remove.
func TestIntegrationRefusesConflictingWritersAndAppliesNothing(t *testing.T) {
	base, r := baseTree(t, map[string]string{
		"a.go": "package p\n\nfunc A() {}\n\nfunc B() {}\n" + strings.Repeat("// filler\n", 40),
		"b.go": "package p\n",
	})
	before, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}

	first := mustIsolate(t, r, "backend")
	second := mustIsolate(t, r, "frontend")
	if res, _ := first.Execute(ToolPatchFile, `{"path":"a.go","old_content":"func A() {}","new_content":"func A() { println(1) }"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("first patch: %q", res)
	}
	// A change to an unrelated file that would be applied if the refusal were not
	// all-or-nothing. Ordered first on purpose.
	if res, _ := first.Execute(ToolPatchFile, `{"path":"b.go","old_content":"package p","new_content":"package p // touched"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("first unrelated patch: %q", res)
	}
	if res, _ := second.Execute(ToolPatchFile, `{"path":"a.go","old_content":"func B() {}","new_content":"func B() { println(2) }"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("second patch: %q", res)
	}

	fc, err := first.Changes()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := second.Changes()
	if err != nil {
		t.Fatal(err)
	}
	contribs := []Contribution{{Writer: "backend", Changes: fc}, {Writer: "frontend", Changes: sc}}

	conflicts := Conflicts(contribs)
	if len(conflicts) != 1 || conflicts[0].Path != "a.go" {
		t.Fatalf("Conflicts() = %v, want exactly a.go", conflicts)
	}
	if len(conflicts[0].Writers) != 2 {
		t.Fatalf("conflict names %v, want both writers", conflicts[0].Writers)
	}

	applied, err := r.Integrate(contribs)
	if err == nil {
		t.Fatal("conflicting writers were integrated")
	}
	if len(applied) != 0 {
		t.Fatalf("%d paths were applied despite the conflict: %v", len(applied), applied)
	}
	if !strings.Contains(err.Error(), "a.go") {
		t.Errorf("the error does not name the conflicting path: %v", err)
	}
	after, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("the tree changed even though integration was refused")
	}
	body, err := os.ReadFile(filepath.Join(base, "b.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "touched") {
		t.Fatal("a non-conflicting change was applied before the conflict was hit")
	}
}

// The gate: rollback after a parallel run restores the original tree hash,
// untracked files included (TreeHash walks the tree, not the index).
func TestRollbackAfterAParallelRunRestoresTheTreeHash(t *testing.T) {
	_, r := baseTree(t, map[string]string{
		"a.go":          "package p\n\nconst X = 1\n",
		"untracked.txt": "not in any index\n",
	})
	before, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}

	first := mustIsolate(t, r, "backend")
	second := mustIsolate(t, r, "frontend")
	if res, _ := first.Execute(ToolPatchFile, `{"path":"a.go","old_content":"const X = 1","new_content":"const X = 2"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("patch: %q", res)
	}
	if res, _ := second.Execute(ToolWriteFile, `{"path":"created.go","content":"package p\n"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("write: %q", res)
	}
	fc, err := first.Changes()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := second.Changes()
	if err != nil {
		t.Fatal(err)
	}
	applied, err := r.Integrate([]Contribution{
		{Writer: "backend", Changes: fc},
		{Writer: "frontend", Changes: sc},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied %v, want both changes", applied)
	}
	mid, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if mid == before {
		t.Fatal("integration did not change the tree; this test is not exercising rollback")
	}

	// Every integrated change is its own checkpoint, so undoing them in reverse
	// returns the tree byte-for-byte.
	for range applied {
		if _, ok := r.Undo(); !ok {
			t.Fatal("Undo refused a checkpoint written by integration")
		}
	}
	after, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rollback did not restore the tree:\nbefore=%s\nafter =%s", before, after)
	}
}

// The overlay directory name comes from a model-authored plan, so it is untrusted
// input becoming a filesystem path. Every one of these must either be reduced to a
// safe single component or refused — never escape the isolation directory.
func TestIsolationSlugCannotEscape(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n"})
	for _, name := range []string{
		"..", "../..", "../../../etc", "a/../../b", "back/end", `back\end`,
		".", "...", "/abs", `C:\win`, "a\x00b", "  ", "…", "\x1b[2J",
	} {
		iso, err := r.Isolated(name)
		if err != nil {
			continue // refused outright, which is the other acceptable answer
		}
		rel, relErr := filepath.Rel(filepath.Join(r.root, filepath.FromSlash(isolationDir)), iso.root)
		if relErr != nil {
			t.Errorf("%q produced an unrelatable path %q", name, iso.root)
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/") {
			t.Errorf("name %q escaped the isolation directory: %q", name, rel)
		}
	}
}

// Two writers with the same role must not be handed the same directory: their
// changes would merge invisibly, which is the shared-tree defect wearing a
// different hat.
func TestDistinctWritersGetDistinctTrees(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n"})
	first := mustIsolate(t, r, "backend")
	second := mustIsolate(t, r, "frontend")
	if first.root == second.root {
		t.Fatal("two writers share one overlay directory")
	}
	if first.checkpoints == second.checkpoints {
		t.Fatal("two writers share one checkpoint store; one could undo the other's work")
	}
}

// Re-isolating the same role starts from an empty tree. A leftover directory from
// an earlier run would contribute files nobody wrote in this run.
func TestReIsolatingClearsThePreviousTree(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n"})
	first := mustIsolate(t, r, "backend")
	if res, _ := first.Execute(ToolWriteFile, `{"path":"stale.go","content":"package p\n"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("write: %q", res)
	}
	again := mustIsolate(t, r, "backend")
	changes, err := again.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("a re-isolated writer inherited %d change(s): %v", len(changes), changes)
	}
}

// A file materialized but written back unchanged is not a change, so it must not
// collide with another writer that genuinely edited it.
func TestUnchangedMaterializationIsNotAChange(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n\nconst X = 1\n"})
	iso := mustIsolate(t, r, "backend")
	// Patch and then patch back.
	if res, _ := iso.Execute(ToolPatchFile, `{"path":"a.go","old_content":"const X = 1","new_content":"const X = 2"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("patch: %q", res)
	}
	if res, _ := iso.Execute(ToolPatchFile, `{"path":"a.go","old_content":"const X = 2","new_content":"const X = 1"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("patch back: %q", res)
	}
	changes, err := iso.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("a net-zero edit was reported as a change: %v", changes)
	}
}

// The API boundaries. Each of these would be a silent no-op or a corrupted tree if
// it were allowed, so each is an error rather than a best effort.
func TestIsolationAPIRefusesWrongRunners(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n"})
	iso := mustIsolate(t, r, "backend")

	if _, err := iso.Isolated("nested"); err == nil {
		t.Error("an isolated Runner was isolated again")
	}
	if _, err := r.Changes(); err == nil {
		t.Error("Changes() answered for a non-isolated Runner")
	}
	if err := r.Discard(); err == nil {
		t.Error("Discard() ran on the project tree")
	}
	if _, err := iso.Integrate(nil); err == nil {
		t.Error("Integrate() ran on an overlay")
	}
	if _, err := r.Isolated(""); err == nil {
		t.Error("an unnamed writer got a tree")
	}
	if !iso.IsIsolated() || r.IsIsolated() {
		t.Error("IsIsolated does not distinguish the two")
	}
}

// Discard removes the writer's tree without touching the project.
func TestDiscardRemovesOnlyTheWritersTree(t *testing.T) {
	_, r := baseTree(t, map[string]string{"a.go": "package p\n\nconst X = 1\n"})
	before, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	iso := mustIsolate(t, r, "backend")
	if res, _ := iso.Execute(ToolPatchFile, `{"path":"a.go","old_content":"const X = 1","new_content":"const X = 9"}`); !strings.HasPrefix(res, "ok:") {
		t.Fatalf("patch: %q", res)
	}
	if err := iso.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, statErr := os.Stat(iso.root); !os.IsNotExist(statErr) {
		t.Error("the overlay survived Discard")
	}
	after, err := r.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("Discard changed the project tree")
	}
}
