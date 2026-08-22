package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"qeuro/internal/agentloop"
	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/tools"
)

// Options задают один запуск агента.
type Options struct {
	Model       string // "" или "auto" — довериться роутеру (catalog)
	WorkDir     string
	AutoApprove bool // облако/автономный режим: не ждать approval_response
	// BudgetCredits — жёсткий потолок расхода на запуск. 0 — без потолка, как и
	// в TUI: потолок, о котором не просили, останавливал бы работу на середине.
	BudgetCredits float64
	// MaxToolSteps — 0 берёт значение по умолчанию (DefaultMaxToolSteps).
	MaxToolSteps int
}

// Значения по умолчанию совпадают с TUI: headless, который сдаётся раньше
// терминала, — это другой агент под тем же именем.
const (
	DefaultMaxToolSteps = 64
	toolLimitWarnSteps  = 8
	maxTokensPerTurn    = 4096
)

// Deps — зависимости ядра. Пакеты уже свободны от Bubble Tea и передаются как
// есть; интерфейс объявлен минимальным, чтобы тест мог подставить фейк, не
// поднимая ни сети, ни файловой системы.
type Deps struct {
	// Provider — источник инференса: backend-прокси или локальная модель. Обе
	// реализации возвращают один и тот же <-chan client.Event, поэтому цикл не
	// ветвится по режиму.
	Provider client.Provider
	// Runner исполняет тулы. nil означает «тулов нет»: вызов будет отклонён с
	// сообщением модели, а не паникой.
	Runner ToolRunner
}

// ToolRunner — то, что цикл требует от исполнителя тулов. Это подмножество
// *tools.Runner, объявленное здесь, чтобы тесты цикла не создавали настоящий
// runner с реальным корнем на диске.
type ToolRunner interface {
	Execute(name, argsJSON string) (result string, mutated bool)
}

var ErrCancelled = errors.New("run cancelled")

// Emitter — интерфейс для отправки событий. Это позволяет TUI использовать
// ChannelEmitter вместо JSONL Emitter без изменения Engine.
type EventEmitter interface {
	Emit(Event) error
}

// Engine — синхронный цикл агента, переписанный с tea.Cmd-событий на каналы.
type Engine struct {
	Emit      EventEmitter
	Approvals <-chan HostCommand
	Cancel    <-chan struct{}
	Deps      Deps
	Opts      Options
}

// RequestApproval блокирует run до решения хоста (или AutoApprove).
// Сюда сходятся все точки, где TUI ждал кнопку подтверждения
// (RequiresApproval / Preview / Mutating из tools.Runner).
//
// Событие approval_request эмитится ВСЕГДА, в том числе при AutoApprove.
// Хост уже рассчитывает на это: cloud-worker/execute.go рендерит такое событие
// как «Approval auto-granted in the isolated runner: <action>» и кладёт строку
// в таймлайн запуска и в чек пользователя. Если пропускать эмит под
// AutoApprove, то автономный режим — единственный, где никто не смотрит на
// экран, — оказался бы наименее аудируемым. Это ровно наоборот.
//
// Роадмап 5.3 требует, чтобы AutoApprove действовал только для тулов из белого
// списка, а сетевые, mutating и push-операции всегда требовали явного апрува.
// Единственный владелец этого списка — autoApprovableAction ниже.
func (e *Engine) RequestApproval(ctx context.Context, id, action, preview string) (bool, error) {
	if err := e.Emit.Emit(Event{Kind: KindApprovalRequest, ID: id, Action: action, Preview: preview}); err != nil {
		return false, err
	}
	if e.Opts.AutoApprove {
		return autoApprovableAction(action), nil
	}
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-e.Cancel:
			return false, ErrCancelled
		case c := <-e.Approvals:
			if c.ID == id {
				return c.Decision == "approve", nil
			}
		}
	}
}

// autoApprovableAction is deliberately a tiny capability allow-list. Build,
// test, package, VCS mutation, file mutation and command actions can execute
// repository-controlled code and therefore never inherit autonomous approval.
func autoApprovableAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "read_file", "list_dir", "search_code":
		return true
	default:
		return false
	}
}

