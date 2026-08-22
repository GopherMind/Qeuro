package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"

	"qeuro/internal/client"
	"qeuro/internal/state"
)

// The ceiling exists to stop a tool loop, which is the only thing in this program
// that can bill twenty times without the user acting. These tests pin that,
// plus the arithmetic that decides when it fires.

// newBudgetTestModel builds a model with a ceiling and a usable input widget.
// The input matters: onSubmit reads and rewrites it, and a zero-value textarea
// would make the "the user's text survives" assertion vacuous.
func newBudgetTestModel(limit float64) model {
	in := textarea.New()
	in.SetWidth(72)
	return model{
		app:            state.New(),
		width:          80,
		input:          in,
		budget:         budget{limit: limit},
		turnStartIndex: -1,
	}
}

func TestBudgetZeroLimitIsUnlimited(t *testing.T) {
	b := budget{}
	b.record(1_000_000)
	if b.active() {
		t.Fatal("a zero limit reported an active ceiling")
	}
	if b.exhausted() {
		t.Fatal("a session with no ceiling was stopped")
	}
}

// The boundary is inclusive: a 10-credit ceiling that permits work at exactly 10
// spent is not a 10-credit ceiling.
func TestBudgetIsExhaustedAtExactlyTheLimit(t *testing.T) {
	b := budget{limit: 10}
	b.record(9.999)
	if b.exhausted() {
		t.Fatalf("stopped early at %v of %v", b.spent, b.limit)
	}
	b.record(0.001)
	if !b.exhausted() {
		t.Fatalf("not stopped at %v of %v", b.spent, b.limit)
	}
}

// A refund must not buy room under the ceiling. Settles are negative on this
// backend (credits are reserved before generation), so a signed accumulator
// would let a turn that reserved and released credits spend twice.
func TestBudgetIgnoresNegativeCredits(t *testing.T) {
	b := budget{limit: 10}
	b.record(6)
	b.record(-4)
	if b.spent != 6 {
		t.Fatalf("spent = %v after a refund, want 6", b.spent)
	}
	b.record(4)
	if !b.exhausted() {
		t.Fatalf("a refund made room under the ceiling: spent %v of %v", b.spent, b.limit)
	}
}

func TestBudgetRemainingIsFlooredAtZero(t *testing.T) {
	b := budget{limit: 5}
	b.record(9)
	if got := b.remaining(); got != 0 {
		t.Fatalf("remaining = %v, want 0 (never negative)", got)
	}
	if got := (budget{}).remaining(); got != 0 {
		t.Fatalf("remaining with no ceiling = %v, want 0", got)
	}
}

// The notice must name both numbers and the way out. A session that stops
// without saying why is indistinguishable from a broken client.
func TestBudgetNoticeExplainsItself(t *testing.T) {
	b := budget{limit: 20}
	b.record(20)
	notice := b.notice()
	for _, want := range []string{"budget", "20", "--budget"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q does not mention %q", notice, want)
		}
	}
}

// The receipt is where spend becomes known, so this pins the wiring: a usage
// event must move the session counter, or the gate below never fires.
func TestUsageEventAccumulatesTheBudget(t *testing.T) {
	m := model{budget: budget{limit: 10}}
	m.app = state.New()
	m.streamCh = make(chan client.Event, 1)

	next, _ := m.onStreamEvent(streamEventMsg{ok: true, ev: client.Event{
		Kind:  client.EventUsage,
		Usage: &client.Usage{In: 100, Out: 50, Credits: 4, Balance: 96},
	}})
	got := next.(model)
	if got.budget.spent != 4 {
		t.Fatalf("budget.spent = %v after a 4-credit call, want 4", got.budget.spent)
	}
	if got.budget.exhausted() {
		t.Fatal("4 of 10 credits exhausted the ceiling")
	}
}

