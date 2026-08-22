package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/agentcore"
	"qeuro/internal/agentloop"
	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/session"
	"qeuro/internal/state"
	"qeuro/internal/styles"
)

// onAgentEvent handles one event from the agentcore.Engine host adapter.
func (m model) onAgentEvent(msg agentEventMsg) (tea.Model, tea.Cmd) {
	ev := msg.ev

	switch ev.Kind {
	case agentcore.KindRoute:
		// New route: model + effort
		m.streamMeta = ev.Model
		if ev.Text != "" {
			m.streamMeta += " · " + ev.Text
		}
		m.app.Conn = state.Connecting
		return m, m.agentHost.listenEvents()

	case agentcore.KindToken:
		// Stream token: accumulate into streamText
		m.streamText += ev.Text
		return m, m.agentHost.listenEvents()

	case agentcore.KindAssistant:
		// Final assistant message
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			text = "(empty reply)"
		}
		m.streamText = text
		m.streamMeta = ev.Model
		return m, m.agentHost.listenEvents()

	case agentcore.KindStatus:
		// Status message (budget warning, gate, etc.)
		// Print it inline as a system message
		line := styles.Message(styles.RoleSystem, "", ev.Name, ev.Text, m.width)
		m.logSession(session.KindPartial, ev.Text)
		return m, tea.Batch(
			tea.Println(strings.TrimRight(line, "\n")),
			m.agentHost.listenEvents(),
		)

	case agentcore.KindToolCall:
		// Generic tool call event (non-mutating or approval already granted)
		m.toolStep++
		line := agentloop.FirstResultLine(ev.Text)
		m.toolLines = append(m.toolLines, "• "+ev.Name+": "+line)
		return m, m.agentHost.listenEvents()

	case agentcore.KindFileWrite:
		// File write event
		m.toolStep++
		m.verificationRequired = true
		display := "• edited " + ev.Path
		if ev.Diff != "" {
			display += "\n" + clientcfg.DisplaySafeBlock(ev.Diff)
		}
		m.toolLines = append(m.toolLines, display)
		return m, m.agentHost.listenEvents()

	case agentcore.KindCommand:
		// Command execution event
		m.toolStep++
		display := "• ran " + clientcfg.DisplaySafe(ev.Cmd)
		if ev.ExitCode != nil {
			if *ev.ExitCode == 0 {
				// Successful command: check if it satisfies verification gate
				if m.verificationRequired && !m.verificationPassed {
					if agentloop.IsVerificationCommand(ev.Cmd) {
						m.verificationPassed = true
						m.verificationNote = ev.Cmd
					}
				}
			} else {
				display += " (exit " + strconv.Itoa(*ev.ExitCode) + ")"
			}
		}
		m.toolLines = append(m.toolLines, display)
		return m, m.agentHost.listenEvents()

	case agentcore.KindApprovalRequest:
		// Approval request: show the overlay
		m.awaitingApproval = true
		m.approvalChoice = 0
		m.announcedToolID = ev.ID
		m.pendingPreview = clientcfg.DisplaySafeBlock(ev.Preview)
		// For the overlay display: store action as a pseudo-tool call
		// This matches what the old toolloop path expected
		m.pendingTool = &client.ToolCall{
			ID: ev.ID,
			Function: client.FunctionCall{
				Name:      ev.Action,
				Arguments: "", // preview is already formatted
			},
		}
		return m, m.agentHost.listenEvents()

	case agentcore.KindUsage:
		// Usage/billing event
		if ev.CostUSD > 0 {
			m.budget.spent += ev.CostUSD
		}
		m.lastUsage = &client.Usage{
			In:      ev.TokensIn,
			Out:     ev.TokensOut,
			CostUSD: ev.CostUSD,
		}
		return m, m.agentHost.listenEvents()

	case agentcore.KindError:
		// Error event
		m.streamErr = clientcfg.DisplaySafeBlock(ev.Text)
		return m, m.agentHost.listenEvents()

	default:
		// Unknown event: ignore and continue
		return m, m.agentHost.listenEvents()
	}
}

// onAgentDone handles the terminal done event from the engine.
func (m model) onAgentDone(msg agentDoneMsg) (tea.Model, tea.Cmd) {
	m.streaming = false
	m.app.Conn = state.Online
	m.app.Phase = state.PhaseIdle
	m.agentHost = nil
	m.endTurn()

	// Print accumulated tool lines
	var cmds []tea.Cmd
	for _, line := range m.toolLines {
		cmds = append(cmds, tea.Println(line))
	}
	m.toolLines = nil

	switch msg.status {
	case agentcore.DoneCancelled:
		// User cancelled
		m.interrupted = true
		line := styles.Message(styles.RoleSystem, "", "cancelled", "interrupted by user", m.width)
		cmds = append(cmds, tea.Println(strings.TrimRight(line, "\n")))
		m.logSession(session.KindPartial, "cancelled by user")

	case agentcore.DoneError:
		// Engine error
		errText := m.streamErr
		if errText == "" {
			errText = "engine failed"
		}
		line := styles.Message(styles.RoleError, "", "error", errText, m.width)
		cmds = append(cmds, tea.Println(strings.TrimRight(line, "\n")))
		m.logSession(session.KindError, errText)

	case agentcore.DoneOK:
		// Success: print final assistant message
		text := strings.TrimSpace(m.streamText)
		if text != "" {
			line := styles.Message(styles.RoleAgent, "", m.streamMeta, text, m.width)
			cmds = append(cmds, tea.Println(strings.TrimRight(line, "\n")))
			m.logSession(session.KindAssistant, text)
		}
	}

	return m, tea.Batch(cmds...)
}
