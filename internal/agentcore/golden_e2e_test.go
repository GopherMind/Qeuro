package agentcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/tools"
)

// TestGoldenE2EHeadlessRepairAndRollback is the Phase 0 P0-E proof for the
// headless engine. It deliberately uses a real, confined tools.Runner over a
// temporary repository, but a scripted provider: no user workspace, network,
// credentials, or external model is needed for this proof.
//
// The Runner checkpoint stack is deliberately process-local. This test proves
// that rollback restores a still-running Runner's workspace; it does not claim
// recovery after a process crash or a durable checkpoint implementation.
func TestGoldenE2EHeadlessRepairAndRollback(t *testing.T) {
	repo := goldenFixtureRepo(t)
	baseline := goldenTreeHash(t, repo)

	runner, err := tools.NewRunner(repo)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	const (
		originalSum = "return a + b"
		brokenSum   = "return a + b + 1"
		draftNote   = "Status: draft"
		changedNote = "Status: repaired"
	)
	provider := &fakeProvider{turns: [][]client.Event{
		toolTurn("read-module", tools.ToolReadFile, `{"path":"go.mod"}`),
		toolTurn("find-sum", tools.ToolSearchCode, `{"query":"return a + b","path":"."}`),
		{
			{Kind: client.EventToken, Text: "Plan: reproduce the test failure, repair it, then verify and report the diff."},
			{Kind: client.EventToolCalls, ToolCalls: []client.ToolCall{
				{ID: "break-sum", Function: client.FunctionCall{Name: tools.ToolPatchFile, Arguments: `{"path":"calc.go","old_content":"return a + b","new_content":"return a + b + 1"}`}},
				{ID: "change-note", Function: client.FunctionCall{Name: tools.ToolPatchFile, Arguments: `{"path":"notes.txt","old_content":"Status: draft","new_content":"Status: repaired"}`}},
			}},
		},
		toolTurn("observe-failure", tools.ToolRunCommand, `{"command":"go test ./..."}`),
		toolTurn("repair-sum", tools.ToolPatchFile, `{"path":"calc.go","old_content":"return a + b + 1","new_content":"return a + b"}`),
		toolTurn("verify-repair", tools.ToolRunCommand, `{"command":"go test ./..."}`),
		textTurn("Repair verified. The two-file diff is ready for review and can be rolled back."),
	}}
	approvals := make(chan HostCommand, 5)
	for _, id := range []string{"break-sum", "change-note", "observe-failure", "repair-sum", "verify-repair"} {
		approvals <- HostCommand{ID: id, Decision: "approve"}
	}

	var jsonl bytes.Buffer
	eng := &Engine{
		Emit:      NewEmitter(&jsonl, "golden-e2e"),
		Approvals: approvals,
		Deps:      Deps{Provider: provider, Runner: runner},
		Opts:      Options{WorkDir: repo},
	}
	if err := eng.Run(context.Background(), "Repair the fixture and prove the result"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := decodeEvents(t, &jsonl)

	assertGoldenToolTrace(t, events)
	assertGoldenTerminalEvent(t, events)

	changedSum := goldenRead(t, filepath.Join(repo, "calc.go"))
	changedNotes := goldenRead(t, filepath.Join(repo, "notes.txt"))
	if !strings.Contains(changedSum, originalSum) || strings.Contains(changedSum, brokenSum) {
		t.Fatalf("repair did not restore calc.go before rollback:\n%s", changedSum)
	}
	if !strings.Contains(changedNotes, changedNote) {
		t.Fatalf("second changed file is not present before rollback:\n%s", changedNotes)
	}
	if goldenTreeHash(t, repo) == baseline {
		t.Fatal("the repair's remaining notes.txt change was absent from the pre-rollback tree")
	}

	receipt := goldenReceipt(events)
	if !receipt.failureObserved || !receipt.repairVerified || receipt.status != DoneOK || len(receipt.changedPaths) != 2 {
		t.Fatalf("incomplete golden receipt: %+v", receipt)
	}

	// A new Runner proves rollback state is durable rather than an in-memory stack.
	restarted, err := tools.NewRunner(repo)
	if err != nil {
		t.Fatalf("NewRunner after simulated restart: %v", err)
	}
	for restarted.UndoDepth() > 0 {
		if _, ok := restarted.Undo(); !ok {
			t.Fatal("Undo reported no rollback while a durable checkpoint remained")
		}
	}
	if got := goldenTreeHash(t, repo); got != baseline {
		t.Fatalf("tree hash after rollback = %s, baseline = %s", got, baseline)
	}
	if got := goldenRead(t, filepath.Join(repo, "notes.txt")); !strings.Contains(got, draftNote) {
		t.Fatalf("notes.txt was not restored by rollback:\n%s", got)
	}
}

type goldenEvidence struct {
	status          string
	changedPaths    []string
	failureObserved bool
	repairVerified  bool
}

func goldenReceipt(events []Event) goldenEvidence {
	var out goldenEvidence
	seenPaths := make(map[string]bool)
	for _, ev := range events {
		switch ev.Kind {
		case KindDone:
			out.status = ev.Status
		case KindFileWrite:
			if ev.Path != "" && !seenPaths[ev.Path] {
				seenPaths[ev.Path] = true
				out.changedPaths = append(out.changedPaths, ev.Path)
			}
		case KindCommand:
			if ev.ExitCode != nil && *ev.ExitCode != 0 {
				out.failureObserved = true
			}
			if ev.ExitCode != nil && *ev.ExitCode == 0 {
				out.repairVerified = true
			}
		}
	}
	sort.Strings(out.changedPaths)
	return out
}

func assertGoldenToolTrace(t *testing.T, events []Event) {
	t.Helper()
	var read, search, failedTest, passedTest bool
	for _, ev := range events {
		switch {
		case ev.Kind == KindToolCall && ev.Name == tools.ToolReadFile:
			read = true
		case ev.Kind == KindToolCall && ev.Name == tools.ToolSearchCode:
			search = true
		case ev.Kind == KindCommand && ev.ExitCode != nil && *ev.ExitCode != 0:
			failedTest = true
		case ev.Kind == KindCommand && ev.ExitCode != nil && *ev.ExitCode == 0:
			passedTest = true
		}
	}
	if !read || !search || !failedTest || !passedTest {
		t.Fatalf("incomplete tool trace: read=%t search=%t failed-test=%t passed-test=%t; events=%v", read, search, failedTest, passedTest, kinds(events))
	}
}

func assertGoldenTerminalEvent(t *testing.T, events []Event) {
	t.Helper()
	var done []Event
	for _, ev := range events {
		if ev.Kind == KindDone {
			done = append(done, ev)
		}
	}
	if len(done) != 1 || done[0].Status != DoneOK {
		t.Fatalf("terminal events = %+v, want exactly one done/ok", done)
	}
}

func goldenFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{
		"go.mod":       "module goldenfixture\n\ngo 1.26\n",
		"calc.go":      "package goldenfixture\n\nfunc Sum(a, b int) int {\n\treturn a + b\n}\n",
		"calc_test.go": "package goldenfixture\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif got := Sum(1, 2); got != 3 {\n\t\tt.Fatalf(\"Sum(1, 2) = %d, want 3\", got)\n\t}\n}\n",
		"notes.txt":    "Title: Golden fixture\nOwner: test\nStatus: draft\nLine: 4\nLine: 5\nLine: 6\nLine: 7\nLine: 8\nLine: 9\nLine: 10\nLine: 11\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return repo
}

func goldenTreeHash(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".infinity/checkpoints" || strings.HasPrefix(relSlash, ".infinity/checkpoints/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, relSlash+"\x00"+string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("hash fixture tree: %v", err)
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, entry := range entries {
		_, _ = fmt.Fprintln(h, entry)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func goldenRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
