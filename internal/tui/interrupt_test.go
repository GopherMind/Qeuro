package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/state"
)

// TestEscInterruptsStreamingTurn drives a keypress through Update end to end:
// Esc during an in-flight stream must cancel the per-turn context (so the
// HTTP/SSE goroutine unblocks), clear the partial reply and return the model
// to idle without quitting.
func TestEscInterruptsStreamingTurn(t *testing.T) {
	m := model{app: state.New(), width: 80}
	ctx := m.beginTurn()
	m.streaming = true
	m.streamText = "partial answer"
	m.streamMeta = "sonnet · medium"

	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got, ok := res.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", res)
	}

	if !got.interrupted {
		t.Fatal("esc must mark the turn interrupted")
	}
	if got.streaming {
		t.Fatal("esc must stop streaming")
	}
	if got.streamText != "" || got.streamMeta != "" {
		t.Fatalf("partial reply must be cleared, got %q / %q", got.streamText, got.streamMeta)
	}
	if got.turnCancel != nil || got.streamCh != nil {
		t.Fatal("turn resources must be released")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("turn context must be canceled, got %v", ctx.Err())
	}
	if got.app.Phase != state.PhaseIdle {
		t.Fatalf("phase = %v, want idle", got.app.Phase)
	}
	if got.quit {
		t.Fatal("esc must interrupt the turn, not quit the app")
	}
	if cmd == nil {
		t.Fatal("expected a command flushing the interrupt notice to scrollback")
	}
}

// TestStaleStreamEventsAfterInterruptAreDropped: tokens that were already in
// flight when the user pressed Esc must not resurrect the turn or re-arm the
// stream wait.
func TestStaleStreamEventsAfterInterruptAreDropped(t *testing.T) {
	m := model{app: state.New(), width: 80, interrupted: true}

	res, cmd := m.onStreamEvent(streamEventMsg{
		ok: true,
		ev: client.Event{Kind: client.EventToken, Text: "late token"},
	})
	got := res.(model)

	if got.streamText != "" {
		t.Fatalf("stale token must be dropped, got %q", got.streamText)
	}
	if cmd != nil {
		t.Fatal("stale event must not re-arm the stream wait")
	}
}

// TestBeginTurnCancelsPreviousTurn: starting a new turn must always release
// the previous turn's context so goroutines are never leaked.
func TestBeginTurnCancelsPreviousTurn(t *testing.T) {
	m := model{app: state.New(), width: 80}
	first := m.beginTurn()
	second := m.beginTurn()
	if first.Err() != context.Canceled {
		t.Fatalf("previous turn context must be canceled, got %v", first.Err())
	}
	if second.Err() != nil {
		t.Fatalf("new turn context must be live, got %v", second.Err())
	}
	m.endTurn()
	if second.Err() != context.Canceled {
		t.Fatalf("endTurn must cancel the turn context, got %v", second.Err())
	}
}