// runState — состояние одного запуска. Это те поля модели TUI, которые
// участвуют в решениях цикла, без всего, что относилось к отрисовке.
type runState struct {
	history   []client.Message
	work      agentloop.WorkingState
	loop      agentloop.State
	streamRaw string // текст ответа модели как он пришёл, без разметки
	meta      string // модель и effort из route-события
	streamErr string
	pending   []client.ToolCall
	toolFinal bool // финал без тулов: следующий запрос уходит без определений
}

// Run — главный цикл: план → инструменты → верификация → ответ.
//
// Решения (лимит шагов, потолок бюджета, gate верификации, скрининг вызова)
// приняты в internal/agentloop и общие с TUI. Здесь остаётся то, что headless
// делает по-своему: ожидание событий стрима, исполнение тулов и перевод каждого
// шага в JSONL. Так добавить правило в цикл можно один раз, а не дважды с
// риском, что терминал и облако разойдутся именно на правилах про деньги и
// запись в файлы.
//
// Завершение всегда одно событие done (ok | cancelled | error) — хост
// рассчитывает на него, чтобы закрыть запуск.
func (e *Engine) Run(ctx context.Context, prompt string) (err error) {
	if e.Emit == nil {
		return ErrEmitterMissing
	}
	terminal := newTerminalEmitter(e.Emit)
	run := *e
	run.Emit = terminal

	defer func() {
		if recover() != nil {
			errorErr := terminal.Emit(Event{
				Kind: KindError,
				Text: "agent engine stopped after an internal panic",
				Code: "engine_panic",
			})
			doneErr := terminal.Emit(Event{Kind: KindDone, Status: DoneError})
			err = errors.Join(ErrEnginePanic, errorErr, doneErr)
			return
		}
		if !terminal.terminalSent() {
			doneErr := terminal.Emit(Event{Kind: KindDone, Status: DoneError})
			if err == nil {
				err = ErrTerminalMissing
			}
			err = errors.Join(err, doneErr)
		}
	}()

	return run.run(ctx, prompt)
}

func (e *Engine) run(ctx context.Context, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return e.fail("empty prompt", "bad_request")
	}
	if e.Deps.Provider == nil {
		// Не паника: отсутствие провайдера — это конфигурация, а хост обязан
		// получить done, иначе он будет ждать запуск, которого нет.
		return e.fail("no inference provider configured", "no_provider")
	}

	// Отмена хоста переводится в отмену контекста, чтобы её увидели и стрим, и
	// исполнение тулов, а не только точки ожидания approval.
	ctx, cancel := e.withHostCancel(ctx)
	defer cancel()

	limits := e.limits()
	st := &runState{
		history: []client.Message{
			{Role: "system", Content: agentloop.SystemPrompt},
			{Role: "system", Content: agentloop.ShellPrompt},
			{Role: "user", Content: prompt},
		},
	}

	for {
		if err := ctx.Err(); err != nil {
			return e.cancelled()
		}

		if err := e.stream(ctx, st); err != nil {
			if errors.Is(err, context.Canceled) {
				return e.cancelled()
			}
			return e.fail(err.Error(), "stream_failed")
		}

		// Финальный проход завершает ход всегда и без исключений.
		//
		// Это страховка от незавершаемого цикла, а не оптимизация. Каждый виток
		// дописывает историю двумя сообщениями и делает платный вызов, поэтому
		// «ещё один шанс закончить красиво» — это рост памяти без границы и счёт,
		// который никто не остановит: в контейнере cloud-worker процесс умирает
		// по OOM, успев потратить кредиты на вызовы, результат которых уже некому
		// отдать. Ни лимит шагов, ни gate верификации не могут отменить этот
		// выход: оба они и приводят сюда.
		if st.toolFinal {
			return e.finish(st)
		}

		switch agentloop.NextAfterStream(limits, st.loop, len(st.pending), st.streamErr) {
		case agentloop.DecisionStopBudget:
			e.emitText("budget", agentloop.BudgetStopMessage)
			return e.done(DoneError)

		case agentloop.DecisionStopVerifyLimit:
			// Код изменён, проверка не прошла, шаги кончились. Это ошибка запуска:
			// хост не должен принять непроверенную правку за успешную работу.
			return e.fail(agentloop.VerifyLimitMessage, "verification_failed")

		case agentloop.DecisionStopToolLimit:
			// Лимит шагов просит у модели финал без тулов — один раз. Если и он
			// приходит с вызовами, они отбрасываются: иначе «финал без тулов» стал
			// бы циклом, который лимит и должен был закончить.
			st.loop.ToolStep++
			st.history = append(st.history, assistantTurn(st))
			st.history = append(st.history, client.Message{
				Role:    "user",
				Content: agentloop.ToolLimitFinal("tool loop reached its step limit"),
			})
			st.pending = nil
			st.toolFinal = true
			e.emitText("limit", "tool loop limit reached; asking model for final no-tools status")
			continue

		case agentloop.DecisionRunTools:
			if err := e.runTools(ctx, st, limits); err != nil {
				if errors.Is(err, ErrCancelled) || errors.Is(err, context.Canceled) {
					return e.cancelled()
				}
				return e.fail(err.Error(), "tool_failed")
			}
			continue

		case agentloop.DecisionVerify:
			st.loop.ToolStep++
			if text := strings.TrimSpace(st.streamRaw); text != "" {
				st.history = append(st.history, client.Message{Role: "assistant", Content: text})
			}
			st.history = append(st.history, client.Message{
				Role:    "user",
				Content: agentloop.GateMessage(st.loop.Gate.Note),
			})
			st.streamRaw, st.streamErr = "", ""
			e.emitText("quality gate", "a passing check is required after changes: test/build/lint/typecheck")
			continue

		case agentloop.DecisionFinish:
			return e.finish(st)
		}
	}
}

