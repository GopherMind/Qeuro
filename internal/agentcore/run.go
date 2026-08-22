package agentcore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/skills"
	"qeuro/internal/team"
	"qeuro/internal/tools"
)

const headlessUsage = `использование: qeuro run --headless --jsonl [--model m] [--budget c] [--parallel N] -- "<prompt>"

При --parallel N > 1 каждый пишущий агент работает в отдельном рабочем дереве, а его
правки применяются к проекту после шага, в порядке плана (roadmap §4.1). Если один и
тот же файл изменили два агента, выводится путь и не применяется ничего — вместо
молчаливой потери одной из правок. Команды в параллельном шаге недоступны: их эффекты
нельзя удержать внутри одного дерева, поэтому сборку и тесты запускает тестер после.`

const maxHeadlessWorkers = 16

// parseHeadlessArgs разбирает argv команды run.
//
// Неизвестный флаг — ошибка, а не позиционный аргумент. Значение --parallel
// ограничено безопасным верхним пределом: оно управляет числом одновременно
// работающих агентов и поэтому не должно превращаться в неограниченный spawn.
func parseHeadlessArgs(args []string) (headlessArgs, error) {
	out := headlessArgs{}
	seenPrompt := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			// Всё после `--` — задача целиком: так её текст не может быть
			// перепутан с флагом, даже если начинается с дефиса.
			rest := args[i+1:]
			if len(rest) == 0 {
				return out, errors.New("после -- нет prompt")
			}
			out.prompt = strings.Join(rest, " ")
			return out, nil
		case a == "--headless":
			// headless — единственный режим этого входа
		case a == "--jsonl":
			out.jsonl = true
		case a == "--model":
			if i+1 >= len(args) {
				return out, errors.New("--model требует значение")
			}
			i++
			out.model = args[i]
		case a == "--budget":
			if i+1 >= len(args) {
				return out, errors.New("--budget требует значение")
			}
			i++
			out.budget = args[i]
		case a == "--parallel":
			if i+1 >= len(args) {
				return out, errors.New("--parallel requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > maxHeadlessWorkers {
				return out, fmt.Errorf("--parallel must be an integer from 1 to %d", maxHeadlessWorkers)
			}
			out.parallel = n
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf("неизвестный флаг: %s", a)
		default:
			if seenPrompt {
				return out, errors.New("несколько prompt: используйте -- \"<prompt>\"")
			}
			out.prompt, seenPrompt = a, true
		}
	}
	return out, nil
}

// headlessArgs — разобранный argv. Значения флагов остаются строками: они уходят
// во флаговый слой clientcfg, который их и валидирует, чтобы `config doctor`
// показывал победивший слой, а не значение, разобранное дважды по-разному.
type headlessArgs struct {
	model    string
	budget   string
	parallel int
	prompt   string
	jsonl    bool
}

// RunHeadless — реализация `qeuro run --headless --jsonl [--model m] "<prompt>"`.
// Зарегистрирована в реестре команд `main.go`.
func RunHeadless(args []string) int {
	parsed, err := parseHeadlessArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, headlessUsage)
		return 2
	}
	if parsed.prompt == "" || !parsed.jsonl {
		fmt.Fprintln(os.Stderr, headlessUsage)
		return 2
	}

	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	emit := NewEmitter(os.Stdout, runID)
	approvals, cancel := ReadHostCommands(os.Stdin)

	flags := map[string]string{}
	if parsed.model != "" {
		flags["model"] = parsed.model
	}
	if parsed.budget != "" {
		flags["budget"] = parsed.budget
	}
	cfg, cfgErr := clientcfg.LoadWithFlags(flags)
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "config warning:", cfgErr)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "config warning:", w)
	}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot determine working directory:", err)
		return 1
	}
	runner, err := tools.NewRunner(wd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start tool runner:", err)
		return 1
	}
	provider := cfg.Provider()
	if parsed.parallel > 0 {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		go func() {
			select {
			case <-cancel:
				stop()
			case <-ctx.Done():
			}
		}()
		return runParallelHeadless(ctx, emit, provider, runner, cfg.Model, cfg.Budget, parsed.parallel, parsed.prompt, cfg.UnsafeParallelWrites)
	}

	eng := &Engine{
		Emit:      emit,
		Approvals: approvals,
		Cancel:    cancel,
		Deps: Deps{
			Provider: provider,
			Runner:   runner,
		},
		Opts: Options{
			Model:         cfg.Model,
			WorkDir:       wd,
			AutoApprove:   cfg.AutoApprove,
			BudgetCredits: cfg.Budget,
		},
	}
	if err := eng.Run(context.Background(), parsed.prompt); err != nil {
		return 1
	}
	return 0
}

// runParallelHeadless adapts the existing team pipeline to the headless Agent
// Protocol. It emits progress as status events and one terminal assistant/done
// pair, keeping stdout valid JSONL for cloud-worker consumption.
func runParallelHeadless(ctx context.Context, emit EventEmitter, provider client.Provider, runner *tools.Runner, model string, budget float64, workers int, prompt string, unsafeWrites bool) int {
	lib, err := skills.Load()
	if err != nil {
		_ = emit.Emit(Event{Kind: KindError, Text: "skills unavailable: " + err.Error(), Code: "skills_unavailable"})
		_ = emit.Emit(Event{Kind: KindDone, Status: DoneError})
		return 1
	}
	profile := team.ProfileForTier("")
	profile.MaxWorkers = workers
	teamEngine := team.NewWithBudget(provider, runner, lib, profile, func(ev team.Event) {
		text := ev.Text
		if ev.Role != "" {
			text = ev.Role + ": " + text
		}
		_ = emit.Emit(Event{Kind: KindStatus, Name: "team", Text: text})
	}, nil, budget)
	// roadmap-v3 §4.1 rollout flag. Off by default, so `--parallel N` with N > 1
	// runs read-only: the workers share one tree and concurrent writes to it lose
	// edits without reporting anything.
	teamEngine.AllowUnsafeParallelWrites(unsafeWrites)
	summary, err := teamEngine.Run(ctx, prompt)
	if err != nil {
		_ = emit.Emit(Event{Kind: KindError, Text: err.Error(), Code: "team_failed"})
		_ = emit.Emit(Event{Kind: KindDone, Status: DoneError})
		return 1
	}
	if summary != "" {
		_ = emit.Emit(Event{Kind: KindAssistant, Text: summary, Model: model})
	}
	_ = emit.Emit(Event{Kind: KindDone, Status: DoneOK})
	return 0
}
