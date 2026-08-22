package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/agentloop"
	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/state"
	"qeuro/internal/styles"
	"qeuro/internal/tools"
)

// runToolCalls records the assistant tool-call turn, bounds runaway loops, and
// starts executing the requested calls through the local approval pipeline.
func (m model) runToolCalls() (tea.Model, tea.Cmd) {
	m.toolStep++
	if m.toolStep > maxToolSteps {
		return m.finalizeToolLimit("tool loop reached its step limit")
	}

	// The session's spend ceiling is enforced here, not only at submit time. Each
	// tool step is another billed provider call that the loop starts on its own,
	// so this is the point where continuing costs money and where a hard budget
	// has to be able to say no. The check runs before the assistant turn is
	// recorded, so the stopped turn does not leave a tool-call message in history
	// with no results answering it.
	if m.budget.exhausted() {
		return m.stopForBudget()
	}

	calls := append([]client.ToolCall(nil), m.pendingCalls...)
	m.history = append(m.history, client.Message{
		Role:      "assistant",
		Content:   m.streamText,
		ToolCalls: calls,
	})

	m.turnPreface = m.streamText
	m.toolQueue = calls
	m.pendingCalls = nil
	m.toolResults = nil
	m.toolLines = nil
	m.streamText, m.streamErr = "", ""

	if !m.toolWarned {
		remaining := maxToolSteps - m.toolStep
		if remaining <= toolLimitWarnSteps {
			m.toolWarned = true
			m.history = append(m.history, client.Message{Role: "user", Content: toolLimitWarning(remaining)})
		}
	}

	return m.advanceTools()
}

// Тексты для модели и скрининг вызова берутся из internal/agentloop, чтобы
// терминал и headless говорили модели одно и то же.
func toolLimitWarning(remaining int) string {
	return agentloop.ToolLimitWarning(remaining)
}

func (m model) finalizeToolLimit(reason string) (tea.Model, tea.Cmd) {
	m.pendingCalls = nil
	m.toolQueue = nil
	m.toolResults = nil
	m.toolLines = nil
	// The tool-role notes for this step are discarded, so their fenced payloads
	// have nothing left to belong to. Keeping them would inject a block from an
	// abandoned step into a later turn, where the model sees third-party data with
	// no call that asked for it.
	m.untrustedBlocks = nil
	m.turnPreface = ""
	m.streamText, m.streamErr = "", ""
	m.awaitingApproval = false
	m.pendingTool = nil
	m.pendingPreview = ""
	m.toolFinal = true
	m.app.Phase = state.PhaseGenerating
	m.app.Conn = state.Connecting

	m.history = append(m.history, client.Message{Role: "user", Content: agentloop.ToolLimitFinal(reason)})

	ctx := m.turnCtx
	if ctx == nil {
		ctx = m.beginTurn()
	}
	block := styles.Message(styles.RoleSystem, clock(m.turnStarted), "tool limit", "tool loop limit reached; asking model for final no-tools status", m.width)
	return m, tea.Batch(
		tea.Println(strings.TrimRight(block, "\n")),
		startStream(ctx, m.provider, m.buildRequest()),
	)
}

// stopForBudget ends the turn because the session ceiling was reached. Unlike
// finalizeToolLimit it does not ask the model for a closing summary: that would
// be one more billed call, which is the thing the ceiling exists to prevent. The
// turn simply ends, and the partial text is kept for the same reason a cancelled
// turn keeps it — the tokens were produced and billed.
func (m model) stopForBudget() (tea.Model, tea.Cmd) {
	m.budget.stopped = true
	// interrupted is what onStreamEvent checks to drop events from a turn that is
	// already over (stream.go). Cancelling the context is not enough on its own:
	// events already queued as Bubble Tea messages arrive after the cancel, and
	// without this flag they would append tokens to a turn that was stopped and
	// re-arm the reader, printing a reply under a ceiling that just refused one.
	m.interrupted = true
	m.pendingCalls = nil
	m.toolQueue = nil
	m.toolResults = nil
	m.toolLines = nil
	// Same reason as finalizeToolLimit and interruptTurn: fenced payloads from a
	// step that will never complete must not ride into the next turn.
	m.untrustedBlocks = nil
	m.awaitingApproval = false
	m.pendingTool = nil
	m.pendingPreview = ""
	m.approvalChoice = 0
	m.toolFinal = false

	m.journalPartial()
	m.keepPartialInHistory()
	// The model is told in its own history why the turn ended, so a later turn
	// does not read the stop as a task it abandoned voluntarily.
	m.history = append(m.history, client.Message{Role: "user", Content: budgetStopMessage})

	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	m.streaming = false
	m.streamCh = nil
	m.turnStartIndex = -1
	m.turnHistoryStable = false
	m.turnPreface = ""
	m.streamText, m.streamMeta, m.streamErr = "", "", ""
	m.app.Phase = state.PhaseIdle
	m.app.Conn = state.Online

	block := styles.Message(styles.RoleSystem, clock(m.turnStarted), "budget", m.budget.notice(), m.width)
	return m, tea.Println(strings.TrimRight(block, "\n"))
}

