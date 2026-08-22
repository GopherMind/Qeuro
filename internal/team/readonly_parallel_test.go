package team

import (
	"context"
	"strings"
	"testing"

	"qeuro/internal/tools"
)

// roadmap-v3 §4.1 (C-1). The engine holds one *tools.Runner and dispatch() runs
// its items concurrently, so a fan-out of writers is several agents editing one
// tree. Until each writer gets its own tree, a parallel step is read-only —
// explicitly, with a message, not by silently permitting the write.
//
// These tests drive toolGate() and dispatch() rather than a copy of either,
// because those two functions are the whole policy: the gate is the only thing
// between a model's tool call and the disk in team mode (there is no human to
// approve), and dispatch is the only place concurrency is created.

// The positive case: while a fan-out is in flight, a worker holding every write
// grant the code can express is still refused. autoApprove is what normally lets
// a worker write without a human, so a spec with it set is the strongest
// configuration a role has — if the restriction did not bind here it would not
// bind anywhere.
func TestSharedTreeRefusesWritersDuringFanOut(t *testing.T) {
	e, _ := testEngine(t, 4)
	e.sharedTree.Store(true)

	worker := agentSpec{role: "backend", allowTools: workerTools, autoApprove: true}
	for _, name := range []string{tools.ToolPatchFile, tools.ToolWriteFile, tools.ToolRemember} {
		skip := e.toolGate(worker, call(name, `{"path":"x","content":"y","old_content":"a","new_content":"b","category":"c","note":"n"}`))
		if skip == "" {
			t.Fatalf("%s was permitted while workers shared one tree", name)
		}
		if !strings.Contains(skip, "sharing one tree") {
			t.Fatalf("%s refused with %q, want the shared tree named as the reason", name, skip)
		}
	}

	// The tester's grant is separate and just as strong for this purpose: a
	// command can rewrite the tree without going through the file tools at all.
	tester := agentSpec{role: "tester", allowTools: testerTools, allowCommands: true}
	skip := e.toolGate(tester, call(tools.ToolRunCommand, `{"command":"go build ./..."}`))
	if skip == "" {
		t.Fatal("run_command was permitted while agents shared one tree")
	}
	if !strings.Contains(skip, "sharing one tree") {
		t.Fatalf("run_command refused with %q, want the shared tree named", skip)
	}
}

// The refusal must not cost the read half. §4.1 says read-only nodes may share a
// snapshot precisely because they change nothing, so a restriction that also
// blocked reads would make a parallel run useless rather than safe.
func TestSharedTreeStillAllowsReaders(t *testing.T) {
	e, _ := testEngine(t, 4)
	e.sharedTree.Store(true)

	worker := agentSpec{role: "backend", allowTools: workerTools, autoApprove: true}
	for _, name := range []string{tools.ToolReadFile, tools.ToolListDir, tools.ToolSearchCode, tools.ToolRecall} {
		if skip := e.toolGate(worker, call(name, `{"path":"x","query":"q"}`)); skip != "" {
			t.Fatalf("%s was refused during a read-only parallel step: %q", name, skip)
		}
	}
}

// The negative case, and the one that makes the restriction a restriction rather
// than a mode: with no fan-out in flight, the same worker with the same grants
// writes. A test that only checked the refusal would pass just as well against
// "team mode never writes", which is a different and much worse product.
func TestWriterAllowedWhenTreeIsNotShared(t *testing.T) {
	e, _ := testEngine(t, 4)

	worker := agentSpec{role: "backend", allowTools: workerTools, autoApprove: true}
	for _, name := range []string{tools.ToolPatchFile, tools.ToolWriteFile, tools.ToolRemember} {
		if skip := e.toolGate(worker, call(name, `{"path":"x","content":"y","old_content":"a","new_content":"b","category":"c","note":"n"}`)); skip != "" {
			t.Fatalf("%s was refused outside a fan-out: %q", name, skip)
		}
	}

	tester := agentSpec{role: "tester", allowTools: testerTools, allowCommands: true}
	if skip := e.toolGate(tester, call(tools.ToolRunCommand, `{"command":"go test ./..."}`)); skip != "" {
		t.Fatalf("the tester was refused a benign command outside a fan-out: %q", skip)
	}
}

