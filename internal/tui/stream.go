package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/state"
	"qeuro/internal/styles"
)

// beginTurn creates the per-turn cancelable context (H2). Any previous turn
// context is cancelled first so a context is never leaked.
func (m *model) beginTurn() context.Context {
	if m.turnCancel != nil {
		m.turnCancel()
	}
	m.interrupted = false
	m.turnCtx, m.turnCancel = context.WithCancel(context.Background())
	return m.turnCtx
}

// endTurn releases the per-turn context after a turn finishes or is aborted.
func (m *model) endTurn() {
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	m.turnCtx = nil
}

// startStream opens the configured inference stream off the UI goroutine,
// bound to the per-turn context so the request can be cancelled (H2). The
// provider is either the backend or the local model; callers do not branch.
func startStream(ctx context.Context, provider client.Provider, req client.ChatRequest) tea.Cmd {
	return func() tea.Msg {
		ch, err := provider.Chat(ctx, req)
		return streamStartMsg{ch: ch, err: err}
	}
}

// onStreamStart handles the connection result.
func (m model) onStreamStart(msg streamStartMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.streaming = false
		m.app.Phase = state.PhaseError
		m.app.Conn = state.Offline
		where := "backend"
		if m.local {
			where = "local model at " + clientcfg.DisplaySafe(m.localAt)
		}
		detail := clientcfg.DisplaySafeBlock(msg.err.Error())
		block := styles.Message(styles.RoleError, clock(time.Now()), "", "could not connect to the "+where+": "+detail, m.width)
		return m, tea.Println(strings.TrimRight(block, "\n"))
	}
	m.streamCh = msg.ch
	m.app.Conn = state.Online
	return m, waitStream(m.streamCh)
}

// onStreamEvent applies one SSE event and either continues or finalises.
func (m model) onStreamEvent(msg streamEventMsg) (tea.Model, tea.Cmd) {
	// Stale event from a turn the user interrupted: ignore and do not re-arm.
	if m.interrupted || m.streamCh == nil {
		return m, nil
	}
	if !msg.ok {
		return m.finishStream()
	}

	switch msg.ev.Kind {
	case client.EventRoute:
		if msg.ev.Route != nil {
			// A fresh route (e.g. an escalation) replaces the prior partial text.
			m.streamText = ""
			meta := msg.ev.Route.Model + " · " + msg.ev.Route.Effort
			if msg.ev.Route.Escalated {
				meta += " ⤴ escalated"
			}
			m.streamMeta = meta
		}
	case client.EventToken:
		m.streamText += msg.ev.Text
	case client.EventToolCalls:
		m.pendingCalls = msg.ev.ToolCalls
	case client.EventUsage:
		m.lastUsage = msg.ev.Usage
		if msg.ev.Usage != nil {
			m.credits = msg.ev.Usage.Balance
			m.creditsKnown = true
			m.app.CtxUsed = msg.ev.Usage.In
			if m.app.CtxUsed > m.app.CtxLimit {
				m.app.CtxUsed = m.app.CtxLimit
			}
			m.app.Usage.RecordUsage(state.UsageRecord{
				InputTokens:       msg.ev.Usage.In,
				OutputTokens:      msg.ev.Usage.Out,
				CachedInputTokens: msg.ev.Usage.CachedInputTokens,
				CostUSD:           msg.ev.Usage.CostUSD,
				Credits:           msg.ev.Usage.Credits,
				SavedUSD:          msg.ev.Usage.SavedUSD,
				Balance:           msg.ev.Usage.Balance,
			})
			// The receipt is the only place the client learns what a call actually
			// cost, so it is where the session ceiling accumulates. Recording here
			// rather than at the end of a turn means a tool loop's spend is known
			// between its steps, which is what makes the gate in runToolCalls able
			// to stop one.
			m.budget.record(msg.ev.Usage.Credits)
		}
	case client.EventError:
		// Provider errors can contain server-controlled text. Preserve newlines for
		// a readable error block, but neutralise terminal control sequences before
		// the value reaches scrollback.
		m.streamErr = clientcfg.DisplaySafeBlock(msg.ev.ErrMsg)
	}

	return m, waitStream(m.streamCh)
}

// finishStream is called when the SSE channel closes. If the model requested
// tool calls, it executes them locally and continues the turn; otherwise it
// flushes the final (or errored) reply to scrollback.
func (m model) finishStream() (tea.Model, tea.Cmd) {
	m.streamCh = nil

	// Stream-level error: stop the turn.
	if m.streamErr != "" {
		m.streaming = false
		m.pendingCalls = nil
		m.turnStartIndex = -1
		m.turnHistoryStable = false
		m.endTurn()
		m.app.Phase = state.PhaseError
		m.logSession(session.KindError, m.streamErr)
		block := styles.Message(styles.RoleError, clock(m.turnStarted), m.streamMeta, m.streamErr, m.width)
		m.streamText, m.streamMeta, m.streamErr = "", "", ""
		return m, tea.Println(strings.TrimRight(block, "\n"))
	}

	// Tool calls: run them locally and continue the conversation.
	if len(m.pendingCalls) > 0 {
		return m.runToolCalls()
	}

	if m.verificationRequired {
		return m.enforceVerificationGate()
	}

	// Final answer.
	m.streaming = false
	m.turnStartIndex = -1
	m.turnHistoryStable = false
	m.endTurn()
	m.app.Phase = state.PhaseDone
	body := m.streamText
	if strings.TrimSpace(body) == "" {
		body = "(empty reply)"
	}
	// M5.2: pretty-print the finished reply as terminal Markdown (glamour).
	// Live partial chunks above stay plain; only the flushed block is styled.
	body = renderMarkdown(body, m.width-8)
	block := styles.Message(styles.RoleAgent, clock(m.turnStarted), m.streamMeta, body, m.width)
	m.history = append(m.history, client.Message{Role: "assistant", Content: m.streamText})
	// The journal gets the model's text, not the rendered block: replaying
	// terminal formatting back into the conversation is not the same reply.
	m.logSession(session.KindAssistant, m.streamText)
	if m.lastUsage != nil {
		m.notice = fmt.Sprintf("usage %s in / %s out · %s credits",
			formatCompactInt(m.lastUsage.In), formatCompactInt(m.lastUsage.Out), fmtCredits(m.lastUsage.Credits))
	}
	m.streamText, m.streamMeta, m.streamErr = "", "", ""
	return m, tea.Println(strings.TrimRight(block, "\n"))
}