// advanceTools executes queued tool calls until a call needs user approval or
// all results are ready to be sent back to the model.
func (m model) advanceTools() (tea.Model, tea.Cmd) {
	for len(m.toolQueue) > 0 {
		c := m.toolQueue[0]
		m.toolQueue = m.toolQueue[1:]

		name := c.Function.Name

		// Решение принимает agentloop.ScreenCall — тот же код, что и в headless.
		// Скрининг именно здесь: имя вне реестра нельзя ни исполнить, ни осмысленно
		// показать в предпросмотре, а реестр включает MCP-тулы, которые
		// пользователь не разрешал.
		verdict := agentloop.ScreenCall(name, c.Function.Arguments, approvalMode(m.app.Approvals))
		if verdict.Blocked != "" {
			// Имя тула контролируется моделью и попадает в scrollback.
			m.recordTool(c, clientcfg.DisplaySafe(verdict.Blocked), false, false)
			continue
		}
		if verdict.NeedsApproval {
			m.awaitingApproval = true
			m.approvalChoice = 0
			m.pendingTool = &c
			m.pendingPreview = tools.Preview(name, c.Function.Arguments)
			m.app.Phase = state.PhaseGenerating
			return m, nil
		}

		return m, m.execCallCmd(c)
	}

	return m.continueAfterTools()
}

// execCallCmd picks the executor for one call: the local runner for built-ins,
// the MCP manager for a registered external tool.
func (m model) execCallCmd(c client.ToolCall) tea.Cmd {
	if tools.IsMCPName(c.Function.Name) {
		return execMCPCmd(m.turnCtx, m.mcp, c)
	}
	return execToolCmd(c, m.runner)
}

// onToolDone applies an async tool result and advances to the next queued call.
func (m model) onToolDone(msg toolDoneMsg) (tea.Model, tea.Cmd) {
	m.recordTool(msg.call, msg.result, msg.mutated, true)
	if msg.untrusted != "" {
		m.untrustedBlocks = append(m.untrustedBlocks, msg.untrusted)
	}
	return m.advanceTools()
}

func (m model) resolveApproval(approved bool) (tea.Model, tea.Cmd) {
	if m.pendingTool == nil {
		m.awaitingApproval = false
		return m, nil
	}
	c := *m.pendingTool

	// Solo mode via agentHost: send decision to engine and continue listening
	if m.agentHost != nil {
		status := approvalStatus(approved, tools.Summary(c.Function.Name, c.Function.Arguments))
		erase := eraseOverlayCmd(status)

		m.awaitingApproval = false
		m.pendingTool = nil
		m.pendingPreview = ""
		m.approvalChoice = 0
		m.announcedToolID = c.ID

		// Send decision to engine
		m.agentHost.approve(c.ID, approved)

		// Continue listening for events
		return m, seqCmds(erase, m.agentHost.listenEvents())
	}

	// Legacy toolloop path (team mode or old stream path)
	// Claude Code-style cleanup: the whole approval frame is erased from the
	// terminal the moment the user decides, and only a one-line ✓/✗ status is
	// printed in its place, so the panel never lands in the scrollback. The
	// erase + status println is sequenced before the tool command so output
	// ordering stays deterministic on every OS (see overlay.go).
	status := approvalStatus(approved, tools.Summary(c.Function.Name, c.Function.Arguments))
	erase := eraseOverlayCmd(status)

	m.awaitingApproval = false
	m.pendingTool = nil
	m.pendingPreview = ""
	m.approvalChoice = 0
	// recordTool must not add a second transcript line for this call — the
	// status line above already logs it.
	m.announcedToolID = c.ID

	if !approved {
		m.recordTool(c, "rejected by user", false, false)
		next, cmd := m.advanceTools()
		return next, seqCmds(erase, cmd)
	}

	return m, seqCmds(erase, m.execCallCmd(c))
}

func execToolCmd(c client.ToolCall, runner *tools.Runner) tea.Cmd {
	return func() tea.Msg {
		if runner == nil {
			return toolDoneMsg{call: c, result: "error: tool runner is not available", mutated: false}
		}
		result, mutated := runner.Execute(c.Function.Name, c.Function.Arguments)
		return toolDoneMsg{call: c, result: result, mutated: mutated}
	}
}

// approvalMode переводит режим подтверждений сессии в режим agentloop.
func approvalMode(m state.Approval) agentloop.ApprovalMode {
	if m == state.ApprovalAsk {
		return agentloop.ApprovalAsk
	}
	return agentloop.ApprovalAuto
}