// limits собирает границы хода из опций, подставляя значения TUI по умолчанию.
func (e *Engine) limits() agentloop.Limits {
	steps := e.Opts.MaxToolSteps
	if steps <= 0 {
		steps = DefaultMaxToolSteps
	}
	return agentloop.Limits{
		MaxToolSteps:  steps,
		WarnAtRemain:  toolLimitWarnSteps,
		BudgetCredits: e.Opts.BudgetCredits,
	}
}

// withHostCancel связывает канал Cancel с контекстом запуска.
func (e *Engine) withHostCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if e.Cancel == nil {
		return ctx, cancel
	}
	// A command may already be queued before Run starts. Relying only on the
	// goroutine below races a fast provider: it can produce done/ok before the
	// scheduler observes the closed channel. Consume that state synchronously so
	// cancellation determines the terminal status deterministically.
	select {
	case <-e.Cancel:
		cancel()
		return ctx, cancel
	default:
	}
	go func() {
		select {
		case <-e.Cancel:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// stream проводит один запрос к модели и собирает его события.
func (e *Engine) stream(ctx context.Context, st *runState) error {
	st.streamRaw, st.streamErr = "", ""
	st.pending = nil

	ch, err := e.Deps.Provider.Chat(ctx, e.request(st))
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			e.applyEvent(st, ev)
		}
	}
}

// applyEvent переводит одно событие провайдера в состояние и в JSONL.
func (e *Engine) applyEvent(st *runState, ev client.Event) {
	switch ev.Kind {
	case client.EventRoute:
		if ev.Route != nil {
			// Новый маршрут (эскалация) отменяет уже накопленный текст: он был
			// ответом другой модели, и склеивать два ответа в один нельзя.
			st.streamRaw = ""
			st.meta = ev.Route.Model + " · " + ev.Route.Effort
			_ = e.Emit.Emit(Event{Kind: KindRoute, Model: ev.Route.Model, Text: ev.Route.Reason})
		}
	case client.EventToken:
		st.streamRaw += ev.Text
		_ = e.Emit.Emit(Event{Kind: KindToken, Text: ev.Text})
	case client.EventToolCalls:
		if st.toolFinal {
			// Финальный запрос уходит без определений тулов, поэтому вызовы в
			// ответ на него — поведение провайдера, а не решение цикла. Принять
			// их значило бы снова упереться в лимит и снова попросить финал:
			// «последний ход» стал бы бесконечным платным циклом с провайдером,
			// который игнорирует отсутствие тулов.
			e.emitText("limit", "ignoring tool calls in the final no-tools pass")
			return
		}
		st.pending = ev.ToolCalls
	case client.EventUsage:
		if ev.Usage != nil {
			// Потолок накапливается по чеку, а не по догадке: чек — единственное
			// место, где клиент узнаёт настоящую стоимость вызова, и именно поэтому
			// расход между шагами цикла известен точно.
			if ev.Usage.Credits > 0 {
				st.loop.SpentCredits += ev.Usage.Credits
			}
			_ = e.Emit.Emit(Event{
				Kind:      KindUsage,
				Tokens:    ev.Usage.In + ev.Usage.Out,
				TokensIn:  ev.Usage.In,
				TokensOut: ev.Usage.Out,
				CostUSD:   ev.Usage.CostUSD,
				Model:     st.modelName(),
			})
		}
	case client.EventError:
		// Текст ошибки контролируется сервером и уходит в терминал хоста, поэтому
		// проходит через DisplaySafeBlock (.ai/SECURITY.md:33).
		st.streamErr = clientcfg.DisplaySafeBlock(ev.ErrMsg)
	}
}

