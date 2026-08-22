package tui

import (
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/state"
	"qeuro/internal/tools"
)

func runCall(command string) client.ToolCall {
	return client.ToolCall{ID: "run-1", Type: "function",
		Function: client.FunctionCall{Name: tools.ToolRunCommand, Arguments: `{"command":"` + command + `"}`}}
}

func TestVerificationRequiredAfterMutation(t *testing.T) {
	m := model{app: state.New(), width: 80}
	m.recordTool(patchCall("main.go", "old", "new"), "ok: file main.go modified", true, true)

	if !m.verificationRequired {
		t.Fatal("mutation should require verification")
	}
	if m.verificationPassed {
		t.Fatal("verification must not be marked passed right after mutation")
	}
	if !strings.Contains(m.verificationNote, "main.go") {
		t.Fatalf("verification note should name the changed file, got %q", m.verificationNote)
	}
}

func TestSuccessfulVerificationClearsGate(t *testing.T) {
	m := model{app: state.New(), width: 80}
	m.recordTool(patchCall("main.go", "old", "new"), "ok: file main.go modified", true, true)
	m.recordTool(runCall("go test ./..."), "ok (exit code 0)\n--- output ---\nok", false, true)

	if m.verificationRequired {
		t.Fatal("successful test command should clear verification gate")
	}
	if !m.verificationPassed {
		t.Fatal("successful test command should mark verification passed")
	}
}

func TestFailedVerificationKeepsGate(t *testing.T) {
	m := model{app: state.New(), width: 80}
	m.recordTool(patchCall("main.go", "old", "new"), "ok: file main.go modified", true, true)
	m.recordTool(runCall("go test ./..."), "failed with error: exit status 1\n--- output ---\nFAIL", false, true)

	if !m.verificationRequired {
		t.Fatal("failed test command should keep verification gate active")
	}
	if m.verificationPassed {
		t.Fatal("failed test command must not mark verification passed")
	}
	if !strings.Contains(m.verificationNote, "verification failed") {
		t.Fatalf("expected failed verification note, got %q", m.verificationNote)
	}
}

func TestFinishStreamEnforcesVerificationBeforeFinalAnswer(t *testing.T) {
	m := model{
		app:                  state.New(),
		width:                80,
		streamText:           "готово",
		streaming:            true,
		verificationRequired: true,
		verificationNote:     "latest code change: main.go",
	}

	res, cmd := m.finishStream()
	got := res.(model)
	if !got.streaming {
		t.Fatal("gate should keep the turn streaming")
	}
	if got.app.Phase != state.PhaseGenerating {
		t.Fatalf("phase = %v, want generating", got.app.Phase)
	}
	if cmd == nil {
		t.Fatal("gate should continue the model turn")
	}
	var foundGate bool
	for _, msg := range got.history {
		if msg.Role == "user" && strings.Contains(msg.Content, "QUALITY GATE") {
			foundGate = true
		}
	}
	if !foundGate {
		t.Fatal("expected QUALITY GATE message in history")
	}
}

func TestStreamUsageRecordsLastWindowAndSessionTotals(t *testing.T) {
	m := model{app: state.New(), width: 80, streaming: true, streamCh: make(chan client.Event)}

	res, _ := m.onStreamEvent(streamEventMsg{ok: true, ev: client.Event{
		Kind: client.EventUsage,
		Usage: &client.Usage{
			In:                12000,
			Out:               900,
			CachedInputTokens: 3000,
			CostUSD:           0.0123,
			Credits:           0.615,
			SavedUSD:          0.04,
			Balance:           42.5,
		},
	}})
	got := res.(model)

	if got.app.CtxUsed != 12000 {
		t.Fatalf("CtxUsed = %d, want last input window 12000", got.app.CtxUsed)
	}
	if got.app.Usage.Requests != 1 {
		t.Fatalf("requests = %d, want 1", got.app.Usage.Requests)
	}
	if got.app.Usage.Total.CachedInputTokens != 3000 {
		t.Fatalf("cached total = %d, want 3000", got.app.Usage.Total.CachedInputTokens)
	}
	if !got.creditsKnown || got.credits != 42.5 {
		t.Fatalf("credits not updated: known=%v balance=%v", got.creditsKnown, got.credits)
	}
}
