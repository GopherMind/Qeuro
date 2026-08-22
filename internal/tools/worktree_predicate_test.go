package tools

import "testing"

// roadmap-v3 §4.1. TouchesWorkTree answers "can two agents doing this at once
// corrupt each other's work". The package already had two predicates near it, and
// the point of a third is that neither existing one gives that answer.

// The exact partition, named explicitly. A table rather than a loop over builtins
// so that adding a tool forces a decision here instead of inheriting the
// fail-closed default silently — and TestEveryBuiltinIsClassified below is what
// makes that forcing real.
func TestTouchesWorkTreePartitionsTheBuiltins(t *testing.T) {
	readers := []string{ToolReadFile, ToolListDir, ToolSearchCode, ToolRecall}
	writers := []string{ToolPatchFile, ToolWriteFile, ToolRunCommand, ToolRemember}

	for _, name := range readers {
		if TouchesWorkTree(name) {
			t.Errorf("%s is classified as a writer; it only reads", name)
		}
	}
	for _, name := range writers {
		if !TouchesWorkTree(name) {
			t.Errorf("%s is classified as read-only; it can change shared state", name)
		}
	}
	if len(readers)+len(writers) != len(builtins) {
		t.Fatalf("classified %d tools, there are %d builtins: a new tool needs a decision here",
			len(readers)+len(writers), len(builtins))
	}
}

// A tool the classification has never heard of is a writer. An MCP tool's effects
// are described only by the server hosting it, and .ai/RULES.md:22 forbids
// letting untrusted text settle an authorization question — so the default has to
// be the answer that costs concurrency rather than correctness.
func TestTouchesWorkTreeIsFailClosed(t *testing.T) {
	for _, name := range []string{
		"", "invented_tool", "read_file ", "READ_FILE",
		MCPName("github", "create_issue"),
	} {
		if !TouchesWorkTree(name) {
			t.Errorf("TouchesWorkTree(%q) = false; an unrecognized name must be treated as a writer", name)
		}
	}
}

// The reason this is not Mutating. Mutating drives the ✎ glyph and is
// deliberately false for run_command, because that tool has no undo record — but
// `go build -o x`, `npm install` and `git checkout` all rewrite the tree. A
// concurrency rule built on Mutating would let commands run in parallel, which is
// the single worst case: the tester's own build output racing four workers.
func TestTouchesWorkTreeDisagreesWithMutatingOnCommands(t *testing.T) {
	if Mutating(ToolRunCommand) {
		t.Fatal("run_command became Mutating; this test's premise needs rechecking")
	}
	if !TouchesWorkTree(ToolRunCommand) {
		t.Fatal("run_command is not a writer for concurrency purposes, but it can rewrite the tree")
	}
	if Mutating(ToolRemember) {
		t.Fatal("remember became Mutating; this test's premise needs rechecking")
	}
	if !TouchesWorkTree(ToolRemember) {
		t.Fatal("remember is not a writer for concurrency purposes, but it appends to .infinity/memory")
	}
}

// The reason this is not RequiresApproval either. Approval asks whether a human
// must confirm; it says nothing about whether concurrent use is safe. They agree
// today on the four read-only builtins and disagree on remember, which needs no
// human but does write.
func TestTouchesWorkTreeDisagreesWithApprovalOnRemember(t *testing.T) {
	if RequiresApproval(ToolRemember) {
		t.Fatal("remember started requiring approval; this test's premise needs rechecking")
	}
	if !TouchesWorkTree(ToolRemember) {
		t.Fatal("remember writes shared state and must count as a writer")
	}
}

// Every built-in has to be classified deliberately. Without this, adding a
// writing tool would inherit the fail-closed default and be correct by accident —
// and adding a reading tool would inherit it and be needlessly excluded from
// parallel runs, with no test failing either way.
func TestEveryBuiltinIsClassified(t *testing.T) {
	classified := map[string]bool{
		ToolReadFile: false, ToolListDir: false, ToolSearchCode: false, ToolRecall: false,
		ToolPatchFile: true, ToolWriteFile: true, ToolRunCommand: true, ToolRemember: true,
	}
	for _, s := range builtins {
		want, ok := classified[s.Name]
		if !ok {
			t.Errorf("builtin %s has no entry in the work-tree classification", s.Name)
			continue
		}
		if got := TouchesWorkTree(s.Name); got != want {
			t.Errorf("TouchesWorkTree(%s) = %v, want %v", s.Name, got, want)
		}
	}
}