// request собирает запрос к модели. Порядок сообщений стабилен внутри хода,
// чтобы кэш промптов провайдера попадал на продолжениях цикла.
func (e *Engine) request(st *runState) client.ChatRequest {
	msgs := append([]client.Message(nil), st.history...)
	if state := st.work.Message(st.loop.Gate); state != "" {
		msgs = append(msgs, client.Message{Role: "system", Content: state})
	}

	req := client.ChatRequest{
		Mode:      "agent",
		Messages:  msgs,
		MaxTokens: maxTokensPerTurn,
		Model:     e.Opts.Model,
	}
	if !st.toolFinal {
		req.Tools = tools.Definitions()
	}
	return req
}

// runTools исполняет очередь вызовов одного шага и возвращает результаты модели.
func (e *Engine) runTools(ctx context.Context, st *runState, limits agentloop.Limits) error {
	st.loop.ToolStep++
	calls := append([]client.ToolCall(nil), st.pending...)
	st.history = append(st.history, client.Message{
		Role:      "assistant",
		Content:   st.streamRaw,
		ToolCalls: calls,
	})
	st.pending = nil

	if limits.NeedsToolLimitWarning(st.loop) {
		st.loop.Warned = true
		st.history = append(st.history, client.Message{
			Role:    "user",
			Content: agentloop.ToolLimitWarning(limits.MaxToolSteps - st.loop.ToolStep),
		})
	}

	for _, c := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, mutated, ran, err := e.execCall(ctx, c)
		if err != nil {
			return err
		}
		e.recordCall(st, c, result, mutated, ran)
	}
	return nil
}

// execCall решает судьбу одного вызова и исполняет его, если решено.
func (e *Engine) execCall(ctx context.Context, c client.ToolCall) (result string, mutated, ran bool, err error) {
	name := c.Function.Name
	args := c.Function.Arguments

	// Режим подтверждений здесь всегда «спрашивать»: RequestApproval сам решает,
	// отвечать ли автоматически, и разрешает только чтение. Передать сюда
	// ApprovalAuto значило бы дать автономному запуску молча править файлы.
	verdict := agentloop.ScreenCall(name, args, agentloop.ApprovalAsk)
	if verdict.Blocked != "" {
		// Имя тула приходит от модели и попадает в терминал хоста.
		return clientcfg.DisplaySafe(verdict.Blocked), false, false, nil
	}
	if verdict.NeedsApproval {
		ok, err := e.RequestApproval(ctx, c.ID, name, tools.Preview(name, args))
		if err != nil {
			return "", false, false, err
		}
		if !ok {
			return "rejected: not approved", false, false, nil
		}
	}

	if e.Deps.Runner == nil {
		return "error: tool runner is not available", false, false, nil
	}
	result, mutated = e.Deps.Runner.Execute(name, args)
	return result, mutated, true, nil
}

