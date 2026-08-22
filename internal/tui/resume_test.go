package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/state"
)

// isolateSessions points the journal directory at a temp dir so the tests never
// touch the developer's real ~/.qeuro/sessions.
func isolateSessions(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if session.Dir() == "" {
		t.Skip("no config dir available on this platform")
	}
}

// seed writes a journal the tests can resume from.
func seed(t *testing.T, id string, clean bool, recs ...session.Record) {
	t.Helper()
	j, err := session.New(id, time.Now(), session.Record{Version: "test"})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	for _, r := range recs {
		if err := j.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if clean {
		if err := j.Close(time.Now()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return
	}
	// A crashed session keeps no end marker. The descriptor still has to be
	// released so the temp dir can be removed on Windows, and Close would write
	// the marker — so the file is closed at cleanup, after the assertions have
	// already read it as crashed.
	t.Cleanup(func() { _ = j.Close(time.Now()) })
}

func newJournalModel(t *testing.T) model {
	t.Helper()
	id := session.NewID(time.Now())
	j, err := session.New(id, time.Now(), session.Record{Version: "test"})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	t.Cleanup(func() { _ = j.Close(time.Now()) })
	return model{app: state.New(), width: 80, sessionID: id, journal: j}
}

func TestResumeRestoresConversationTurnsOnly(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-100000-aaaaaa", true,
		session.Record{Kind: session.KindUser, Text: "what does this repo do"},
		session.Record{Kind: session.KindAssistant, Text: "it is a CLI"},
		session.Record{Kind: session.KindError, Text: "provider timeout"},
		session.Record{Kind: session.KindUser, Text: "and the backend"},
		session.Record{Kind: session.KindAssistant, Text: "a proxy"},
	)

	m := newJournalModel(t)
	out, cmd := m.resumeSession("")
	rm := out.(model)
	if cmd == nil {
		t.Fatal("resume should return a command printing the restored transcript")
	}
	if len(rm.history) != 4 {
		t.Fatalf("history = %d messages, want 4 (the error record is not a turn)", len(rm.history))
	}
	for _, msg := range rm.history {
		if msg.Role != "user" && msg.Role != "assistant" {
			t.Fatalf("restored a non-conversation role %q", msg.Role)
		}
	}
	if rm.history[0].Content != "what does this repo do" || rm.history[3].Content != "a proxy" {
		t.Fatalf("turns restored out of order: %+v", rm.history)
	}
	if rm.app.MsgCount != 4 {
		t.Fatalf("MsgCount = %d, want 4: the context indicator must count restored turns", rm.app.MsgCount)
	}
	if !strings.Contains(rm.notice, "restored messages: 4") {
		t.Fatalf("notice = %q, want the restored count", rm.notice)
	}
}

func TestResumeReportsCrashAndOffersThePartialReply(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-100100-aaaaaa", false,
		session.Record{Kind: session.KindUser, Text: "long question"},
		session.Record{Kind: session.KindPartial, Text: "I was halfway through expl"},
	)

	m := newJournalModel(t)
	out, cmd := m.resumeSession("")
	rm := out.(model)
	if len(rm.history) != 1 {
		t.Fatalf("history = %d, want only the user turn: an unfinished reply is not a turn", len(rm.history))
	}
	if !strings.Contains(rm.notice, "did not exit cleanly") {
		t.Fatalf("notice = %q, want the crash reported", rm.notice)
	}
	if cmd == nil {
		t.Fatal("expected a print command")
	}
	printed := renderCmd(t, cmd)
	if !strings.Contains(printed, "halfway through expl") {
		t.Fatalf("the lost reply must be shown to the user, got:\n%s", printed)
	}
	if !strings.Contains(printed, "NOT restored") {
		t.Fatalf("the output must say the partial was not restored, got:\n%s", printed)
	}
}

func TestResumeByIDPicksThatSession(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-100200-aaaaaa", true, session.Record{Kind: session.KindUser, Text: "older"})
	seed(t, "20260813-100300-bbbbbb", true, session.Record{Kind: session.KindUser, Text: "newer"})

	m := newJournalModel(t)
	out, _ := m.resumeSession("20260813-100200-aaaaaa")
	rm := out.(model)
	if len(rm.history) != 1 || rm.history[0].Content != "older" {
		t.Fatalf("resume by id restored %+v, want the named session", rm.history)
	}
}

func TestResumeNeverRestoresItsOwnSession(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	// This session's own turn is journalled, and it is the newest file. Replaying
	// it would duplicate the live conversation into itself.
	m.logSession(session.KindUser, "current turn")

	out, _ := m.resumeSession("")
	rm := out.(model)
	if len(rm.history) != 0 {
		t.Fatalf("history = %+v, want empty: the current session is not resumable", rm.history)
	}
	if !strings.Contains(rm.notice, "no saved session") {
		t.Fatalf("notice = %q, want 'no saved session'", rm.notice)
	}
}

