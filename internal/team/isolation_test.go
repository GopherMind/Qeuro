package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"qeuro/internal/tools"
)

// roadmap-v3 §4.1 (C-1), engine half. The tools package proves that an isolated
// worktree contains a writer's edits; these tests prove the engine actually hands
// one to every writer in a parallel fan-out, integrates them in plan order, and
// falls back to read-only rather than to shared writing when it cannot.

// treeEngine is testEngine with a real project tree, so the isolation path runs.
func treeEngine(t *testing.T, maxWorkers int, files map[string]string) (*Engine, string, func() []Event) {
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
	runner, err := tools.NewRunner(dir)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []Event
	e := New(nil, runner, nil, Profile{Name: "test", MaxWorkers: maxWorkers}, func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}, nil)

	return e, dir, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), events...)
	}
}

// Every writer in a parallel fan-out gets a tree of its own, and no two writers get
// the same one. This is the gate's "два независимых writer-узла не делят
// mutable-дерево" at the level where the trees are actually handed out.
func TestEveryParallelWriterGetsItsOwnTree(t *testing.T) {
	e, _, _ := treeEngine(t, 4, map[string]string{"a.go": "package p\n"})

	var mu sync.Mutex
	roots := map[string]bool{}
	nilRunners := 0
	dispatchWriters(e, context.Background(), subtasks("backend", "frontend", "db", "docs"), roleOf,
		func(_ Subtask, runner *tools.Runner) {
			mu.Lock()
			defer mu.Unlock()
			if runner == nil {
				nilRunners++
				return
			}
			if !runner.IsIsolated() {
				t.Error("a writer was handed the project runner during a parallel fan-out")
			}
			roots[runner.Root()] = true
		})

	if nilRunners != 0 {
		t.Fatalf("%d writers shared the project tree", nilRunners)
	}
	if len(roots) != 4 {
		t.Fatalf("4 writers received %d distinct trees", len(roots))
	}
}

// A lone writer keeps the project tree: there is nothing to isolate it from, and an
// overlay would only add a copy-up per file to reach the same result.
func TestLoneWriterKeepsTheProjectTree(t *testing.T) {
	e, _, _ := treeEngine(t, 4, map[string]string{"a.go": "package p\n"})

	seen := 0
	dispatchWriters(e, context.Background(), subtasks("only"), roleOf,
		func(_ Subtask, runner *tools.Runner) {
			if runner != nil {
				t.Error("a lone writer was given an overlay")
			}
			seen++
		})
	if seen != 1 {
		t.Fatalf("the lone writer ran %d times", seen)
	}
}

// The end-to-end property: two writers edit different files concurrently, and both
// edits are in the project tree afterwards. Before isolation exactly one survived
// (ledger §40.2).
func TestBothWritersEditsSurviveIntegration(t *testing.T) {
	e, dir, events := treeEngine(t, 4, map[string]string{
		"a.go": "package p\n\nfunc A() {}\n\nfunc B() {}\n" + strings.Repeat("// filler\n", 40),
		"b.go": "package p\n\nconst Y = 1\n",
	})

	dispatchWriters(e, context.Background(), subtasks("backend", "frontend"), roleOf,
		func(st Subtask, runner *tools.Runner) {
			if runner == nil {
				t.Errorf("%s: no isolated tree; the writers are sharing the project tree", st.Role)
				return
			}
			args := `{"path":"a.go","old_content":"func A() {}","new_content":"func A() { println(1) }"}`
			if st.Role == "frontend" {
				args = `{"path":"b.go","old_content":"const Y = 1","new_content":"const Y = 2"}`
			}
			if res, _ := runner.Execute(tools.ToolPatchFile, args); !strings.HasPrefix(res, "ok:") {
				t.Errorf("%s: patch failed: %q", st.Role, res)
			}
		})

	a, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "b.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a), "println(1)") {
		t.Error("the first writer's edit is missing from the project tree")
	}
	if !strings.Contains(string(b), "const Y = 2") {
		t.Error("the second writer's edit is missing from the project tree")
	}

	var integrated string
	for _, ev := range events() {
		if strings.Contains(ev.Text, "integrated") {
			integrated = ev.Text
		}
	}
	if integrated == "" {
		t.Error("integration was silent; the user cannot tell whether changes were applied")
	}
}

// Conflicting writers: nothing is applied, and the user is told which path. Picking
// a winner silently is the defect the row exists to remove.
func TestConflictingWritersApplyNothingAndAreReported(t *testing.T) {
	e, dir, events := treeEngine(t, 4, map[string]string{
		"a.go": "package p\n\nfunc A() {}\n\nfunc B() {}\n" + strings.Repeat("// filler\n", 40),
	})
	before, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}

	dispatchWriters(e, context.Background(), subtasks("backend", "frontend"), roleOf,
		func(st Subtask, runner *tools.Runner) {
			if runner == nil {
				t.Errorf("%s: no isolated tree; the writers are sharing the project tree", st.Role)
				return
			}
			args := `{"path":"a.go","old_content":"func A() {}","new_content":"func A() { println(1) }"}`
			if st.Role == "frontend" {
				args = `{"path":"a.go","old_content":"func B() {}","new_content":"func B() { println(2) }"}`
			}
			if res, _ := runner.Execute(tools.ToolPatchFile, args); !strings.HasPrefix(res, "ok:") {
				t.Errorf("%s: patch failed: %q", st.Role, res)
			}
		})

	after, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a conflicting change was applied to the project tree")
	}

	var reported string
	for _, ev := range events() {
		if ev.Kind == EvError && strings.Contains(ev.Text, "conflicting writers") {
			reported = ev.Text
		}
	}
	if reported == "" {
		t.Fatal("the conflict was not reported to the user")
	}
	for _, want := range []string{"a.go", "backend", "frontend", "nothing was applied"} {
		if !strings.Contains(reported, want) {
			t.Errorf("the report %q does not mention %q", reported, want)
		}
	}
}

