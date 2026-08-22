package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/agentloop"
	"qeuro/internal/client"
	"qeuro/internal/state"
	"qeuro/internal/styles"
)

// noteToolForVerification tracks whether code edits still need a green
// verification command before the agent may finish the turn.
// Решение о состоянии gate принимает agentloop.NoteGate — тот же код, что и в
// headless-цикле. Здесь остаётся только раскладка результата по полям модели.
func (m *model) noteToolForVerification(c client.ToolCall, result string, mutated, ran bool) {
	g := agentloop.NoteGate(
		agentloop.Gate{
			Required: m.verificationRequired,
			Passed:   m.verificationPassed,
			Note:     m.verificationNote,
		},
		c.Function.Name, c.Function.Arguments, result, mutated, ran,
	)
	m.verificationRequired = g.Required
	m.verificationPassed = g.Passed
	m.verificationNote = g.Note
}

// Обёртки над agentloop: тесты TUI и вызовы внутри пакета продолжают
// пользоваться короткими именами, но реализация одна.
func isVerificationCommand(command string) bool {
	return agentloop.IsVerificationCommand(command)
}

func commandSucceeded(result string) bool {
	return agentloop.CommandSucceeded(result)
}

func firstResultLine(result string) string {
	return agentloop.FirstResultLine(result)
}

func (m model) enforceVerificationGate() (tea.Model, tea.Cmd) {
	m.toolStep++
	if m.toolStep > maxToolSteps {
		m.streaming = false
		m.endTurn()
		m.app.Phase = state.PhaseError
		block := styles.Message(styles.RoleError, clock(m.turnStarted), m.streamMeta,
			"verification did not pass within the step limit — task stopped", m.width)
		m.streamText, m.streamMeta, m.streamErr = "", "", ""
		return m, tea.Println(strings.TrimRight(block, "\n"))
	}

	attemptedFinal := strings.TrimSpace(m.streamText)
	if attemptedFinal != "" {
		m.history = append(m.history, client.Message{Role: "assistant", Content: attemptedFinal})
	}

	m.history = append(m.history, client.Message{Role: "user", Content: agentloop.GateMessage(m.verificationNote)})

	m.streamText, m.streamErr = "", ""
	m.app.Phase = state.PhaseGenerating
	m.app.Conn = state.Connecting
	ctx := m.turnCtx
	if ctx == nil {
		ctx = m.beginTurn()
	}
	block := styles.Message(styles.RoleSystem, clock(m.turnStarted), "quality gate",
		"a passing check is required after changes: test/build/lint/typecheck", m.width)
	return m, tea.Batch(
		tea.Println(strings.TrimRight(block, "\n")),
		startStream(ctx, m.provider, m.buildRequest()),
	)
}