// dispatch() is what decides the flag, and it needs both halves of the condition.
// A profile that permits four workers but a plan with one subtask has no
// concurrency to restrict: the single agent owns the tree, and refusing its
// writes would break the ordinary interactive plan for no gain.
func TestSingleItemFanOutKeepsTheTreeWritable(t *testing.T) {
	e, events := testEngine(t, 4)

	var sawWritable bool
	dispatch(e, context.Background(), subtasks("only"), roleOf, func(Subtask) {
		worker := agentSpec{role: "only", allowTools: workerTools, autoApprove: true}
		sawWritable = e.toolGate(worker, call(tools.ToolWriteFile, `{"path":"x","content":"y"}`)) == ""
	})

	if !sawWritable {
		t.Fatal("a lone worker was refused a write; the restriction must need two agents, not just a high cap")
	}
	for _, ev := range events() {
		if strings.Contains(ev.Text, "read-only") {
			t.Fatalf("a single-item fan-out announced the read-only restriction: %q", ev.Text)
		}
	}
}

// The other half: MaxWorkers=1 is strictly sequential (TestParallelMaxWorkersOne
// pins that), so many items still means one agent in the tree at a time and
// writing stays allowed. Without this, "any fan-out is read-only" would pass —
// and that would silently disable writing for every free-tier headless run at
// --parallel 1.
func TestSequentialFanOutKeepsTheTreeWritable(t *testing.T) {
	e, events := testEngine(t, 1)

	writable := 0
	dispatch(e, context.Background(), subtasks("a", "b", "c"), roleOf, func(Subtask) {
		worker := agentSpec{role: "w", allowTools: workerTools, autoApprove: true}
		if e.toolGate(worker, call(tools.ToolWriteFile, `{"path":"x","content":"y"}`)) == "" {
			writable++
		}
	})

	if writable != 3 {
		t.Fatalf("%d of 3 sequential workers could write, want all of them", writable)
	}
	for _, ev := range events() {
		if strings.Contains(ev.Text, "read-only") {
			t.Fatalf("a sequential run announced the read-only restriction: %q", ev.Text)
		}
	}
}

// A real fan-out: the flag must be observable from inside the work function,
// which is where an agent's tool calls actually happen. Asserting on the field
// directly after dispatch returned would prove nothing — it is cleared by then.
func TestFanOutIsReadOnlyWhenTheTreeIsShared(t *testing.T) {
	e, events := testEngine(t, 4)

	// dispatchWriters sets this when it could not give each writer its own tree;
	// the fallback path is asserted end-to-end in TestIsolationFailureFallsBackToReadOnly.
	// Here the concern is that a fan-out running under the flag refuses every writer.
	e.sharedTree.Store(true)
	refused := 0
	dispatch(e, context.Background(), subtasks("a", "b", "c", "d"), roleOf, func(Subtask) {
		worker := agentSpec{role: "w", allowTools: workerTools, autoApprove: true}
		if e.toolGate(worker, call(tools.ToolPatchFile, `{"path":"x","old_content":"a","new_content":"b"}`)) != "" {
			refused++
		}
	})
	if refused != 4 {
		t.Fatalf("%d of 4 concurrent workers were refused a write, want all of them", refused)
	}
	_ = events
}

// The restriction is a window, not a latch. The tester runs after dispatch has
// returned, sequentially, and running the project's tests is its entire job — so
// the flag has to be cleared on the way out, including when the fan-out stopped
// early on an empty balance.
func TestTreeBecomesWritableAgainAfterTheFanOut(t *testing.T) {
	e, _ := testEngine(t, 4)

	dispatch(e, context.Background(), subtasks("a", "b", "c"), roleOf, func(Subtask) {})

	tester := agentSpec{role: "tester", allowTools: testerTools, allowCommands: true}
	if skip := e.toolGate(tester, call(tools.ToolRunCommand, `{"command":"go test ./..."}`)); skip != "" {
		t.Fatalf("the tester could not run tests after the fan-out finished: %q", skip)
	}

	// Same requirement on the early-stop path.
	depleted, _ := testEngine(t, 4)
	depleted.setBalance(0)
	if !dispatch(depleted, context.Background(), subtasks("a", "b", "c"), roleOf, func(Subtask) {}) {
		t.Fatal("a zero balance did not stop the fan-out; this test is not exercising the early-stop path")
	}
	if skip := depleted.toolGate(tester, call(tools.ToolRunCommand, `{"command":"go test ./..."}`)); skip != "" {
		t.Fatalf("the tree stayed locked after an early-stopped fan-out: %q", skip)
	}
}

