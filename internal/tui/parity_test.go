package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/agentcore"
	"qeuro/internal/client"
	"qeuro/internal/tools"
)

// TestTUIHeadlessParityGoldenFixture proves that the TUI solo agent host and
// the headless agentcore.Engine produce equivalent tool outcomes, tree hash,
// and rollback baseline when driven by the same scripted provider over the
// same fixture repository. This is the Phase 0 parity gate: one agent runtime
// with two presentation layers.
//
// The test deliberately uses the real confined tools.Runner checkpoint stack
// and the same golden fixture as TestGoldenE2EHeadlessRepairAndRollback.
func TestTUIHeadlessParityGoldenFixture(t *testing.T) {
	repo := parityFixtureRepo(t)
	baseline := parityTreeHash(t, repo)

	// Headless path: run the engine directly with JSONL emission.
	headlessRepo := parityFixtureRepo(t)
	headlessRunner, err := tools.NewRunner(headlessRepo)
	if err != nil {
		t.Fatalf("headless NewRunner: %v", err)
	}

	headlessProvider := parityScriptedProvider()
	headlessApprovals := make(chan agentcore.HostCommand, 5)
	for _, id := range []string{"break-sum", "change-note", "observe-failure", "repair-sum", "verify-repair"} {
		headlessApprovals <- agentcore.HostCommand{ID: id, Decision: "approve"}
	}

	var headlessJSONL bytes.Buffer
	headlessEngine := &agentcore.Engine{
		Emit:      agentcore.NewEmitter(&headlessJSONL, "headless-parity"),
		Approvals: headlessApprovals,
		Deps:      agentcore.Deps{Provider: headlessProvider, Runner: headlessRunner},
		Opts:      agentcore.Options{WorkDir: headlessRepo},
	}
	if err := headlessEngine.Run(context.Background(), "Repair the fixture and prove the result"); err != nil {
		t.Fatalf("headless Run: %v", err)
	}
	headlessEvents := decodeParityEvents(t, &headlessJSONL)
	headlessReceipt := parityReceipt(headlessEvents)

	if headlessReceipt.status != agentcore.DoneOK {
		t.Fatalf("headless status = %s, want ok", headlessReceipt.status)
	}
	if !headlessReceipt.failureObserved || !headlessReceipt.repairVerified {
		t.Fatalf("headless receipt incomplete: %+v", headlessReceipt)
	}

	// Rollback headless
	headlessRestarted, err := tools.NewRunner(headlessRepo)
	if err != nil {
		t.Fatalf("headless rollback NewRunner: %v", err)
	}
	for headlessRestarted.UndoDepth() > 0 {
		if _, ok := headlessRestarted.Undo(); !ok {
			t.Fatal("headless rollback failed")
		}
	}
	headlessRolledBack := parityTreeHash(t, headlessRepo)
	if headlessRolledBack != baseline {
		t.Fatalf("headless rollback hash = %s, baseline = %s", headlessRolledBack, baseline)
	}

	// TUI path: drive the model through the agentHost adapter.
	tuiRepo := parityFixtureRepo(t)
	tuiRunner, err := tools.NewRunner(tuiRepo)
	if err != nil {
		t.Fatalf("tui NewRunner: %v", err)
	}

	tuiProvider := parityScriptedProvider()
	m := newModelWithFlags("test-parity", map[string]string{})
	m.width = 80
	m.runner = tuiRunner
	m.provider = tuiProvider
	m.history = nil
	m.turnStartIndex = -1
	closeJournal(t, &m)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	host, cmd := startAgentHost(ctx, tuiProvider, tuiRunner, "Repair the fixture and prove the result", "test-model", 0)
	m.agentHost = host
	m.turnCtx = ctx
	m.turnCancel = cancel

	// Drive the TUI event pump and approval decisions until done.
	approvalsSent := 0
	approvalsNeeded := []string{"break-sum", "change-note", "observe-failure", "repair-sum", "verify-repair"}
	var tuiEvents []agentcore.Event

	for {
		msg := cmd()
		if msg == nil {
			t.Fatal("nil message from TUI event pump")
		}

		switch msg := msg.(type) {
		case agentEventMsg:
			tuiEvents = append(tuiEvents, msg.ev)
			var next tea.Model
			var nextCmd tea.Cmd
			next, nextCmd = m.Update(msg)
			m = next.(model)
			if msg.ev.Kind == agentcore.KindApprovalRequest {
				if approvalsSent >= len(approvalsNeeded) {
					t.Fatalf("unexpected approval request: %s", msg.ev.ID)
				}
				expectedID := approvalsNeeded[approvalsSent]
				if msg.ev.ID != expectedID {
					t.Fatalf("approval id = %s, want %s", msg.ev.ID, expectedID)
				}
				approved, approvalCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				m = approved.(model)
				approvalsSent++
				cmd = approvalCmd
			} else {
				cmd = nextCmd
			}

		case agentDoneMsg:
			tuiEvents = append(tuiEvents, agentcore.Event{Kind: agentcore.KindDone, Status: msg.status})
			next, doneCmd := m.Update(msg)
			m = next.(model)
			_ = doneCmd
			tuiReceipt := parityReceipt(tuiEvents)
			if tuiReceipt.status != agentcore.DoneOK {
				t.Fatalf("tui status = %s, want ok", tuiReceipt.status)
			}
			if !tuiReceipt.failureObserved || !tuiReceipt.repairVerified {
				t.Fatalf("tui receipt incomplete: %+v", tuiReceipt)
			}

			// Rollback TUI
			tuiRestarted, err := tools.NewRunner(tuiRepo)
			if err != nil {
				t.Fatalf("tui rollback NewRunner: %v", err)
			}
			for tuiRestarted.UndoDepth() > 0 {
				if _, ok := tuiRestarted.Undo(); !ok {
					t.Fatal("tui rollback failed")
				}
			}
			tuiRolledBack := parityTreeHash(t, tuiRepo)
			if tuiRolledBack != baseline {
				t.Fatalf("tui rollback hash = %s, baseline = %s", tuiRolledBack, baseline)
			}

			// Assert equivalence
			if headlessReceipt.status != tuiReceipt.status {
				t.Errorf("status mismatch: headless=%s tui=%s", headlessReceipt.status, tuiReceipt.status)
			}
			if headlessReceipt.failureObserved != tuiReceipt.failureObserved {
				t.Errorf("failure observed mismatch: headless=%t tui=%t", headlessReceipt.failureObserved, tuiReceipt.failureObserved)
			}
			if headlessReceipt.repairVerified != tuiReceipt.repairVerified {
				t.Errorf("repair verified mismatch: headless=%t tui=%t", headlessReceipt.repairVerified, tuiReceipt.repairVerified)
			}
			if !equalStringSlices(headlessReceipt.changedPaths, tuiReceipt.changedPaths) {
				t.Errorf("changed paths mismatch:\n  headless=%v\n  tui=%v", headlessReceipt.changedPaths, tuiReceipt.changedPaths)
			}
			if headlessRolledBack != tuiRolledBack {
				t.Errorf("rollback hash mismatch: headless=%s tui=%s", headlessRolledBack, tuiRolledBack)
			}

			return

		default:
			cmd = nextAgentCmd(t, msg)
		}
	}
}

