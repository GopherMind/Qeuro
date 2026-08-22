package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/styles"
)

// trimmedHistory shrinks the conversation before re-sending it each tool step
// (token economy, plan §14). The policy lives in the client package so team-mode
// workers share it; see client.TrimMessages.
func trimmedHistory(history []client.Message) []client.Message {
	return client.TrimMessages(history)
}

// logSession appends one record to the durable session journal
// (~/.qeuro/sessions/<id>.jsonl, roadmap §8). Best-effort and nil-safe:
// journalling must never break the turn.
//
// The text is what was sent to or received from the model, not what was printed:
// a rendered reply carries ANSI escapes and hard wrapping, and replaying that on
// resume would feed the model its own terminal formatting.
func (m *model) logSession(kind session.Kind, text string) {
	if m.journal == nil {
		return
	}
	if err := m.journal.Append(session.Record{Kind: kind, Text: text}); err != nil && !m.journalWarned {
		// Once per session: a full disk fails every record, and repeating the
		// warning on each turn would bury the conversation.
		m.journalWarned = true
		m.notice = "session journal: " + clientcfg.DisplaySafe(err.Error())
	}
}

// displaySafeHistory copies a conversation with control characters escaped, for
// printing only. It is used where the text came off disk rather than off the
// wire: a live reply is rendered as it streams, but a restored one has been
// sitting in a file that anything with write access to the home directory could
// have edited.
func displaySafeHistory(history []client.Message) []client.Message {
	out := make([]client.Message, len(history))
	for i, msg := range history {
		msg.Content = clientcfg.DisplaySafeBlock(msg.Content)
		out[i] = msg
	}
	return out
}

// journalPartial records text streamed by a turn that did not finish, so a
// cancel or a Ctrl+C does not erase the model's work from the transcript. It is
// a no-op when nothing was streamed.
func (m *model) journalPartial() {
	if strings.TrimSpace(m.streamText) == "" {
		return
	}
	m.logSession(session.KindPartial, m.streamText)
}

// resumeSession restores a previous session's conversation into the current
// dialog: the model regains the earlier context and the user continues where
// they left off. With no argument it takes the newest other session; with an id
// it takes that one, so `/resume <id>` and `qeuro resume <id>` name the same
// thing.
//
// Only user and assistant records replay. A partial reply — the answer a crash
// or a cancel cut off — is shown but not replayed: the model never finished it,
// and restoring it as a completed turn would teach the next turn that a
// truncated answer was accepted.
func (m model) resumeSession(id string) (tea.Model, tea.Cmd) {
	// A turn in flight owns the tail of m.history: the user message is already
	// there and the reply belongs after it. Splicing an older conversation in
	// between would leave the live question answered by a restored answer, and
	// the turn's cache boundary (turnStartIndex) pointing into the wrong message.
	// Slash commands are dispatched before the "wait for the reply" gate — that
	// is deliberate, so /help and /exit work mid-stream — so this one refuses on
	// its own behalf rather than relying on the caller.
	if m.streaming || m.awaitingApproval || m.awaitingTeamInput {
		m.notice = "finish or cancel the current turn before restoring a session (esc)"
		return m, nil
	}

	var (
		s   session.Session
		err error
	)
	if id == "" {
		s, err = session.LoadLatest(m.sessionID)
	} else {
		s, err = session.Load(id)
	}
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			m.notice = "no saved session to restore"
		} else {
			m.notice = "resume failed: " + clientcfg.DisplaySafe(err.Error())
		}
		return m, nil
	}

	turns := s.Turns()
	if len(turns) == 0 {
		m.notice = "session " + s.ID + " has no conversation to restore"
		return m, nil
	}
	for _, r := range turns {
		m.history = append(m.history, client.Message{Role: string(r.Kind), Content: r.Text})
	}
	m.app.MsgCount += len(turns)

	notes := []string{fmt.Sprintf("restored messages: %d · session %s", len(turns), s.Label())}
	if s.Crashed {
		notes = append(notes, "previous run did not exit cleanly")
	}
	if s.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d unreadable record(s) skipped", s.Skipped))
	}
	if s.Truncated {
		notes = append(notes, "older turns beyond the replay limit were left out")
	}
	m.notice = strings.Join(notes, " · ")

	// The resume itself is journalled, so this session's transcript explains
	// where its opening history came from instead of appearing to start
	// mid-conversation. The restored turns are NOT re-journalled: they already
	// have a file, and copying them would double every transcript through a
	// chain of resumes.
	m.logSession(session.KindResume, "resumed from session "+s.ID)

	// A journal is a file, so its text is untrusted for display purposes even
	// though the model wrote it: anything that can put bytes in the file can
	// address the terminal the transcript is printed into. The conversation
	// itself keeps the original text — the model has to see what it said — and
	// only the rendering is escaped. Newlines survive, or a restored answer
	// would come back as one unreadable line.
	out := historyScreen(displaySafeHistory(m.history), m.width)
	if tail := strings.TrimSpace(s.PartialTail()); tail != "" {
		out += "\n" + strings.TrimRight(styles.Message(styles.RoleSystem, clock(time.Now()),
			"unfinished reply from "+s.ID,
			"this reply was cut off and was NOT restored into the conversation:\n\n"+
				clientcfg.DisplaySafeBlock(tail), m.width), "\n")
	}
	return m, tea.Println(out)
}
