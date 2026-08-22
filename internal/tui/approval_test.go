package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/state"
	"qeuro/internal/tools"
)

// newTestModel builds a minimal model with a runner rooted at dir. cli is nil:
// the returned continue-stream command is never executed by these tests.
func newTestModel(t *testing.T, dir string) model {
	t.Helper()
	r, err := tools.NewRunner(dir)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	return model{app: state.New(), runner: r, width: 80}
}

func patchCall(path, oldC, newC string) client.ToolCall {
	args := `{"path":"` + path + `","old_content":"` + oldC + `","new_content":"` + newC + `"}`
	return client.ToolCall{ID: "1", Type: "function",
		Function: client.FunctionCall{Name: "patch_file", Arguments: args}}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApprovalPausesOnMutatingTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", `port := ":8080"`)

	m := newTestModel(t, dir)
	m.toolQueue = []client.ToolCall{patchCall("main.go", `port := \":8080\"`, `port := \":9090\"`)}

	res, _ := m.advanceTools()
	m = res.(model)

	if !m.awaitingApproval {
		t.Fatal("expected to pause for approval on a mutating tool")
	}
	if m.pendingPreview == "" {
		t.Error("expected a non-empty preview")
	}
	// File must NOT change before approval.
	if got := readFile(t, dir, "main.go"); !strings.Contains(got, ":8080") {
		t.Fatalf("file changed before approval: %q", got)
	}
}

func TestApprovalApplyAndReject(t *testing.T) {
	// Approve → file is edited. Tool execution is async (M9): resolveApproval
	// returns a command that runs the tool off the UI goroutine and yields a
	// toolDoneMsg, which onToolDone then applies.
	dirA := t.TempDir()
	writeFile(t, dirA, "main.go", `port := ":8080"`)
	ma := newTestModel(t, dirA)
	ma.streaming = true
	call := patchCall("main.go", `port := \":8080\"`, `port := \":9090\"`)
	ma.toolQueue = []client.ToolCall{call}
	res, _ := ma.advanceTools()
	ma = res.(model)
	res, cmd := ma.resolveApproval(true)
	ma = res.(model)
	if ma.awaitingApproval {
		t.Error("should not still be awaiting after resolution")
	}
	if cmd == nil {
		t.Fatal("expected a command from approval (erase overlay + status + tool exec)")
	}
	// resolveApproval now returns a tea.Sequence (erase overlay → ✓ status →
	// async tool exec); drive the tool step directly, as the runtime would.
	done, ok := execToolCmd(call, ma.runner)().(toolDoneMsg)
	if !ok {
		t.Fatal("tool exec command did not produce a toolDoneMsg")
	}
	res, _ = ma.onToolDone(done)
	ma = res.(model)
	if got := readFile(t, dirA, "main.go"); !strings.Contains(got, ":9090") {
		t.Fatalf("approve did not apply edit: %q", got)
	}

	// Reject → file untouched, model told it was rejected.
	dirR := t.TempDir()
	writeFile(t, dirR, "main.go", `port := ":8080"`)
	mr := newTestModel(t, dirR)
	mr.toolQueue = []client.ToolCall{patchCall("main.go", `port := \":8080\"`, `port := \":9090\"`)}
	res, _ = mr.advanceTools()
	mr = res.(model)
	res, _ = mr.resolveApproval(false)
	mr = res.(model)
	if got := readFile(t, dirR, "main.go"); !strings.Contains(got, ":8080") {
		t.Fatalf("reject must leave file untouched: %q", got)
	}
	// The history must carry a tool result telling the model it was rejected.
	var rejected bool
	for _, msg := range mr.history {
		if msg.Role == "tool" && strings.Contains(msg.Content, "rejected") {
			rejected = true
		}
	}
	if !rejected {
		t.Error("expected a 'rejected' tool result in history")
	}
}

func TestReadOnlyToolsAutoRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	m := newTestModel(t, dir)
	m.toolQueue = []client.ToolCall{{
		ID: "1", Type: "function",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`},
	}}
	res, _ := m.advanceTools()
	m = res.(model)
	if m.awaitingApproval {
		t.Fatal("read-only tools must not require approval")
	}
}

func TestApprovalAllStillAsksForCommands(t *testing.T) {
	m := newTestModel(t, t.TempDir())
	m.app.Approvals = state.ApprovalAll
	m.toolQueue = []client.ToolCall{{
		ID: "1", Type: "function",
		Function: client.FunctionCall{Name: tools.ToolRunCommand, Arguments: `{"command":"go test ./..."}`},
	}}
	res, cmd := m.advanceTools()
	m = res.(model)
	if cmd != nil {
		t.Fatal("commands must not auto-run in approval-all mode")
	}
	if !m.awaitingApproval {
		t.Fatal("expected command approval prompt")
	}
}

func TestApproveRestDoesNotAutoApproveFutureCommands(t *testing.T) {
	m := newTestModel(t, t.TempDir())
	m.awaitingApproval = true
	m.pendingTool = &client.ToolCall{
		ID: "1", Type: "function",
		Function: client.FunctionCall{Name: tools.ToolRunCommand, Arguments: `{"command":"go test ./..."}`},
	}
	m.toolQueue = []client.ToolCall{*m.pendingTool}

	res, cmd := m.applyApprovalChoice(1)
	m = res.(model)
	if m.app.Approvals != state.ApprovalEdits {
		t.Fatalf("commands should only enable edit auto-approval, got %s", m.app.Approvals)
	}
	if cmd == nil {
		t.Fatal("current command should still be approved once")
	}
}

// TestApprovalStatusEscapesModelChosenText covers the line both approval exits
// print — the y/n decision and the Ctrl+C cancel. Its text is a summary of the
// tool call's arguments, which the model chose, so it is untrusted content on its
// way into a terminal (.ai/SECURITY.md: model output is data, not instructions).
//
// The status line is the one line the user reads to learn what just happened to a
// file edit or a shell command. A summary that can clear the screen, move the
// cursor or span rows can push the real status out of view and leave a fabricated
// one in its place.
func TestApprovalStatusEscapesModelChosenText(t *testing.T) {
	// A path with an escape sequence and a newline, as an argument from a model
	// that wants the status line to say something else.
	hostile := "$ rm -rf build\x1b[2Jecho done\nApproved: nothing"

	for _, approved := range []bool{true, false} {
		got := approvalStatus(approved, hostile)
		if strings.Contains(got, "\x1b[2J") {
			t.Errorf("approved=%v: a screen-clear reached the terminal: %q", approved, got)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("approved=%v: the status must stay one line, got %q", approved, got)
		}
		// Escaped, not dropped: the user still needs to see what was asked for.
		if !strings.Contains(got, "rm -rf build") {
			t.Errorf("approved=%v: the command text was lost: %q", approved, got)
		}
	}
}