// nextAgentCmd unwraps Bubble Tea sequences produced by the approval overlay and
// returns the next command that can yield an agent event or terminal message.
func nextAgentCmd(t *testing.T, msg tea.Msg) tea.Cmd {
	t.Helper()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		t.Fatalf("unexpected message type: %T", msg)
	}
	for i := 0; i < v.Len(); i++ {
		child, ok := v.Index(i).Interface().(tea.Cmd)
		if !ok || child == nil {
			continue
		}
		if childMsg := child(); childMsg != nil {
			if _, ok := childMsg.(agentEventMsg); ok {
				return func() tea.Msg { return childMsg }
			}
			if _, ok := childMsg.(agentDoneMsg); ok {
				return func() tea.Msg { return childMsg }
			}
			if reflect.ValueOf(childMsg).Kind() == reflect.Slice {
				return nextAgentCmd(t, childMsg)
			}
		}
	}
	t.Fatalf("sequence did not contain an agent event")
	return nil
}

func parityScriptedProvider() *parityFakeProvider {
	const (
		originalSum = "return a + b"
		brokenSum   = "return a + b + 1"
		draftNote   = "Status: draft"
		changedNote = "Status: repaired"
	)
	return &parityFakeProvider{turns: [][]client.Event{
		toolTurn("read-module", tools.ToolReadFile, `{"path":"go.mod"}`),
		toolTurn("find-sum", tools.ToolSearchCode, `{"query":"return a + b","path":"."}`),
		{
			{Kind: client.EventToken, Text: "Plan: reproduce the test failure, repair it, then verify and report the diff."},
			{Kind: client.EventToolCalls, ToolCalls: []client.ToolCall{
				{ID: "break-sum", Function: client.FunctionCall{Name: tools.ToolPatchFile, Arguments: `{"path":"calc.go","old_content":"` + originalSum + `","new_content":"` + brokenSum + `"}`}},
				{ID: "change-note", Function: client.FunctionCall{Name: tools.ToolPatchFile, Arguments: `{"path":"notes.txt","old_content":"` + draftNote + `","new_content":"` + changedNote + `"}`}},
			}},
		},
		toolTurn("observe-failure", tools.ToolRunCommand, `{"command":"go test ./..."}`),
		toolTurn("repair-sum", tools.ToolPatchFile, `{"path":"calc.go","old_content":"`+brokenSum+`","new_content":"`+originalSum+`"}`),
		toolTurn("verify-repair", tools.ToolRunCommand, `{"command":"go test ./..."}`),
		{{Kind: client.EventToken, Text: "Repair verified. The two-file diff is ready for review and can be rolled back."}},
	}}
}

