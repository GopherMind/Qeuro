package tui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/session"
	"qeuro/internal/state"
)

// Ctrl+C, roadmap §8 row "Прерывание": the first press is a soft cancel that
// keeps the partial answer, the second quits.
//
// The behaviour these pin is the one a user discovers by accident, so each test
// is written against what they would do rather than against the branch: press it
// mid-stream, press it twice, press it once and then keep typing.

// ctrlC is the key event the terminal sends, so the tests go through Update
// rather than calling onInterruptKey — the dispatch order in onKey is part of
// what can break (the palette, an open info panel and the approval prompt all
// consume keys before it).
var ctrlC = tea.KeyMsg{Type: tea.KeyCtrlC}

func pressCtrlC(t *testing.T, m model) (model, tea.Cmd) {
	t.Helper()
	res, cmd := m.Update(ctrlC)
	got, ok := res.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", res)
	}
	return got, cmd
}

// TestFirstCtrlCCancelsTheTurnWithoutQuitting is the row's headline. Before this
// change Ctrl+C mid-stream quit the program, discarding the partial answer the
// row exists to keep.
func TestFirstCtrlCCancelsTheTurnWithoutQuitting(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	ctx := m.beginTurn()
	m.streaming = true
	m.streamText = "an answer that was still being written"

	got, cmd := pressCtrlC(t, m)

	if got.quit {
		t.Fatal("the first ctrl+c must cancel the turn, not end the session")
	}
	if !got.interrupted || got.streaming {
		t.Fatalf("turn not cancelled: interrupted=%v streaming=%v", got.interrupted, got.streaming)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("turn context = %v, want cancelled so the stream goroutine unblocks", ctx.Err())
	}
	if cmd == nil {
		t.Fatal("expected the interrupt notice to be flushed to scrollback")
	}
	// The notice has to name the second press. A cancel that says nothing about
	// what Ctrl+C now does leaves the user pressing it again to find out — which,
	// with the exit armed, ends the session.
	if !strings.Contains(got.notice, "quit") {
		t.Errorf("notice = %q, want it to say a second ctrl+c quits", got.notice)
	}
}

// TestSecondCtrlCQuits: the escape hatch must still exist. A cancel-only Ctrl+C
// would leave a user with a wedged turn and no way out but SIGKILL.
func TestSecondCtrlCQuits(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.beginTurn()
	m.streaming = true
	m.streamText = "half an answer"

	first, _ := pressCtrlC(t, m)
	if first.quit {
		t.Fatal("the first press quit")
	}
	second, cmd := pressCtrlC(t, first)
	if !second.quit {
		t.Fatal("the second ctrl+c must quit")
	}
	if cmd == nil {
		t.Fatal("expected tea.Quit")
	}
}

// TestCtrlCWithNothingRunningArmsAndSaysSo: idle, there is nothing to cancel, so
// the press must still do something visible. A Ctrl+C that produces no response
// reads as a hung program, and the next thing a user reaches for is the one that
// loses the session.
func TestCtrlCWithNothingRunningArmsAndSaysSo(t *testing.T) {
	m := model{app: state.New(), width: 80}

	first, cmd := pressCtrlC(t, m)
	if first.quit {
		t.Fatal("a single ctrl+c must not quit")
	}
	if !first.quitArmed {
		t.Fatal("the press must arm the exit")
	}
	if first.notice == "" {
		t.Fatal("a ctrl+c that changes nothing on screen looks like a hang")
	}
	if cmd != nil {
		t.Errorf("nothing was cancelled, so there is nothing to flush: cmd = %T", cmd)
	}

	second, cmd := pressCtrlC(t, first)
	if !second.quit || cmd == nil {
		t.Fatalf("the second idle ctrl+c must quit: quit=%v cmd=%v", second.quit, cmd)
	}
}

// TestAnyOtherKeyDisarmsTheExit: the second press has to be a second press, not
// an eventual one. Without this a Ctrl+C from ten minutes and three turns ago
// still has an exit loaded.
func TestAnyOtherKeyDisarmsTheExit(t *testing.T) {
	m := model{app: state.New(), width: 80}
	armed, _ := pressCtrlC(t, m)
	if !armed.quitArmed {
		t.Fatal("setup: the press did not arm")
	}

	res, _ := armed.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	typed := res.(model)
	if typed.quitArmed {
		t.Fatal("typing must disarm the pending exit")
	}

	next, _ := pressCtrlC(t, typed)
	if next.quit {
		t.Fatal("after typing, the next ctrl+c is a first press again")
	}
}

