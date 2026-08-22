// Package agentloop — решения цикла агента, отделённые от того, кто их рисует.
//
// Роадмап Фаза 0 (§5.3, §4.8, §12) требует перенести цикл из TUI в agentcore,
// чтобы headless-запуск, десктоп и облачный воркер шли тем же путём, что и
// терминал. Прямой перенос ~830 строк из `internal/tui/update.go` создал бы
// вторую реализацию: TUI продолжил бы жить своей копией, и две копии разошлись
// бы на первой же правке — а расходятся здесь не пиксели, а gate верификации,
// потолок бюджета и лимит шагов, то есть места, где цикл решает тратить деньги
// и менять файлы.
//
// Поэтому решения вынесены сюда как чистые функции над явным состоянием, без
// Bubble Tea, без терминала и без сети. TUI и agentcore спрашивают у одного и
// того же кода «что делать дальше» и различаются только тем, как показывают
// ответ: tea.Cmd и scrollback против JSONL-событий.
//
// Инвариант: этот пакет ничего не исполняет. Он не вызывает тулы, не открывает
// стримы и не пишет в файлы — он возвращает решение, а исполняет вызывающий.
// Так решение остаётся проверяемым в unit-тесте без сети и без TUI.
package agentloop

import (
	"strings"

	"qeuro/internal/tools"
)

// Limits — границы одного хода. Значения приходят от вызывающего, чтобы TUI и
// headless не держали каждый свою константу.
type Limits struct {
	MaxToolSteps  int // сколько провайдерских вызовов допустимо в одном ходе
	WarnAtRemain  int // за сколько шагов до лимита предупредить модель
	BudgetCredits float64
}

// Decision — что цикл делает дальше. Ровно один вариант, а не набор флагов:
// «продолжить и остановиться» — не состояние, а баг.
type Decision int

const (
	// DecisionContinue — отдать результаты модели и ждать следующий ход.
	DecisionContinue Decision = iota
	// DecisionRunTools — исполнить очередь tool call'ов.
	DecisionRunTools
	// DecisionAskApproval — ждать решения человека или хоста.
	DecisionAskApproval
	// DecisionVerify — код изменён, зелёной проверки нет: вернуть модель в цикл.
	DecisionVerify
	// DecisionStopToolLimit — шаги исчерпаны: просить финал без тулов.
	DecisionStopToolLimit
	// DecisionStopBudget — потолок кредитов достигнут: ход прекращён без
	// дополнительного вызова модели.
	DecisionStopBudget
	// DecisionStopVerifyLimit — шаги исчерпаны, а проверка так и не прошла: ход
	// прекращён ошибкой. Отдельное решение, а не StopToolLimit, потому что
	// просить финал здесь нельзя — gate снимается только успешной проверкой, и
	// просьба вернула бы цикл в то же состояние на следующем витке.
	DecisionStopVerifyLimit
	// DecisionFinish — финальный ответ готов.
	DecisionFinish
)

func (d Decision) String() string {
	switch d {
	case DecisionContinue:
		return "continue"
	case DecisionRunTools:
		return "run_tools"
	case DecisionAskApproval:
		return "ask_approval"
	case DecisionVerify:
		return "verify"
	case DecisionStopToolLimit:
		return "stop_tool_limit"
	case DecisionStopBudget:
		return "stop_budget"
	case DecisionStopVerifyLimit:
		return "stop_verify_limit"
	case DecisionFinish:
		return "finish"
	default:
		return "unknown"
	}
}

// ApprovalMode — политика подтверждений для мутирующих тулов.
type ApprovalMode int

const (
	// ApprovalAsk — спрашивать всегда.
	ApprovalAsk ApprovalMode = iota
	// ApprovalAuto — правки файлов внутри корня проекта идут без вопроса.
	// На run_command и MCP не распространяется никогда.
	ApprovalAuto
)

// Gate — состояние gate верификации: код изменён, значит ход не может
// закончиться, пока сборка/тесты/линт не прошли.
type Gate struct {
	Required bool
	Passed   bool
	Note     string
}

// State — состояние хода, которое влияет на решения. Держится вызывающим
// (моделью TUI или Engine) и передаётся сюда по значению: пакет ничего не
// помнит между вызовами, поэтому одно и то же состояние всегда даёт одно и то
// же решение.
type State struct {
	ToolStep     int
	SpentCredits float64
	Gate         Gate
	Warned       bool
}

// NeedsToolLimitWarning сообщает, пора ли предупредить модель, что шаги
// заканчиваются. Предупреждение одноразовое: повторять его каждый шаг — значит
// тратить контекст на текст, который модель уже видела.
func (l Limits) NeedsToolLimitWarning(s State) bool {
	if s.Warned || l.WarnAtRemain <= 0 {
		return false
	}
	return l.MaxToolSteps-s.ToolStep <= l.WarnAtRemain
}

// BudgetExhausted — потолок достигнут. Сравнение нестрогое: бюджет в 10
// кредитов, разрешающий вызов ровно на 10 потраченных, — это бюджет больше 10.
func (l Limits) BudgetExhausted(s State) bool {
	return l.BudgetCredits > 0 && s.SpentCredits >= l.BudgetCredits
}

// StepsExceeded — шаг, который вызывающий собирается сделать, выходит за лимит.
func (l Limits) StepsExceeded(s State) bool {
	return l.MaxToolSteps > 0 && s.ToolStep > l.MaxToolSteps
}