func TestResumeOnUnknownIDReportsIt(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	out, cmd := m.resumeSession("20260813-999999-zzzzzz")
	rm := out.(model)
	if cmd != nil {
		t.Fatal("a failed resume must not print a transcript")
	}
	if len(rm.history) != 0 {
		t.Fatal("a failed resume must not change the conversation")
	}
	if !strings.Contains(rm.notice, "no saved session") {
		t.Fatalf("notice = %q", rm.notice)
	}
}

func TestResumeRejectsUnsafeIDWithoutTouchingHistory(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	out, _ := m.resumeSession("../../../etc/passwd")
	rm := out.(model)
	if len(rm.history) != 0 {
		t.Fatal("an invalid id must not restore anything")
	}
	if !strings.Contains(rm.notice, "resume failed") {
		t.Fatalf("notice = %q, want a failure report", rm.notice)
	}
}

func TestResumeIsRecordedInTheNewJournal(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-100400-aaaaaa", true,
		session.Record{Kind: session.KindUser, Text: "q"},
		session.Record{Kind: session.KindAssistant, Text: "a"},
	)
	m := newJournalModel(t)
	out, _ := m.resumeSession("")
	rm := out.(model)

	s, err := session.Load(rm.sessionID)
	if err != nil {
		t.Fatalf("Load current session: %v", err)
	}
	var marker string
	restored := 0
	for _, r := range s.Records {
		switch r.Kind {
		case session.KindResume:
			marker = r.Text
		case session.KindUser, session.KindAssistant:
			restored++
		}
	}
	if !strings.Contains(marker, "20260813-100400-aaaaaa") {
		t.Fatalf("resume marker = %q, want the source session id", marker)
	}
	// The restored turns already have a journal. Copying them would double every
	// transcript across a chain of resumes.
	if restored != 0 {
		t.Fatalf("restored turns were re-journalled (%d records)", restored)
	}
}

