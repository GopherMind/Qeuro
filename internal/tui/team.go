package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/session"
	"qeuro/internal/skills"
	"qeuro/internal/state"
	"qeuro/internal/styles"
	"qeuro/internal/team"
)

// teamEvent is one item on a running team's progress channel: a progress event,
// a request for user input (ask), or the terminal done signal with the summary.
type teamEvent struct {
	ev      team.Event
	ask     bool   // engine is asking the user a clarifying question
	askText string // the question text (when ask)
	done    bool
	summary string
	err     error
}

// teamEventMsg delivers one teamEvent to the Bubble Tea update loop.
type teamEventMsg struct {
	te teamEvent
	ok bool
}

// profileName returns the team profile label for the current tier.
func (m model) profileName() string {
	return team.ProfileForTier(m.tier).Name
}

// toggleTeam flips team mode on/off, loading the skill library on first enable.
func (m model) toggleTeam() (tea.Model, tea.Cmd) {
	if m.streaming {
		m.notice = "wait for the current reply to finish…"
		return m, nil
	}
	if m.teamMode {
		m.teamMode = false
		m.notice = "team mode off"
		return m, nil
	}
	// Enable: lazily load the skill library.
	if m.skills == nil {
		lib, _ := skills.Load()
		m.skills = lib
	}
	m.teamMode = true
	n := m.skills.Count()
	m.notice = "team mode ON · profile «" + m.profileName() + "» · skills: " + itoa(n) +
		" · asks for details when the task is vague · edits and tests auto"
	return m, nil
}

// startTeamRun launches the orchestration engine in a goroutine and returns a
// command that waits for its first progress event. Progress and the final
// summary arrive as teamEventMsg values. The run is bound to the per-turn
// context (H2): cancelling it (Esc/quit) unblocks the engine's chat calls so the
// goroutine exits instead of being orphaned, and an ask that is waiting on the
// user is released so it cannot deadlock the goroutine forever.
func (m model) startTeamRun(ctx context.Context, task string) (model, tea.Cmd) {
	ch := make(chan teamEvent, 64)
	reply := make(chan string, 1)
	m.teamCh = ch
	m.teamReplyCh = reply
	m.streaming = true
	m.app.Phase = state.PhaseGenerating
	m.app.Conn = state.Online

	cli := m.provider
	runner := m.runner
	lib := m.skills
	prof := team.ProfileForTier(m.tier)
	// Captured here rather than read off m inside the goroutine: the model is
	// copied by value through the Bubble Tea update loop, and reading a field of
	// the enclosing copy from a background goroutine is the pattern H2 forbids.
	unsafeWrites := m.unsafeParallelWrites

	go func() {
		emit := func(ev team.Event) {
			// Drop events once cancelled so a full buffer can't wedge the goroutine.
			select {
			case ch <- teamEvent{ev: ev}:
			case <-ctx.Done():
			}
		}
		// ask blocks the engine until the user answers via teamReplyCh, or until
		// the turn is cancelled (so Esc during a clarification doesn't hang it).
		ask := func(q string) string {
			select {
			case ch <- teamEvent{ask: true, askText: q}:
			case <-ctx.Done():
				return ""
			}
			select {
			case r := <-reply:
				return r
			case <-ctx.Done():
				return ""
			}
		}
		eng := team.New(cli, runner, lib, prof, emit, ask)
		// roadmap-v3 §4.1 rollout flag. Off by default: parallel steps are
		// read-only because concurrent writers in one tree silently drop edits.
		eng.AllowUnsafeParallelWrites(unsafeWrites)
		summary, err := eng.Run(ctx, task)
		select {
		case ch <- teamEvent{done: true, summary: summary, err: err}:
		case <-ctx.Done():
		}
		close(ch)
	}()

	return m, waitTeam(ch)
}