// NextAfterStream решает, что делать после закрытия стрима.
//
// Порядок проверок — не стилистика. Бюджет проверяется раньше лимита шагов и
// раньше gate, потому что и то и другое продолжает ход новым платным вызовом:
// потолок, который пропускает «ещё один вызов, чтобы красиво закончить», — это
// не потолок. Gate проверяется после tool call'ов, потому что модель, которая в
// том же ходе просит запустить тесты, должна получить эту возможность, а не
// упрёк за её отсутствие.
func NextAfterStream(l Limits, s State, pendingCalls int, streamErr string) Decision {
	if streamErr != "" {
		return DecisionFinish
	}
	if pendingCalls > 0 {
		if l.BudgetExhausted(s) {
			return DecisionStopBudget
		}
		if l.StepsExceeded(State{ToolStep: s.ToolStep + 1}) {
			return DecisionStopToolLimit
		}
		return DecisionRunTools
	}
	if s.Gate.Required {
		if l.BudgetExhausted(s) {
			return DecisionStopBudget
		}
		// Исчерпанные шаги на открытом gate — это отказ, а не просьба закончить
		// красиво. Просьба ничего не закрывает: gate снимает только успешная
		// проверка, поэтому «попроси финал» вернуло бы цикл сюда же на следующем
		// витке, и ход не закончился бы никогда — а каждый виток дописывает
		// историю и делает платный вызов. Так же поступает TUI
		// (enforceVerificationGate): ход останавливается с ошибкой.
		if l.StepsExceeded(State{ToolStep: s.ToolStep + 1}) {
			return DecisionStopVerifyLimit
		}
		return DecisionVerify
	}
	return DecisionFinish
}

// CallVerdict — что делать с одним запрошенным tool call'ом до его исполнения.
type CallVerdict struct {
	// Blocked непусто, если вызов отклонён без исполнения; строка идёт модели
	// как результат вызова.
	Blocked string
	// NeedsApproval — требуется решение человека или хоста.
	NeedsApproval bool
}

// ScreenCall решает судьбу одного вызова до исполнения.
//
// Имя, которого нет в реестре, отклоняется первым: неизвестный тул нельзя ни
// исполнить, ни осмысленно показать в предпросмотре, а reestr включает и
// MCP-тулы, которые пользователь не разрешал.
//
// run_command и любой MCP-тул спрашивают всегда, независимо от режима: авто-
// одобрение покрывает правки файлов внутри корня проекта, а запуск шелла и
// передача аргументов в сторонний процесс — не это (roadmap.txt:333).
func ScreenCall(name, argsJSON string, mode ApprovalMode) CallVerdict {
	if !tools.Known(name) {
		return CallVerdict{Blocked: "blocked: no tool named " + name + " is available in this session"}
	}
	if !tools.RequiresApproval(name) {
		return CallVerdict{}
	}
	if name == tools.ToolRunCommand {
		if reason := tools.ScreenCommand(commandArg(argsJSON)); reason != "" {
			return CallVerdict{Blocked: "blocked: " + reason}
		}
	}
	if name == tools.ToolRunCommand || tools.IsMCPName(name) || mode == ApprovalAsk {
		return CallVerdict{NeedsApproval: true}
	}
	return CallVerdict{}
}

// NoteGate обновляет gate верификации по результату одного вызова.
//
// Мутация кода поднимает требование и сбрасывает пройденность: зелёная сборка
// до правки ничего не говорит о состоянии после неё.
func NoteGate(g Gate, name, argsJSON, result string, mutated, ran bool) Gate {
	if mutated && ran {
		return Gate{
			Required: true,
			Passed:   false,
			Note:     "latest code change: " + tools.Summary(name, argsJSON),
		}
	}
	if name != tools.ToolRunCommand || !g.Required {
		return g
	}

	cmd := commandArg(argsJSON)
	switch {
	case !ran:
		g.Note = "verification command was rejected"
	case !IsVerificationCommand(cmd):
		g.Note = "last command was not a build/test/lint/typecheck: " + cmd
	case CommandSucceeded(result):
		g.Required = false
		g.Passed = true
		g.Note = "verification passed: " + cmd
	default:
		g.Note = "verification failed: " + FirstResultLine(result)
	}
	return g
}

// CommandSucceeded читает префикс, который Runner ставит успешной команде.
func CommandSucceeded(result string) bool {
	return strings.HasPrefix(strings.TrimSpace(result), tools.CommandOKPrefix)
}

// IsVerificationCommand сообщает, похожа ли команда на настоящую
// сборку/тесты/линт/тайпчек.
//
// Каждый сегмент конвейера рассматривается отдельно, а печатающие команды
// пропускаются: иначе `echo build ok` закрывал бы gate, не запустив ничего, —
// то есть модель могла бы объявить проверку пройденной одной строкой вывода.
func IsVerificationCommand(command string) bool {
	low := strings.ToLower(command)
	for _, sep := range []string{"&&", "||", ";", "|"} {
		low = strings.ReplaceAll(low, sep, "\n")
	}
	for _, segment := range strings.Split(low, "\n") {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "echo", "printf", "true", "cat", "type", "rem":
			continue
		}
		seg := strings.Join(fields, " ")
		for _, marker := range []string{
			"test", "lint", "build", "check", "typecheck", "tsc", "eslint",
			"vitest", "jest", "playwright", "pytest", "ruff", "mypy",
			"go vet", "go test", "go build", "cargo", "dotnet test",
		} {
			if strings.Contains(seg, marker) {
				return true
			}
		}
	}
	return false
}

// FirstResultLine — первая строка результата, обрезанная до разумной длины.
func FirstResultLine(result string) string {
	result = strings.TrimSpace(result)
	if result == "" {
		return "(empty output)"
	}
	if i := strings.IndexByte(result, '\n'); i >= 0 {
		result = result[:i]
	}
	if len(result) > 180 {
		result = result[:180] + "..."
	}
	return result
}