// The §0.3 rollout flag has to actually lift the restriction, or it is not a
// rollout flag — a restriction with no way out is a hard-coded behaviour change
// with no rollback criterion.
func TestRolloutFlagSkipsIsolationAndWarns(t *testing.T) {
	e, events := testEngine(t, 4)
	e.AllowUnsafeParallelWrites(true)

	// With the flag set the writers share the project tree, so no runner is handed
	// to them and nothing refuses their writes.
	shared := 0
	dispatchWriters(e, context.Background(), subtasks("a", "b", "c", "d"), roleOf,
		func(_ Subtask, runner *tools.Runner) {
			if runner == nil {
				shared++
			}
		})
	if shared != 4 {
		t.Fatalf("%d of 4 writers shared the project tree, want all of them", shared)
	}

	// And it must say so: this is the configuration that loses edits.
	var announced string
	for _, ev := range events() {
		if strings.Contains(ev.Text, "unsafe") {
			announced = ev.Text
		}
	}
	if announced == "" {
		t.Fatal("the run skipped isolation without warning the user")
	}
	if !strings.Contains(announced, "unsafe_parallel_writes") {
		t.Fatalf("the warning %q does not name the setting that caused it", announced)
	}
	if !strings.Contains(announced, "lost") {
		t.Fatalf("the warning %q does not say an edit can be lost", announced)
	}
}

// The flag defaults off in the engine too, not only in the config loader: an
// Engine built and used without touching the setter must be restricted. Without
// this, a caller that forgot the wiring would get concurrent writers.
func TestIsolationIsOnByDefaultInTheEngine(t *testing.T) {
	e, _ := testEngine(t, 4)
	if e.unsafeParallelWrites {
		t.Fatal("a freshly built Engine skips isolation")
	}
	if e.sharedTree.Load() {
		t.Fatal("a freshly built Engine starts with the shared-tree restriction latched on")
	}
}

// The refusal is read by a model and rendered to the user's terminal, and the
// tool name in it comes from model output (.ai/RULES.md:22). The gate reaches
// this branch before the name is validated as a real tool, so an invented name
// with escape sequences in it must not travel through.
func TestSharedTreeRefusalSanitizesTheName(t *testing.T) {
	e, _ := testEngine(t, 4)
	e.sharedTree.Store(true)

	worker := agentSpec{role: "w", allowTools: workerTools, autoApprove: true}
	skip := e.toolGate(worker, call("write\x1b[2Jfile", `{}`))
	if skip == "" {
		t.Fatal("an invented writing name was permitted during a fan-out")
	}
	if strings.ContainsRune(skip, 0x1b) {
		t.Fatalf("the refusal carried an escape character: %q", skip)
	}
}

// An MCP name reaching the gate during a fan-out must be refused for the tree,
// not waved through to the MCP branch: TouchesWorkTree is fail-closed for
// anything that is not a known read-only built-in, and the reason is that the
// only description of an MCP tool's effects comes from the server hosting it.
func TestSharedTreeRefusesUnknownAndMCPNames(t *testing.T) {
	e, _ := testEngine(t, 4)
	e.sharedTree.Store(true)

	spec := agentSpec{
		role:          "w",
		allowTools:    []string{"mcp__github__create_issue", "invented_tool"},
		autoApprove:   true,
		allowCommands: true,
	}
	for _, name := range []string{"mcp__github__create_issue", "invented_tool"} {
		if skip := e.toolGate(spec, call(name, `{}`)); skip == "" {
			t.Fatalf("%s ran during a read-only parallel step", name)
		}
	}
}