// answerTeam delivers the user's reply to a team run that paused to ask a
// question, echoes it into the transcript, and resumes consuming team events.
func (m model) answerTeam(text string) (tea.Model, tea.Cmd) {
	m.input.SetValue("")
	m.input.SetHeight(1)
	m.notice = ""
	full := m.expandPastes(text)
	m.pastes = nil

	m.awaitingTeamInput = false
	m.app.Phase = state.PhaseGenerating
	userBlock := styles.Message(styles.RoleUser, clock(m.turnStarted), "", text, m.width)
	m.history = append(m.history, client.Message{Role: "user", Content: full})
	m.logSession(session.KindUser, full)

	if m.teamReplyCh != nil {
		// Non-blocking send (L8): the channel is buffered (cap 1) and the engine
		// consumes one answer at a time, but if the run was cancelled/finished and
		// is no longer reading, a plain send could wedge the UI goroutine. The
		// default branch drops the answer rather than blocking.
		select {
		case m.teamReplyCh <- full:
		default:
		}
	}
	return m, tea.Batch(
		tea.Println(strings.TrimRight(userBlock, "\n")),
		waitTeam(m.teamCh),
	)
}

// waitTeam blocks for the next event on a team run's channel.
func waitTeam(ch chan teamEvent) tea.Cmd {
	return func() tea.Msg {
		te, ok := <-ch
		return teamEventMsg{te: te, ok: ok}
	}
}

// onTeamEvent applies one team progress event: printing a line and waiting for
// the next, or finalising the run on the done signal.
func (m model) onTeamEvent(msg teamEventMsg) (tea.Model, tea.Cmd) {
	// Stale event from a team run the user interrupted: ignore and do not re-arm.
	if m.interrupted || m.teamCh == nil {
		return m, nil
	}
	if !msg.ok {
		// Channel closed unexpectedly without a done signal.
		m.streaming = false
		m.teamCh = nil
		m.endTurn()
		return m, nil
	}
	te := msg.te

	if te.ask {
		// The team paused for clarification. Show the questions and switch to
		// input mode; the engine is blocked until the next user message, which
		// onSubmit routes back via teamReplyCh. Do NOT re-arm waitTeam here.
		m.awaitingTeamInput = true
		m.app.Phase = state.PhaseIdle
		block := styles.Message(styles.RoleAgent, clock(m.turnStarted), "team · clarification",
			te.askText+"\n\n(reply in one message — the team will continue)", m.width)
		return m, tea.Println(strings.TrimRight(block, "\n"))
	}

	if te.done {
		m.streaming = false
		m.teamCh = nil
		m.endTurn()
		if te.err != nil {
			m.app.Phase = state.PhaseError
			block := styles.Message(styles.RoleError, clock(m.turnStarted), "team",
				"team aborted: "+te.err.Error(), m.width)
			return m, tea.Println(strings.TrimRight(block, "\n"))
		}
		m.app.Phase = state.PhaseDone
		body := strings.TrimSpace(te.summary)
		if body == "" {
			body = "(the team finished without a final summary)"
		}
		m.history = append(m.history, client.Message{Role: "assistant", Content: body})
		m.logSession(session.KindAssistant, body)
		block := styles.Message(styles.RoleAgent, clock(m.turnStarted), "team · "+m.profileName(), body, m.width)
		return m, tea.Println(strings.TrimRight(block, "\n"))
	}

	line := renderTeamEvent(te.ev)
	if line == "" {
		return m, waitTeam(m.teamCh)
	}
	return m, tea.Batch(tea.Println(line), waitTeam(m.teamCh))
}

// renderTeamEvent formats one engine event as a single scrollback line, in the
// project's gutter-and-glyph visual language.
func renderTeamEvent(ev team.Event) string {
	switch ev.Kind {
	case team.EvPhase:
		return "  " + styles.Accent.Render("▎ "+ev.Text)
	case team.EvAgentStart:
		s := "  " + styles.UserTag.Render("▸ "+ev.Role) + styles.Muted.Render(" working")
		if ev.Text != "" {
			s += styles.Subtle.Render("  · skill ") + styles.Muted.Render(ev.Text)
		}
		return s
	case team.EvAgentEnd:
		s := "  " + styles.OK.Render("✓ "+ev.Role)
		if ev.Text != "" {
			s += styles.Muted.Render("  " + ev.Text)
		}
		return s
	case team.EvTool:
		role := ev.Role
		if role == "" {
			role = "tool"
		}
		return "    " + styles.Subtle.Render("⚙ ") + styles.Muted.Render(role+" · "+ev.Text)
	case team.EvInfo:
		return "  " + styles.Subtle.Render("· ") + styles.Muted.Render(ev.Text)
	case team.EvError:
		return "  " + styles.Err.Render("✗ "+ev.Role) + styles.Muted.Render("  "+ev.Text)
	default:
		return ""
	}
}