func TestJournalPartialOnlyWritesRealText(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.streamText = "   \n\t "
	m.journalPartial()
	m.streamText = "real partial output"
	m.journalPartial()

	s, err := session.Load(m.sessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var partials []string
	for _, r := range s.Records {
		if r.Kind == session.KindPartial {
			partials = append(partials, r.Text)
		}
	}
	if len(partials) != 1 || partials[0] != "real partial output" {
		t.Fatalf("partials = %+v, want exactly the non-blank one", partials)
	}
}

func TestInterruptJournalsThePartialReply(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.streaming = true
	m.streamText = "partially streamed answer"
	out, _ := m.interruptTurn()
	rm := out.(model)

	s, err := session.Load(rm.sessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.PartialTail(); got != "partially streamed answer" {
		t.Fatalf("PartialTail = %q, want the cancelled text journalled", got)
	}
}

// TestFinishedReplyIsJournalledUnrendered pins what a replayable transcript
// means. The finished reply is pretty-printed for the terminal — ANSI colour,
// re-wrapping, indentation — and journalling that instead of the model's own
// text would feed terminal control codes back to the model on the next resume.
// Nothing else in the suite would notice: both strings contain the same words.
func TestFinishedReplyIsJournalledUnrendered(t *testing.T) {
	isolateSessions(t)
	m := newJournalModel(t)
	m.streaming = true
	m.turnStartIndex = -1
	raw := "# Heading\n\nsome **bold** text with a `call()` in it\n"
	m.streamText = raw

	out, _ := m.finishStream()
	rm := out.(model)

	s, err := session.Load(rm.sessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got []string
	for _, r := range s.Records {
		if r.Kind == session.KindAssistant {
			got = append(got, r.Text)
		}
	}
	if len(got) != 1 {
		t.Fatalf("assistant records = %d, want 1", len(got))
	}
	if got[0] != raw {
		t.Errorf("journalled reply = %q,\nwant the model's own text %q", got[0], raw)
	}
	// Belt and braces: whatever the renderer does to the block, none of it
	// belongs in the transcript.
	if strings.Contains(got[0], "\x1b[") {
		t.Error("the journal must not contain terminal escape sequences")
	}
}

func TestLogSessionIsNilSafe(t *testing.T) {
	m := model{app: state.New(), width: 80}
	// A session with no journal (no config directory) must still take turns.
	m.logSession(session.KindUser, "text")
	m.journalPartial()
	if m.notice != "" {
		t.Fatalf("notice = %q, want silence when journalling is simply absent", m.notice)
	}
}

func TestSessionsScreenListsOtherSessions(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-100500-aaaaaa", false, session.Record{Kind: session.KindUser, Text: "q"})
	seed(t, "20260813-100600-bbbbbb", true,
		session.Record{Kind: session.KindUser, Text: "q"},
		session.Record{Kind: session.KindAssistant, Text: "a"},
	)
	m := newJournalModel(t)
	// The current session needs a file for its path to be meaningful.
	m.logSession(session.KindUser, "current")

	out := sessionsScreen(m.sessionID, m.journal, 100)
	for _, want := range []string{m.sessionID, "20260813-100500-aaaaaa", "20260813-100600-bbbbbb", "CRASHED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sessions screen missing %q:\n%s", want, out)
		}
	}
	// The current session must not be offered as something to resume into itself.
	if strings.Count(out, m.sessionID) != 1 {
		t.Fatalf("current session id appears %d times, want once (the header)", strings.Count(out, m.sessionID))
	}
}

func TestSessionsScreenWithoutAJournalSaysSo(t *testing.T) {
	out := sessionsScreen("some-id", nil, 100)
	if !strings.Contains(out, "not journalled") {
		t.Fatalf("a session with no journal must say so, got:\n%s", out)
	}
}

// renderCmd runs a tea.Cmd and returns what it would print. bubbletea's
// print-line message type is unexported, so the body is read through fmt rather
// than a type assertion — the alternative is not asserting on the output at all,
// and the output is the whole point of showing a user their lost reply.
// TestResumeRefusesMidTurn covers the states in which restoring a conversation
// would corrupt the one in progress. Slash commands are dispatched before the
// "wait for the reply to finish" gate in onSubmit, on purpose — /help and /exit
// have to work while a reply streams — so /resume has to refuse for itself.
//
// Without the guard the restored turns land between the live question and its
// answer, so the model sees "LIVE Q, OLD Q, OLD A" and answers the old one,
// while turnStartIndex still points at the live message that is now three
// positions from the end.
func TestResumeRefusesMidTurn(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-130000-aaaaaa", true,
		session.Record{Kind: session.KindUser, Text: "old question"},
		session.Record{Kind: session.KindAssistant, Text: "old answer"},
	)

	for _, c := range []struct {
		name  string
		setup func(*model)
	}{
		{"streaming", func(m *model) { m.streaming = true }},
		{"awaiting approval", func(m *model) { m.awaitingApproval = true }},
		{"awaiting team input", func(m *model) { m.awaitingTeamInput = true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newJournalModel(t)
			m.history = append(m.history, client.Message{Role: "user", Content: "live question"})
			m.turnStartIndex = len(m.history) - 1
			m.turnHistoryStable = true
			c.setup(&m)

			out, cmd := m.resumeSession("")
			rm := out.(model)
			if len(rm.history) != 1 || rm.history[0].Content != "live question" {
				t.Fatalf("history = %+v, want the live turn untouched", rm.history)
			}
			if rm.app.MsgCount != 0 {
				t.Errorf("MsgCount = %d, want 0: nothing was restored", rm.app.MsgCount)
			}
			if cmd != nil {
				t.Error("a refused resume must not print a transcript")
			}
			if !strings.Contains(rm.notice, "current turn") {
				t.Errorf("notice = %q, want an explanation of why it was refused", rm.notice)
			}
		})
	}

	// And once the turn is over, the same command works.
	m := newJournalModel(t)
	out, cmd := m.resumeSession("")
	if rm := out.(model); len(rm.history) != 2 {
		t.Fatalf("history = %d turns after the turn ended, want 2: the guard must not be permanent", len(rm.history))
	}
	if cmd == nil {
		t.Error("a successful resume must print the restored transcript")
	}
}

// TestResumeDoesNotLetAJournalAddressTheTerminal covers the one place this row
// prints file contents. A journal lives in the home directory and is read back
// on resume, so anything that can write there — a synced dotfile, another tool,
// a shared machine — chooses what the escape sequences in that print are. The
// conversation must keep the original text (the model has to see what it said),
// while the print must not.
func TestResumeDoesNotLetAJournalAddressTheTerminal(t *testing.T) {
	isolateSessions(t)
	// \x1b[2K erases the line; \r returns to its start. Together they can hide
	// arbitrary output from the listing above them.
	const evil = "answer\x1b[2K\rhidden"
	seed(t, "20260813-110000-aaaaaa", false,
		session.Record{Kind: session.KindUser, Text: "question\x1b[2Kspoofed"},
		session.Record{Kind: session.KindAssistant, Text: evil},
		session.Record{Kind: session.KindPartial, Text: "cut off\x1b[2Kpartial"},
	)

	m := newJournalModel(t)
	out, cmd := m.resumeSession("")
	rm := out.(model)

	// The conversation keeps the bytes the model produced.
	if len(rm.history) != 2 {
		t.Fatalf("history = %d turns, want 2", len(rm.history))
	}
	if rm.history[1].Content != evil {
		t.Errorf("restored content = %q, want the original %q: the model must see its own text",
			rm.history[1].Content, evil)
	}

	// What is printed does not.
	printed := renderCmd(t, cmd)
	if strings.Contains(printed, "\x1b[2K") {
		t.Errorf("an escape sequence from the journal reached the terminal:\n%q", printed)
	}
	if !strings.Contains(printed, `\x1b`) {
		t.Errorf("the escape should be shown in visible form:\n%s", printed)
	}
	// The partial block is printed too, and it is the same kind of text.
	if !strings.Contains(printed, "cut off") {
		t.Error("the unfinished reply should still be shown")
	}
}