// recordCall кладёт результат вызова в историю, в сводку, в gate и в JSONL.
func (e *Engine) recordCall(st *runState, c client.ToolCall, result string, mutated, ran bool) {
	name := c.Function.Name
	args := c.Function.Arguments

	st.history = append(st.history, client.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: c.ID,
		Name:       name,
	})
	st.work.Note(agentloop.SummarizeStep(name, args, result, mutated, ran))
	st.loop.Gate = agentloop.NoteGate(st.loop.Gate, name, args, result, mutated, ran)
	_ = e.Emit.Emit(stepEvent(c, result, mutated, ran))
}

// stepEvent переводит исполненный вызов в событие протокола.
//
// Вид события выбирается по contract'у `qeuro/domain/agentproto`, а не по
// удобству цикла: хосты разбирают именно эти kind'ы. Десктоп кладёт в панель
// Changes только `file_write` (и берёт из него path/diff/before/after для
// Monaco и «Откатить»), а cloud-worker рендерит `file_write` как «Edited …» и
// `command` как «Ran … (exit N)». Своё имя вроде `tool_result` оба обработали
// бы default-веткой: правка файла осталась бы строкой лога, то есть Accept и
// Revert не появились бы там, где агент действительно изменил файл.
func stepEvent(c client.ToolCall, result string, mutated, ran bool) Event {
	name := c.Function.Name
	args := c.Function.Arguments
	ev := Event{
		Kind: KindToolCall,
		Name: name,
		ID:   c.ID,
		Text: agentloop.FirstResultLine(result),
	}

	switch {
	case name == tools.ToolRunCommand:
		ev.Kind = KindCommand
		// Отклонённая политикой команда не показывается как исполненная: пустой
		// Cmd лучше строки, которую никто не запускал.
		ev.Cmd = tools.SanitizedCommandLine(commandOf(args))
		if ran {
			if code, ok := exitCodeOf(result); ok {
				ev.ExitCode = &code
			}
			// Для команды Text — это её собственный вывод целиком, а не первая
			// строка: roadmap-v3 §5.1 требует «ограниченный литеральный вывод» в
			// панели Evidence, и одна строка статуса им не является — падение теста
			// печатается ниже неё. Ограничение — в commandOutputOf.
			//
			// Только для выполненной команды: у отклонённой вывода нет, и первая
			// строка результата там — это причина отказа, которой место в Text как
			// есть.
			if out := commandOutputOf(result); out != "" {
				ev.Text = escapeCommandOutput(out)
			}
		}
	case mutated && ran:
		ev.Kind = KindFileWrite
		ev.Path = pathOf(args)
		before, after := contentSidesOf(name, args)
		ev.Diff = tools.Preview(name, args)
		// before/after — опциональные поля v1.1, и они отдаются только когда
		// известны точно: у patch_file это фрагменты, которые модель прислала
		// сама, у write_file прежнего содержимого нет (тул отказывается
		// перезаписывать существующий файл). Догадка здесь означала бы «Откатить»,
		// возвращающий не то, что было.
		if before != nil {
			ev.Before = before
		}
		if after != nil {
			ev.After = after
		}
	}
	return ev
}

// contentSidesOf достаёт стороны правки из аргументов вызова.
func contentSidesOf(name, argsJSON string) (before, after *string) {
	var a struct {
		OldContent *string `json:"old_content"`
		NewContent *string `json:"new_content"`
		Content    *string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, nil
	}
	switch name {
	case tools.ToolPatchFile:
		return a.OldContent, a.NewContent
	case tools.ToolWriteFile:
		// Новый файл: «до» — пустота, и это факт, а не догадка.
		empty := ""
		return &empty, a.Content
	}
	return nil, nil
}

// maxCommandEvidence ограничивает литеральный вывод команды в событии протокола.
//
// Хост облака отдаёт этот текст в панель Evidence и там же режет его до 4000
// рун; предел здесь тот же, чтобы длинная сборка не раздувала JSONL-строку,
// которую всё равно обрежут. Сам тул уже ограничивает вывод 16 KiB.
const maxCommandEvidence = 4000