// The property the row is about. A turn that has already spent its ceiling must
// not start another tool step, even though the model asked for one and the step
// limit has plenty of room left.
func TestToolLoopStopsWhenTheBudgetIsSpent(t *testing.T) {
	m := newBudgetTestModel(10)
	m.budget.record(10)
	m.streaming = true
	m.pendingCalls = []client.ToolCall{{
		ID:       "call-1",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`},
	}}
	m.streamText = "let me look at that file"

	next, cmd := m.runToolCalls()
	got := next.(model)

	if got.toolQueue != nil {
		t.Fatalf("tool queue survived the budget stop: %+v", got.toolQueue)
	}
	if got.pendingCalls != nil {
		t.Fatalf("pending calls survived the budget stop: %+v", got.pendingCalls)
	}
	if got.streaming {
		t.Fatal("still streaming after the budget stopped the turn")
	}
	if !got.budget.stopped {
		t.Fatal("budget.stopped was not latched")
	}
	if cmd == nil {
		t.Fatal("no command returned, so nothing told the user why work stopped")
	}
	// The stop is explained to the model in its own history, so a later turn does
	// not read an unfinished task as one it chose to abandon.
	var told bool
	for _, msg := range got.history {
		if msg.Role == "user" && strings.Contains(msg.Content, "SESSION BUDGET REACHED") {
			told = true
		}
	}
	if !told {
		t.Fatalf("the model was not told why the turn ended: %+v", got.history)
	}
}

// The stop must not ask the model for a closing summary. finalizeToolLimit does,
// deliberately; doing it here would bill another call to announce that billing
// has stopped.
func TestBudgetStopDoesNotStartAnotherCall(t *testing.T) {
	m := newBudgetTestModel(10)
	m.budget.record(10)
	m.streaming = true
	m.pendingCalls = []client.ToolCall{{
		ID:       "call-1",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
	}}

	next, _ := m.runToolCalls()
	got := next.(model)
	if got.toolFinal {
		t.Fatal("toolFinal was set, which asks the model for one more billed reply")
	}
	if got.app.Phase != state.PhaseIdle {
		t.Fatalf("phase = %v after the budget stop, want idle", got.app.Phase)
	}
	if got.streamCh != nil {
		t.Fatal("a stream channel survived the stop")
	}
}

// The partial reply is kept for the same reason a cancelled turn keeps it: the
// tokens were produced and billed, so discarding them charges for text the user
// cannot refer to.
func TestBudgetStopKeepsThePartialReply(t *testing.T) {
	m := newBudgetTestModel(5)
	m.budget.record(5)
	m.streaming = true
	m.streamText = "I read the config and found"
	m.pendingCalls = []client.ToolCall{{
		ID:       "c",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
	}}

	next, _ := m.runToolCalls()
	got := next.(model)

	var found bool
	for _, msg := range got.history {
		if msg.Role == "assistant" && strings.Contains(msg.Content, "I read the config and found") {
			found = true
			if !strings.Contains(msg.Content, partialSuffix) {
				t.Fatalf("partial reply kept without its marker: %q", msg.Content)
			}
		}
	}
	if !found {
		t.Fatalf("the streamed text was discarded: %+v", got.history)
	}
}

// Cancelling the context does not unqueue the events already sitting in the
// Bubble Tea message queue. If the stop did not also latch `interrupted`, those
// events would append tokens to a turn the ceiling just ended and re-arm the
// reader — a reply printed under a budget that refused to pay for it.
func TestBudgetStopIgnoresLateStreamEvents(t *testing.T) {
	m := newBudgetTestModel(5)
	m.budget.record(5)
	m.streaming = true
	m.pendingCalls = []client.ToolCall{{
		ID:       "c",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
	}}

	next, _ := m.runToolCalls()
	stopped := next.(model)

	if !stopped.interrupted {
		t.Fatal("the stop did not latch interrupted, so a late event would be applied")
	}
	// The guard in onStreamEvent is `m.interrupted || m.streamCh == nil`, and the
	// stop sets both. Restoring the channel here isolates the flag: without it this
	// test would pass on the nil channel alone and prove nothing about the latch.
	stopped.streamCh = make(chan client.Event, 1)

	// A token event that was already in flight when the ceiling fired.
	after, cmd := stopped.onStreamEvent(streamEventMsg{ok: true, ev: client.Event{
		Kind: client.EventToken, Text: "...and here is more text you did not pay for",
	}})
	got := after.(model)
	if got.streamText != "" {
		t.Fatalf("a late token was appended after the budget stop: %q", got.streamText)
	}
	if cmd != nil {
		t.Fatal("the stream reader was re-armed after the budget stop")
	}
	// A usage event for the call that was already paid for must not be treated as
	// the start of a new turn either.
	after2, cmd2 := stopped.onStreamEvent(streamEventMsg{ok: true, ev: client.Event{
		Kind:  client.EventUsage,
		Usage: &client.Usage{In: 10, Out: 10, Credits: 1, Balance: 1},
	}})
	if cmd2 != nil {
		t.Fatal("a late usage event re-armed the reader")
	}
	if after2.(model).streaming {
		t.Fatal("a late event restarted streaming after the stop")
	}
}

// Fenced third-party payloads from the abandoned step must not ride into the
// next turn — the same property finalizeToolLimit and interruptTurn hold.
func TestBudgetStopDropsUntrustedBlocks(t *testing.T) {
	m := newBudgetTestModel(5)
	m.budget.record(5)
	m.streaming = true
	m.untrustedBlocks = []string{"<<<data from some server>>>"}
	m.pendingCalls = []client.ToolCall{{
		ID:       "c",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`},
	}}

	next, _ := m.runToolCalls()
	if got := next.(model); got.untrustedBlocks != nil {
		t.Fatalf("untrusted blocks survived the stop: %+v", got.untrustedBlocks)
	}
}

