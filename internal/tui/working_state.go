package tui

import (
	"qeuro/internal/agentloop"
	"qeuro/internal/client"
)

// Сводка хода целиком живёт в agentloop: и формат строки, и окно записей, и
// заголовок сообщения. Здесь остаётся только связка с моделью TUI, потому что
// одинаковые действия должны давать модели одинаковый вход независимо от того,
// пришёл ход из терминала или из headless-движка.

const (
	maxWorkingStateItems = agentloop.MaxWorkingStateItems
	maxStateLineChars    = agentloop.MaxStateLineChars
)

// noteWorkingState records a compact, stable view of the current turn. It is
// sent on later tool-loop requests so trimming does not erase useful evidence.
func (m *model) noteWorkingState(c client.ToolCall, result string, mutated, ran bool) {
	m.workingState.Note(summarizeWorkStep(c, result, mutated, ran))
}

func (m model) workingStateMessage() string {
	return m.workingState.Message(agentloop.Gate{
		Required: m.verificationRequired,
		Passed:   m.verificationPassed,
		Note:     m.verificationNote,
	})
}

func summarizeWorkStep(c client.ToolCall, result string, mutated, ran bool) string {
	return agentloop.SummarizeStep(c.Function.Name, c.Function.Arguments, result, mutated, ran)
}

func clipStateLine(s string) string {
	return agentloop.ClipStateLine(s)
}