type parityFakeProvider struct {
	turns [][]client.Event
	turn  int
}

func (p *parityFakeProvider) Chat(ctx context.Context, req client.ChatRequest) (<-chan client.Event, error) {
	if p.turn >= len(p.turns) {
		ch := make(chan client.Event, 2)
		ch <- client.Event{Kind: client.EventToken, Text: "All done"}
		ch <- client.Event{Kind: client.EventUsage, Usage: &client.Usage{In: 10, Out: 5, CostUSD: 0.001}}
		close(ch)
		return ch, nil
	}

	events := p.turns[p.turn]
	p.turn++

	ch := make(chan client.Event, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	ch <- client.Event{Kind: client.EventUsage, Usage: &client.Usage{In: 100, Out: 50, CostUSD: 0.01}}
	close(ch)
	return ch, nil
}

type parityEvidence struct {
	status          string
	changedPaths    []string
	failureObserved bool
	repairVerified  bool
}

func parityReceipt(events []agentcore.Event) parityEvidence {
	var out parityEvidence
	seenPaths := make(map[string]bool)
	for _, ev := range events {
		switch ev.Kind {
		case agentcore.KindDone:
			out.status = ev.Status
		case agentcore.KindFileWrite:
			if ev.Path != "" && !seenPaths[ev.Path] {
				seenPaths[ev.Path] = true
				out.changedPaths = append(out.changedPaths, ev.Path)
			}
		case agentcore.KindCommand:
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

func parityFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{
		"go.mod":       "module parityfixture\n\ngo 1.26\n",
		"calc.go":      "package parityfixture\n\nfunc Sum(a, b int) int {\n\treturn a + b\n}\n",
		"calc_test.go": "package parityfixture\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif got := Sum(1, 2); got != 3 {\n\t\tt.Fatalf(\"Sum(1, 2) = %d, want 3\", got)\n\t}\n}\n",
		"notes.txt":    "Title: Parity fixture\nOwner: test\nStatus: draft\nLine: 4\nLine: 5\nLine: 6\nLine: 7\nLine: 8\nLine: 9\nLine: 10\nLine: 11\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return repo
}

func parityTreeHash(t *testing.T, root string) string {
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

func decodeParityEvents(t *testing.T, jsonl *bytes.Buffer) []agentcore.Event {
	t.Helper()
	var events []agentcore.Event
	for {
		line, err := jsonl.ReadBytes('\n')
		if len(line) == 0 {
			break
		}
		if err != nil && err.Error() != "EOF" {
			t.Fatalf("read jsonl: %v", err)
		}
		var ev agentcore.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