// TestDisplaySafeBlockKeepsNewlines pins why the resume print needs its own
// sanitiser: escaping "\n" as well would collapse a restored multi-line answer
// into one unreadable line, which is a regression a security fix should not
// smuggle in.
func TestDisplaySafeBlockKeepsNewlines(t *testing.T) {
	got := clientcfg.DisplaySafeBlock("line one\nline two\x1b[2K")
	if !strings.Contains(got, "line one\nline two") {
		t.Errorf("newlines must survive: %q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("the escape must not: %q", got)
	}
	// The one-line variant still escapes them, for status bars and notices.
	if strings.Contains(clientcfg.DisplaySafe("a\nb"), "\n") {
		t.Error("DisplaySafe must keep escaping newlines: a notice is one line")
	}
}

// TestResumeModelCarriesTheTranscriptIntoInit is the `qeuro resume` entry point
// seen end to end: the restored history is in the model, and the command that
// prints it is in initCmd, where Init will run it. Dropping that command was a
// real bug during implementation — the restored turns were in context but the
// user saw an empty screen, which looks exactly like a resume that did nothing.
func TestResumeModelCarriesTheTranscriptIntoInit(t *testing.T) {
	isolateSessions(t)
	seed(t, "20260813-101000-aaaaaa", true,
		session.Record{Kind: session.KindUser, Text: "earlier question"},
		session.Record{Kind: session.KindAssistant, Text: "earlier answer"},
	)

	m, ok := resumeModel("test", "20260813-101000-aaaaaa")
	if !ok {
		t.Fatal("resumeModel reported failure on a session that loads")
	}
	t.Cleanup(func() { _ = m.journal.Close(time.Now()) })

	if len(m.history) != 2 {
		t.Fatalf("history = %d turns, want 2", len(m.history))
	}
	if m.initCmd == nil {
		t.Fatal("initCmd is nil: the restored transcript would never be printed")
	}
	if got := renderCmd(t, m.initCmd); !strings.Contains(got, "earlier question") {
		t.Fatalf("initCmd prints %q, want the restored transcript", got)
	}
	// The resumed session journals into its own new file, not the one it read.
	if m.sessionID == "20260813-101000-aaaaaa" {
		t.Fatal("a resumed session must open a new journal, not append to the old one")
	}
}

// TestInitOrdersTheWelcomeCardBeforeARestoredTranscript pins the one pair of
// prints in Init whose order is observable to the user. tea.Batch is documented
// to run commands "with no ordering guarantees", so a resumed transcript batched
// alongside the welcome card can land above it — and a test that only checks
// both were printed would never see it. Asserting the message type is the only
// way to tell the two apart: sequenceMsg is unexported, so compare its name.
func TestInitOrdersTheWelcomeCardBeforeARestoredTranscript(t *testing.T) {
	m := model{app: state.New(), width: 80, version: "test"}
	m.initCmd = tea.Println("RESTORED TRANSCRIPT")

	top, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init message = %T, want tea.BatchMsg", m.Init()())
	}
	if len(top) == 0 {
		t.Fatal("Init produced no commands")
	}
	inner := fmt.Sprintf("%T", top[0]())
	if inner != "tea.sequenceMsg" {
		t.Fatalf("welcome card + restored transcript are a %s, want tea.sequenceMsg:\n"+
			"batching them lets the transcript print above the welcome card", inner)
	}

	// And with nothing to restore, the pair collapses to the single print —
	// Sequence must not change the ordinary start.
	plain := model{app: state.New(), width: 80, version: "test"}
	ptop, ok := plain.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatal("Init message is not a tea.BatchMsg without a resume")
	}
	if got := fmt.Sprintf("%T", ptop[0]()); got == "tea.sequenceMsg" {
		t.Error("with no resume command the welcome print must stay a bare print")
	}
}

func renderCmd(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	return fmt.Sprintf("%v", cmd())
}