// When isolation cannot be established the step must run read-only, not fall back
// to shared writing. The fallback is forced by making the isolation directory
// impossible to create: a regular file sits where the directory must go.
func TestIsolationFailureFallsBackToReadOnly(t *testing.T) {
	e, dir, events := treeEngine(t, 4, map[string]string{"a.go": "package p\n"})
	if err := os.MkdirAll(filepath.Join(dir, ".infinity"), 0o755); err != nil {
		t.Fatal(err)
	}
	// .infinity/worktrees is a file, so MkdirAll under it must fail.
	if err := os.WriteFile(filepath.Join(dir, ".infinity", "worktrees"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	refused := 0
	var mu sync.Mutex
	dispatchWriters(e, context.Background(), subtasks("backend", "frontend"), roleOf,
		func(_ Subtask, runner *tools.Runner) {
			mu.Lock()
			defer mu.Unlock()
			if runner != nil {
				t.Error("a writer got an overlay even though isolation failed")
			}
			// The §40 restriction is what must be in force on this path.
			worker := agentSpec{role: "w", allowTools: workerTools, autoApprove: true}
			if e.toolGate(worker, call(tools.ToolPatchFile, `{"path":"a.go","old_content":"package p","new_content":"package q"}`)) != "" {
				refused++
			}
		})

	if refused != 2 {
		t.Fatalf("%d of 2 writers were refused a write after isolation failed, want both", refused)
	}
	var told string
	for _, ev := range events() {
		if strings.Contains(ev.Text, "read-only") {
			told = ev.Text
		}
	}
	if told == "" {
		t.Fatal("the fallback to read-only was silent")
	}
	// The restriction must not latch: the sequential tester runs after this.
	if e.sharedTree.Load() {
		t.Error("the shared-tree restriction stayed on after the fan-out")
	}
}

// The isolation announcement is part of the row's "explicit behaviour" requirement:
// a user has to be able to tell that changes were collected and applied afterwards
// rather than written as the workers went.
func TestIsolationIsAnnounced(t *testing.T) {
	e, _, events := treeEngine(t, 4, map[string]string{"a.go": "package p\n"})
	dispatchWriters(e, context.Background(), subtasks("backend", "frontend"), roleOf,
		func(_ Subtask, _ *tools.Runner) {})

	var told string
	for _, ev := range events() {
		if strings.Contains(ev.Text, "isolation:") {
			told = ev.Text
		}
	}
	if told == "" {
		t.Fatal("a run with isolated writers did not say so")
	}
	for _, want := range []string{"own tree", "integrated"} {
		if !strings.Contains(told, want) {
			t.Errorf("the announcement %q does not mention %q", told, want)
		}
	}
}

// Overlays must not survive the step. A leftover tree would be integrated by a
// later fan-out that never wrote it.
func TestOverlaysAreRemovedAfterTheStep(t *testing.T) {
	e, dir, _ := treeEngine(t, 4, map[string]string{"a.go": "package p\n"})
	dispatchWriters(e, context.Background(), subtasks("backend", "frontend"), roleOf,
		func(_ Subtask, runner *tools.Runner) {
			if runner == nil {
				t.Error("no isolated tree; writers are sharing the project tree")
				return
			}
			if res, _ := runner.Execute(tools.ToolWriteFile, `{"path":"new.go","content":"package p\n"}`); !strings.HasPrefix(res, "ok:") {
				t.Errorf("write failed: %q", res)
			}
		})

	worktrees := filepath.Join(dir, ".infinity", "worktrees")
	entries, err := os.ReadDir(worktrees)
	if err != nil {
		if os.IsNotExist(err) {
			return // removed entirely, which is fine
		}
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("overlays survived the step: %v", names)
	}
}

// Two subtasks can carry the same role, and they must still get separate trees —
// otherwise two writers would share one overlay, which is the original defect with
// an extra step.
func TestSameRoleTwiceStillGetsTwoTrees(t *testing.T) {
	e, _, _ := treeEngine(t, 4, map[string]string{"a.go": "package p\n"})

	var mu sync.Mutex
	roots := map[string]bool{}
	dispatchWriters(e, context.Background(), subtasks("backend", "backend"), roleOf,
		func(_ Subtask, runner *tools.Runner) {
			mu.Lock()
			defer mu.Unlock()
			if runner == nil {
				t.Error("a writer shared the project tree")
				return
			}
			roots[runner.Root()] = true
		})
	if len(roots) != 2 {
		t.Fatalf("two same-role writers received %d distinct trees, want 2", len(roots))
	}
}
