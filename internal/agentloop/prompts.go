package agentloop

import (
	"fmt"
	"strings"
)

// Тексты, которые цикл кладёт в историю разговора.
//
// Они живут здесь, а не в TUI, ровно потому же, почему здесь живут решения:
// это часть контракта цикла с моделью. Headless-запуск, который сообщает модели
// про лимит шагов другими словами, чем терминал, — это другой агент с тем же
// именем, и расхождение вылезло бы как разное поведение в облаке и локально.

// ToolLimitWarning предупреждает модель, что шаги заканчиваются.
func ToolLimitWarning(remaining int) string {
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("TOOL LOOP WARNING: only %d tool steps remain in this turn. Stop broad exploration. Use the next tool calls only for the smallest missing evidence, edit, or verification, then finish with a concrete status.", remaining)
}

// ToolLimitFinal требует закончить без тулов.
func ToolLimitFinal(reason string) string {
	return "TOOL LOOP LIMIT: " + reason + ". You must now finish WITHOUT tool calls. Summarize what was completed, cite the last concrete evidence from WORKING STATE, and state the exact blocker or next manual command if unresolved. Do not request more tools."
}

// GateMessage возвращает модель в цикл, пока проверка не прошла.
func GateMessage(note string) string {
	return "QUALITY GATE: You changed code, but there is no successful verification command after the latest change. " +
		"Do not summarize or stop yet. Run a focused build/test/lint/typecheck command with run_command. " +
		"If it fails, inspect the errors, patch the code, and re-run verification until it passes. " +
		"Latest status: " + note
}

// VerifyLimitMessage — ход остановлен: код изменён, а зелёной проверки так и не
// появилось за отведённые шаги. Текст совпадает с тем, что печатает TUI.
const VerifyLimitMessage = "verification did not pass within the step limit — task stopped"

// BudgetStopMessage — что видит модель, когда ход прекращён потолком.
//
// Модели сообщают причину в её собственной истории: необъяснённый обрыв
// посреди цикла учит её, что бросить работу на середине — нормальный финал.
const BudgetStopMessage = "SESSION BUDGET REACHED: the user's credit ceiling for this session was hit, so this turn was stopped before it finished. No further tool calls will run."

const (
	// MaxWorkingStateItems — окно последних записей сводки.
	MaxWorkingStateItems = 14
	// MaxStateLineChars — предел длины одной строки сводки.
	MaxStateLineChars = 220
)

// WorkingState — компактная сводка хода, которая переживает обрезку истории.
type WorkingState struct {
	lines []string
}

// Note добавляет строку и держит окно последних записей: сводка существует
// затем, чтобы не расти, иначе она стала бы второй копией истории.
func (w *WorkingState) Note(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.lines = append(w.lines, ClipStateLine(line))
	if len(w.lines) > MaxWorkingStateItems {
		w.lines = w.lines[len(w.lines)-MaxWorkingStateItems:]
	}
}

// Lines отдаёт копию: вызывающий не должен уметь править внутреннее окно.
func (w WorkingState) Lines() []string {
	return append([]string(nil), w.lines...)
}

// Len — сколько записей в окне.
func (w WorkingState) Len() int { return len(w.lines) }

// Message собирает системное сообщение WORKING STATE. Пустое, если нечего
// сообщать: заголовок без записей — потраченные токены.
func (w WorkingState) Message(g Gate) string {
	if len(w.lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("WORKING STATE (compact evidence from this turn; trust it over omitted old tool output):")
	for _, line := range w.lines {
		b.WriteString("\n- ")
		b.WriteString(line)
	}
	if g.Required {
		b.WriteString("\n- verification pending: ")
		b.WriteString(ClipStateLine(g.Note))
	} else if g.Passed {
		b.WriteString("\n- ")
		b.WriteString(ClipStateLine(g.Note))
	}
	return b.String()
}

// ClipStateLine сжимает пробелы и ограничивает длину строки сводки.
func ClipStateLine(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return "(no output)"
	}
	if len(s) > MaxStateLineChars {
		return s[:MaxStateLineChars] + "..."
	}
	return s
}
