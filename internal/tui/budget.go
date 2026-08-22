package tui

// A hard spend ceiling for one session (roadmap §8, row "Стоимость":
// «жёсткий --budget с остановом»).
//
// Why this is not a check at submit time. One turn is not one billed call: the
// tool loop runs up to maxToolSteps provider calls, each billed, and each able
// to trigger the next without the user typing anything. A gate that only ran
// when the user pressed enter would see 0 spent, admit the turn, and then let it
// bill twenty times past the ceiling — the ceiling would hold only for users who
// never used tools. So the check runs in two places: before a turn starts, and
// between tool steps, which is the only other point where this program decides
// to spend money.
//
// "Hard" is the operative word: the loop stops, the model is told the turn ended,
// and nothing resumes it. A warning would make the flag advisory, and an advisory
// budget is what the status-bar counter already is.

import "fmt"

// budget is the ceiling and what has been spent under it. Zero limit means no
// ceiling, which is the default: a budget nobody asked for that stops work
// mid-task would be worse than no budget at all.
type budget struct {
	limit float64 // credits; 0 = unlimited
	spent float64 // credits accumulated this session
	// stopped latches once the ceiling is crossed. Without it, a turn that ends
	// exactly at the limit would be reported as stopped on every subsequent
	// keystroke, and the notice would print again for a turn that never started.
	stopped bool
}

// active reports whether a ceiling is in force.
func (b budget) active() bool { return b.limit > 0 }

// record adds the credits a completed call cost. It is the only writer of spent,
// so the accumulated figure cannot disagree with the receipts the session showed.
func (b *budget) record(credits float64) {
	// Negative credits would be a settle, not a charge. The session counter is a
	// spend total, and letting a refund lower it would let a turn that reserved
	// and released credits buy room under the ceiling.
	if credits > 0 {
		b.spent += credits
	}
}

// exhausted reports whether the ceiling has been reached. It is deliberately >=:
// a budget of 10 credits that permits a call at exactly 10 spent is a budget of
// more than 10.
func (b budget) exhausted() bool { return b.active() && b.spent >= b.limit }

// remaining is what is left under the ceiling, floored at zero so a display
// never shows a negative allowance.
func (b budget) remaining() float64 {
	if !b.active() {
		return 0
	}
	if r := b.limit - b.spent; r > 0 {
		return r
	}
	return 0
}

// notice is the one-line explanation shown when the ceiling stops work. It names
// the limit and the flag, because a session that stops without saying why reads
// as a broken client rather than an enforced ceiling.
func (b budget) notice() string {
	return fmt.Sprintf("budget reached: %.1f of %.1f credits spent this session · raise it with --budget or restart", b.spent, b.limit)
}

// budgetStopMessage is what the conversation records when the loop is cut short.
// The model is told, in its own history, that the turn ended for a reason
// unrelated to the task: an unexplained stop mid-tool-loop teaches it that
// abandoning work halfway is a normal ending.
const budgetStopMessage = "SESSION BUDGET REACHED: the user's credit ceiling for this session was hit, so this turn was stopped before it finished. No further tool calls will run."