// TestCancelledReplyStaysInTheConversationMarked is the "с сохранением частичного
// ответа" half. The tokens were generated and billed, so the text has to survive
// into the next request — otherwise the user paid for an answer that "continue"
// cannot refer to.
func TestCancelledReplyStaysInTheConversationMarked(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.history = []client.Message{{Role: "user", Content: "explain database indexes"}}
	m.beginTurn()
	m.streaming = true
	m.streamText = "An index keeps an ordered copy of the key so"

	got, _ := pressCtrlC(t, m)

	if len(got.history) != 2 {
		t.Fatalf("history has %d messages, want the cancelled reply appended", len(got.history))
	}
	last := got.history[1]
	if last.Role != "assistant" {
		t.Errorf("role = %q, want assistant", last.Role)
	}
	if !strings.HasPrefix(last.Content, "An index keeps an ordered copy of the key so") {
		t.Errorf("content = %q, want the streamed text kept verbatim", last.Content)
	}
	// The label is not decoration. An unmarked fragment reads to the model as a
	// complete answer it gave, which teaches it that stopping mid-sentence is an
	// acceptable way to finish.
	if !strings.Contains(last.Content, "cancelled") {
		t.Errorf("content = %q, want it marked as cancelled", last.Content)
	}
	if got.app.MsgCount != 1 {
		t.Errorf("MsgCount = %d, want the appended message counted", got.app.MsgCount)
	}

	// And it is in the journal too, so a resume after the cancel shows it. On-screen
	// scrollback is not a record.
	s, err := session.Load(got.sessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tail := s.PartialTail(); !strings.Contains(tail, "ordered copy") {
		t.Errorf("journal partial = %q, want the cancelled text", tail)
	}
}

// TestNothingIsAppendedWhenNothingWasStreamed: a Ctrl+C between the request and
// the first token has no text to keep. Appending an empty assistant message
// there would send the provider a turn it must answer twice.
func TestNothingIsAppendedWhenNothingWasStreamed(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.history = []client.Message{{Role: "user", Content: "explain database indexes"}}
	m.beginTurn()
	m.streaming = true
	m.streamText = "  \n\t "

	got, _ := pressCtrlC(t, m)

	if len(got.history) != 1 {
		t.Fatalf("history = %+v, want only the user message", got.history)
	}
	if got.app.MsgCount != 0 {
		t.Errorf("MsgCount = %d, want 0", got.app.MsgCount)
	}
}

// TestCancelledPartialIsAppendedOnce guards the double-append: the partial is
// kept by interruptTurn, and a later Ctrl+C — or /exit — must not add it again.
// streamText is cleared by the cancel, which is what makes that true; the test
// exists so a change that stops clearing it fails here rather than in a
// conversation that repeats itself.
func TestCancelledPartialIsAppendedOnce(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.beginTurn()
	m.streaming = true
	m.streamText = "one fragment"

	first, _ := pressCtrlC(t, m)
	second, _ := pressCtrlC(t, first)

	fragments := 0
	for _, msg := range second.history {
		if strings.Contains(msg.Content, "one fragment") {
			fragments++
		}
	}
	if fragments != 1 {
		t.Fatalf("the cancelled fragment appears %d times in the conversation, want 1", fragments)
	}
}

// TestCtrlCCancelsAnApprovalPrompt: an approval prompt is work in flight too —
// the turn is paused waiting for a decision, with a tool call outstanding. Ctrl+C
// there must mean "stop this", not "quit", for the same reason as mid-stream.
func TestCtrlCCancelsAnApprovalPrompt(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.beginTurn()
	m.awaitingApproval = true
	m.streamText = "I will patch main.go"

	got, _ := pressCtrlC(t, m)
	if got.quit {
		t.Fatal("ctrl+c at an approval prompt must cancel the turn, not quit")
	}
	if !got.interrupted {
		t.Fatal("the turn must be marked interrupted")
	}
}

// TestCancellingAtAnApprovalPromptClosesThePrompt is the defect this row
// introduced and the review caught. Esc could never reach interruptTurn with an
// approval open — its guard excludes awaitingApproval — so nothing there closed
// the dialog. Ctrl+C can, and a prompt left up after the cancel still captures
// keys: pressing "y" would run the tool for a turn whose context is already
// cancelled, executing a file edit or a shell command whose result has nowhere
// to go.
func TestCancellingAtAnApprovalPromptClosesThePrompt(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.beginTurn()
	m.awaitingApproval = true
	m.approvalChoice = 2
	m.pendingPreview = "--- a/main.go\n+++ b/main.go"
	// The command carries an escape sequence, because the summary printed in the
	// panel's place is built from these arguments and the model chose them.
	m.pendingTool = &client.ToolCall{
		ID: "call_1",
		Function: client.FunctionCall{
			Name: "run_command",
			// ESC[2J clears the screen and BEL rings the bell, written as JSON escapes
			// so no raw control byte lives in this source file. The summary printed in
			// the panel's place is built from these arguments, which the model chose.
			Arguments: `{"command":"rm -rf build\u001b[2J\u0007"}`,
		},
	}

	got, cmd := pressCtrlC(t, m)

	if got.awaitingApproval {
		t.Error("the approval prompt must not survive the cancel: it still captures keys")
	}
	if got.pendingTool != nil {
		t.Error("the pending tool must be dropped, or a later approval runs it for a dead turn")
	}
	if got.pendingPreview != "" || got.approvalChoice != 0 {
		t.Errorf("approval state left behind: preview=%q choice=%d", got.pendingPreview, got.approvalChoice)
	}
	if cmd == nil {
		t.Fatal("expected the panel to be erased from the frame and replaced by a status line")
	}
	// The panel is multi-line and drawn in the live frame, so the cancel has to wipe
	// it the same way the y/n path does — otherwise it stays in the scrollback
	// looking like it is still waiting for an answer.
	printed := printedText(t, cmd)
	if !strings.Contains(printed, ansiEraseBelow) {
		t.Error("the approval frame was not erased")
	}
	// The summary is built from the tool arguments, which the model chose. The only
	// escape sequence in the output must be the deliberate one above plus the
	// styling — a command that carried its own would be addressing the terminal
	// from inside the line that says what was cancelled.
	if strings.Contains(printed, "\x1b[2J") || strings.Contains(printed, "\x07") {
		t.Errorf("model-chosen text reached the terminal unescaped: %q", printed)
	}
	// And the erase must be ordered before the interrupt notice, not merely emitted
	// alongside it. tea.Batch is documented to run its commands "with no ordering
	// guarantees", and the erase clears from the cursor to the end of the screen —
	// so a notice that happens to print first is erased by the very command meant
	// to tidy up after it. tea.sequenceMsg is unexported, so the type name is the
	// only handle on the distinction.
	if got := fmt.Sprintf("%T", cmd()); got != "tea.sequenceMsg" {
		t.Errorf("interrupt command = %s, want a sequence: the erase must precede the notice", got)
	}
}

// TestAnOrdinaryInterruptStaysASinglePrint: the sequencing above is for the
// approval case only. Esc on a plain stream has one thing to print, and wrapping
// it in a sequence would be machinery with nothing to order.
func TestAnOrdinaryInterruptStaysASinglePrint(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.beginTurn()
	m.streaming = true
	m.streamText = "half an answer"

	_, cmd := pressCtrlC(t, m)
	if cmd == nil {
		t.Fatal("expected the interrupt notice")
	}
	if got := fmt.Sprintf("%T", cmd()); got == "tea.sequenceMsg" {
		t.Errorf("interrupt command = %s, want a bare print when there is no panel to erase", got)
	}
}

// printedText runs a command far enough to read the text it prints, descending
// into a sequence. renderCmd (resume_test.go) formats the message itself, which
// is enough for one print but shows only function pointers for a sequence — and
// the erase here is the first element of one.
func printedText(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	var out strings.Builder
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		// tea.sequenceMsg and tea.BatchMsg are both named []tea.Cmd, and both
		// unexported or awkward to assert against by type, so the slice is reached
		// by kind. Reflection rather than a type switch for exactly that reason.
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice {
			for i := 0; i < v.Len(); i++ {
				if child, ok := v.Index(i).Interface().(tea.Cmd); ok {
					walk(child)
				}
			}
			return
		}
		out.WriteString(fmt.Sprintf("%v", msg))
	}
	walk(cmd)
	return out.String()
}

// TestExitCommandCancelsTheTurnItLeaves: /exit and the second Ctrl+C are the same
// exit, so they must release the same resources. /exit used to leave the in-flight
// request's goroutine to discover the process was gone by itself (H2).
func TestExitCommandCancelsTheTurnItLeaves(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	ctx := m.beginTurn()
	m.streaming = true
	m.streamText = "half an answer"

	res, cmd := m.runCommand("exit")
	got := res.(model)
	if !got.quit || cmd == nil {
		t.Fatalf("/exit must quit: quit=%v cmd=%v", got.quit, cmd)
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("turn context = %v, want cancelled on exit", ctx.Err())
	}
	// Quitting mid-stream loses the terminal but must not lose the answer.
	s, err := session.Load(got.sessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tail := s.PartialTail(); tail != "half an answer" {
		t.Errorf("journal partial = %q, want the in-flight text recorded", tail)
	}
}