// Below the ceiling the loop proceeds normally, so the gate cannot be passing
// its tests by refusing everything.
func TestToolLoopProceedsUnderTheBudget(t *testing.T) {
	m := newBudgetTestModel(100)
	m.budget.record(3)
	m.streaming = true
	m.pendingCalls = []client.ToolCall{{
		ID:       "call-1",
		Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`},
	}}

	next, _ := m.runToolCalls()
	got := next.(model)
	if got.budget.stopped {
		t.Fatal("the loop stopped with 3 of 100 credits spent")
	}
	var told bool
	for _, msg := range got.history {
		if strings.Contains(msg.Content, "SESSION BUDGET REACHED") {
			told = true
		}
	}
	if told {
		t.Fatal("a budget stop was announced while under the ceiling")
	}
}

// A new turn is refused too, and the user's text stays in the box: a limit they
// can raise must not also eat what they typed.
func TestSubmitIsRefusedWhenTheBudgetIsSpent(t *testing.T) {
	m := newBudgetTestModel(5)
	m.budget.record(5)
	m.loggedIn = true
	m.input.SetValue("what did you find?")

	next, cmd := m.onSubmit()
	got := next.(model)
	if got.streaming {
		t.Fatal("a new turn started after the ceiling was reached")
	}
	if cmd != nil {
		t.Fatal("a command was returned, so a request may have been sent")
	}
	if got.input.Value() != "what did you find?" {
		t.Fatalf("input = %q, want the user's text preserved", got.input.Value())
	}
	if !strings.Contains(got.notice, "budget") {
		t.Fatalf("notice = %q, want an explanation mentioning the budget", got.notice)
	}
}

// With no ceiling configured, nothing about submit changes. This is the
// regression guard for the default path: the flag is opt-in.
func TestSubmitIsUnaffectedWithoutABudget(t *testing.T) {
	m := newBudgetTestModel(0)
	m.loggedIn = true
	m.input.SetValue("hello")

	next, cmd := m.onSubmit()
	got := next.(model)
	if !got.streaming {
		t.Fatal("a turn did not start with no ceiling configured")
	}
	if cmd == nil {
		t.Fatal("no command returned, so no request was sent")
	}
}

// The status bar shows the ceiling rather than the balance once one is set, so
// the stop is visible before it happens.
func TestStatusBarShowsTheBudgetWhenSet(t *testing.T) {
	m := newBudgetTestModel(20)
	m.loggedIn = true
	m.credits, m.creditsKnown = 500, true
	m.budget.record(5)

	text := m.creditsText()
	if !strings.Contains(text, "budget") {
		t.Fatalf("status chip = %q, want it to mention the budget", text)
	}
	if strings.Contains(text, "500") {
		t.Fatalf("status chip = %q, showed the account balance instead of the ceiling", text)
	}

	m.budget = budget{}
	if text := m.creditsText(); !strings.Contains(text, "credits") {
		t.Fatalf("without a ceiling the chip = %q, want the balance", text)
	}
}