// commandOutputOf достаёт литеральный вывод команды из результата run_command,
// то есть всё после разделителя. Возвращает "" когда разделителя нет — так
// выглядит результат, который сформировал не Runner (отказ политики, ошибка
// аргументов), и выдавать причину отказа за вывод команды нельзя.
//
// Разделитель ищется как первая строка, равная константе: команда может напечатать
// такую же строку сама, и strings.Index нашёл бы её вывод вместо разделителя.
func commandOutputOf(result string) string {
	rest := result
	for {
		line, tail, found := strings.Cut(rest, "\n")
		if strings.TrimRight(line, "\r") == tools.CommandOutputSeparator {
			out := strings.TrimRight(tail, "\n\r")
			if len([]rune(out)) > maxCommandEvidence {
				out = string([]rune(out)[:maxCommandEvidence])
			}
			return out
		}
		if !found {
			return ""
		}
		rest = tail
	}
}

// escapeCommandOutput обезвреживает вывод команды перед отправкой в протокол.
//
// Раньше в Text попадала только первая строка результата, и её экранировал уже
// сам хост. Теперь это весь вывод сборки — а потребители пишут его прямо в
// терминал (десктоп: TerminalPane пишет text в xterm) и в HTML-блок консоли.
// Управляющая последовательность в таком тексте — это не украшение: она может
// подвинуть курсор и перезаписать строки выше, то есть вывод, «доказывающий»
// успех теста, может быть напечатан выводом, который его лишь заявляет.
//
// DisplaySafeBlock сохраняет \n и \t (без них многострочный вывод перестаёт быть
// читаемым) и превращает остальные управляющие символы в \xNN — видимые, но
// безвредные.
func escapeCommandOutput(s string) string { return clientcfg.DisplaySafeBlock(s) }

// exitCodeOf выводит код возврата из результата run_command.
//
// Успех Runner помечает префиксом, поэтому 0 известен точно. Для провала код
// берётся только из текста самой ошибки exec («exit status N»); если его там
// нет, поле не проставляется — хост печатает «(exit N)», и выдуманное N там
// хуже отсутствующего.
func exitCodeOf(result string) (int, bool) {
	if agentloop.CommandSucceeded(result) {
		return 0, true
	}
	first := agentloop.FirstResultLine(result)
	const marker = "exit status "
	_, digits, ok := strings.Cut(first, marker)
	if !ok {
		return 0, false
	}
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(digits[:end])
	if err != nil {
		return 0, false
	}
	return code, true
}

// finish завершает запуск финальным ответом или ошибкой стрима.
func (e *Engine) finish(st *runState) error {
	if st.streamErr != "" {
		return e.fail(st.streamErr, "provider_error")
	}
	text := strings.TrimSpace(st.streamRaw)
	if text == "" {
		text = "(empty reply)"
	}
	st.history = append(st.history, client.Message{Role: "assistant", Content: st.streamRaw})
	if err := e.Emit.Emit(Event{Kind: KindAssistant, Text: text, Model: st.modelName()}); err != nil {
		return err
	}
	return e.done(DoneOK)
}

func (e *Engine) emitText(name, text string) {
	_ = e.Emit.Emit(Event{Kind: KindStatus, Name: name, Text: text})
}

func (e *Engine) fail(text, code string) error {
	_ = e.Emit.Emit(Event{Kind: KindError, Text: text, Code: code})
	return e.done(DoneError)
}

// cancelled — отмена не ошибка: хост отличает «остановлено по просьбе» от
// «сломалось», и запуск, помеченный error, попал бы в чек как сбой.
func (e *Engine) cancelled() error {
	return e.done(DoneCancelled)
}

func (e *Engine) done(status string) error {
	return e.Emit.Emit(Event{Kind: KindDone, Status: status})
}

func assistantTurn(st *runState) client.Message {
	return client.Message{Role: "assistant", Content: st.streamRaw}
}

func (st *runState) modelName() string {
	if i := strings.Index(st.meta, " · "); i > 0 {
		return st.meta[:i]
	}
	return st.meta
}

func commandOf(argsJSON string) string {
	return stringArg(argsJSON, "command")
}

func pathOf(argsJSON string) string {
	return stringArg(argsJSON, "path")
}

// stringArg достаёт строковое поле из аргументов вызова. Аргументы приходят от
// модели, поэтому невалидный JSON — ожидаемый вход, а не сбой.
func stringArg(argsJSON, field string) string {
	var generic map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &generic); err != nil {
		return ""
	}
	if s, ok := generic[field].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
