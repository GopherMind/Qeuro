// Package team implements the multi-agent "team" mode: a deterministic Go-driven
// pipeline that coordinates several model agents to solve one user task.
//
// Topology (the structure the user described):
//
//	planner  (strong model) — breaks the task into role-tagged subtasks + a test command
//	critic   (strong model) — critiques the plan a couple of times; planner revises
//	workers  (cheap models) — one per subtask, each primed with a matching skill,
//	                          writing real files via the local file tools
//	tester   (cheap model)  — runs the project's tests and reports pass/fail
//	lead     (strong model) — reviews results + tests, orders fixes, writes the final summary
//
// Every agent turn is a normal /v1/chat call, so the backend bills and routes
// each step exactly as it does a solo turn — no backend changes are required.
// All orchestration, including the fix loop, lives here.
package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/skills"
	"qeuro/internal/tools"
)

// EventKind classifies a progress event streamed to the UI.
type EventKind int

const (
	EvPhase      EventKind = iota // a new pipeline phase began
	EvAgentStart                  // an agent started working
	EvAgentEnd                    // an agent finished (Text = short note)
	EvTool                        // an agent ran a tool (Text = summary)
	EvInfo                        // misc info line
	EvError                       // a recoverable error
)

// Event is one progress update from a run.
type Event struct {
	Kind EventKind
	Role string // which agent (e.g. "planner", "backend")
	Text string
}

// Engine runs one team pipeline. It is created per /team invocation.
type Engine struct {
	cli  client.Provider
	run  *tools.Runner
	lib  *skills.Library
	prof Profile
	emit func(Event)
	ask  func(questions string) string // blocking: ask the user and return their reply

	// Credit tracking: updated from each step's usage event so the pipeline can
	// stop before running the balance negative (the backend's soft cap allows
	// overage, which a many-step team run would otherwise blow through).
	balanceMu    sync.Mutex
	balance      float64
	balanceKnown bool
	budget       float64
	spent        float64
	sessionID    string

	// sharedTree is true while a fan-out that could put two agents in the working
	// tree at the same time is in flight. While it is set, no agent may call a
	// tool that changes the tree (roadmap-v3 §4.1).
	//
	// The restriction exists because the engine has exactly one *tools.Runner (the
	// run field) and dispatch() runs its items concurrently, so "parallel" today
	// means several writers in ONE tree. The Runner's mutex makes that
	// memory-safe, not correct: patch_file reads the file before taking the lock
	// and writes under it, so two workers patching one file both succeed and the
	// first edit is silently gone. V2 §1.4 named this an anti-goal.
	//
	// It is a live flag rather than a value fixed at construction because the
	// condition is "two writers are actually sharing the tree", and that is not
	// knowable when the Engine is built: MaxWorkers is 5 or 8 in every profile, so
	// a constructor-time rule would make team mode read-only in every run —
	// including the interactive TUI, where a one-subtask plan writes perfectly
	// safely. Only dispatch() knows both halves (the cap and how many items there
	// are), so only dispatch() sets this. It is lifted by the per-writer worktree
	// isolation this restriction stands in for.
	sharedTree atomic.Bool

	// unsafeParallelWrites is the §0.3 rollout flag for the restriction above,
	// carrying the user's `unsafe_parallel_writes` setting. Off by default, which
	// means the restriction is in force.
	unsafeParallelWrites bool
}

// AllowUnsafeParallelWrites lifts the roadmap-v3 §4.1 read-only restriction on
// parallel steps. It is the rollout flag §0.3 requires and nothing else: the only
// intended caller passes clientcfg's `unsafe_parallel_writes`, which the user sets
// in the environment or a user-level file.
//
// A setter rather than a constructor parameter, and named for what it permits
// rather than for the flag: New/NewWithBudget already take six and seven
// arguments, and a seventh bare bool would be positionally adjacent to `budget`
// with no name at the call site. The verb also makes the grep that matters —
// "who turns concurrent writing on" — return the callers rather than every
// Engine construction.
//
// The engine still announces the mode when the flag is set, because a user who
// asked for concurrent writers a month ago in a shell profile has to be able to
// tell why an edit vanished (ledger §40.4: the loss is silent at the tool level).
func (e *Engine) AllowUnsafeParallelWrites(allow bool) { e.unsafeParallelWrites = allow }

// New builds an Engine. emit may be nil (events are then dropped). ask may be
// nil to disable the clarification phase (non-interactive runs).
// cli is a client.Provider rather than a *client.Client so a team run works in
// offline mode too (roadmap §8 row "Offline"): the engine only ever streams
// chat, which both the backend and a local model server do.
func New(cli client.Provider, runner *tools.Runner, lib *skills.Library, prof Profile, emit func(Event), ask func(string) string) *Engine {
	return NewWithBudget(cli, runner, lib, prof, emit, ask, 0)
}

// NewWithBudget adds a local ceiling for non-interactive callers. The backend's
// reported balance remains authoritative; this ceiling only prevents a team
// adapter from starting unbounded paid work when the caller supplied a budget.
func NewWithBudget(cli client.Provider, runner *tools.Runner, lib *skills.Library, prof Profile, emit func(Event), ask func(string) string, budget float64) *Engine {
	if emit == nil {
		emit = func(Event) {}
	}
	if budget < 0 {
		budget = 0
	}
	return &Engine{
		cli: cli, run: runner, lib: lib, prof: prof, emit: emit, ask: ask,
		budget:    budget,
		sessionID: "qeuro-team-" + projectSessionSeed(),
	}
}

func projectSessionSeed() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "default"
	}
	return shortHash(wd)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// Subtask is one unit of delegated work produced by the planner.
type Subtask struct {
	Role       string   `json:"role"`
	SkillQuery string   `json:"skill_query"`
	Task       string   `json:"task"`
	DependsOn  []string `json:"depends_on,omitempty"` // roles that must complete before this task starts

	skillName string // resolved skill (display)
	skillBody string // resolved skill instructions
}

// Plan is the planner's structured decomposition of the task.
type Plan struct {
	Summary     string    `json:"summary"`
	TestCommand string    `json:"test_command"`
	Subtasks    []Subtask `json:"subtasks"`
}

// workerResult records what one worker reported back.
type workerResult struct {
	role    string
	summary string
}

// per-agent step budgets — keep manager turns short, give workers room to build.
const (
	stepsPlanner = 8
	stepsCritic  = 4
	stepsWorker  = 24
	stepsTester  = 8
	stepsLead    = 5
)

// Run executes the full pipeline and returns the lead's final summary.
func (e *Engine) Run(ctx context.Context, task string) (string, error) {
	// 0) CLARIFY — if the task is underspecified and we can ask, gather details
	// from the user before committing the team to a direction.
	if e.ask != nil {
		if extra := e.clarify(ctx, task); extra != "" {
			task = task + "\n\nClarifications from the user:\n" + extra
		}
	}

	// 1) PLAN
	e.emit(Event{Kind: EvPhase, Text: "Planning"})
	plan, err := e.plan(ctx, task)
	if err != nil {
		return "", fmt.Errorf("planner: %w", err)
	}
	if len(plan.Subtasks) == 0 {
		plan.Subtasks = []Subtask{{Role: "generalist", SkillQuery: task, Task: task}}
	}

	// 2) CRITIQUE → REVISE (a couple of passes)
	for i := 0; i < e.prof.CritiqueRounds; i++ {
		e.emit(Event{Kind: EvPhase, Text: fmt.Sprintf("Plan critique (%d/%d)", i+1, e.prof.CritiqueRounds)})
		critique, ok := e.critiquePlan(ctx, task, plan)
		if ok {
			break // critic approved
		}
		if revised, err := e.replan(ctx, task, plan, critique); err == nil && len(revised.Subtasks) > 0 {
			plan = revised
		}
	}

	// Cap subtasks to the profile budget and resolve a skill for each.
	if len(plan.Subtasks) > e.prof.MaxWorkers {
		e.emit(Event{Kind: EvInfo, Text: fmt.Sprintf("limited to %d workers (profile %s)", e.prof.MaxWorkers, e.prof.Name)})
		plan.Subtasks = plan.Subtasks[:e.prof.MaxWorkers]
	}
	for i := range plan.Subtasks {
		e.resolveSkill(&plan.Subtasks[i])
	}

	// 3) WORKERS — build the first cut. Stop dispatching if credits run out.
	e.emit(Event{Kind: EvPhase, Text: "Execution"})
	results := make([]workerResult, 0, len(plan.Subtasks))

	var mu sync.Mutex
	earlyStop := dispatchWriters(e, ctx, plan.Subtasks,
		func(st Subtask) string { return st.Role },
		func(st Subtask, runner *tools.Runner) {
			out := e.runWorker(ctx, st, plan, "", runner)

			mu.Lock()
			results = append(results, workerResult{role: st.Role, summary: out})
			mu.Unlock()
		})

	if earlyStop {
		return e.localSummary(results, "Ran out of credits during execution."), nil
	}

	// 4) TEST → FIX loop. Skip when out of credits.
	var testOut string
	for round := 0; round <= e.prof.FixRounds; round++ {
		if e.depleted() {
			e.emitDepleted()
			return e.localSummary(results, "Ran out of credits before testing."), nil
		}
		e.emit(Event{Kind: EvPhase, Text: "Testing"})
		testOut = e.runTester(ctx, plan)

		if round == e.prof.FixRounds {
			break // no budget left to fix
		}
		fixes, ok := e.review(ctx, task, plan, results, testOut)
		if ok || len(fixes) == 0 {
			break // lead is satisfied
		}
		e.emit(Event{Kind: EvPhase, Text: fmt.Sprintf("Rework (%d/%d)", round+1, e.prof.FixRounds)})

		// Список правок приходит от модели, поэтому его длину нужно ограничить
		// так же, как len(plan.Subtasks) выше: иначе «лид», сошедший с ума или
		// подменённый, порождает по горутине на каждую строку своего ответа.
		if maxFixes := e.prof.MaxWorkers * 2; len(fixes) > maxFixes {
			e.emit(Event{Kind: EvInfo, Text: fmt.Sprintf("limited to %d fixes (profile %s)", maxFixes, e.prof.Name)})
			fixes = fixes[:maxFixes]
		}

		// Параллельное выполнение fix-задач
		var mu sync.Mutex
		// Роли, чей результат уже перезаписан в этом раунде: первая правка для
		// роли заменяет первый черновик, а вторая дописывается к первой. Иначе
		// два исправления одной роли шли бы «кто последний, тот и прав», и один
		// из выводов терялся бы молча и непредсказуемо.
		reworked := make(map[string]bool, len(fixes))

		stopFixes := dispatchWriters(e, ctx, fixes,
			func(fixItem fix) string { return fixItem.Role },
			func(fixItem fix, runner *tools.Runner) {
				st := e.subtaskForRole(plan, fixItem.Role)
				out := e.runWorker(ctx, st, plan, fixItem.Task, runner)

				mu.Lock()
				// Обновляем существующий результат для этой роли: первая правка
				// заменяет черновик, последующие дописываются, чтобы ни один
				// вывод не пропал (порядок склейки зависит от того, кто первым
				// закончил, но потерять текст он уже не может).
				updated := false
				for i := range results {
					if results[i].role == fixItem.Role {
						if reworked[fixItem.Role] {
							results[i].summary += "\n\n" + out
						} else {
							results[i].summary = out
						}
						updated = true
						break
					}
				}
				if !updated {
					results = append(results, workerResult{role: fixItem.Role, summary: out})
				}
				reworked[fixItem.Role] = true
				mu.Unlock()
			})

		if stopFixes {
			return e.localSummary(results, "Ran out of credits during rework."), nil
		}
	}

	// 5) SYNTHESIZE final answer. Out of credits → assemble locally (no spend).
	if e.depleted() {
		e.emitDepleted()
		return e.localSummary(results, "Ran out of credits — summary assembled without a model call."), nil
	}
	e.emit(Event{Kind: EvPhase, Text: "Assembling the result"})
	return e.synthesize(ctx, task, plan, results, testOut)
}

// dispatch runs work over items with at most prof.MaxWorkers of them in flight,
// and returns true if it stopped dispatching early because credits ran out.
//
// Both parallel phases (первый прогон воркеров и цикл правок) go through here,
// because the ordering of the checks is the whole substance of the thing and two
// copies of it drift apart: the balance has to be read before a slot is taken
// (иначе воркер, которому нечего делать, держит слот) and again after it is
// taken (пока горутина стояла в очереди, usage-событие соседа могло обнулить
// баланс). role() names the item in the skip event; work() does the actual step
// and is responsible for its own locking around shared results.
//
// Callers get back only the early-stop flag: whatever work() managed to record
// before the balance ran out is already in the caller's own slice, so a partial
// run still reports partial results.
//
// dispatchWriters runs a fan-out of writers with per-writer working-tree
// isolation (roadmap-v3 §4.1) and then integrates their work in a controlled
// order. It is the single path both writer fan-outs take — the first cut and the
// fix rounds — so the two cannot drift on how isolation or integration works.
//
// A lone writer gets the project tree directly (runner == nil): there is nothing
// to isolate it from, and an overlay would only add a copy-up per file and an
// integration step to reach the same result.
//
// When isolation cannot be established the run falls back to the §40 read-only
// restriction rather than to shared writing. That ordering is deliberate: the two
// possible answers to "we cannot give each writer its own tree" are "do not write"
// and "write into one tree anyway", and the second is the one measured to lose
// work.
func dispatchWriters[T any](e *Engine, ctx context.Context, items []T, role func(T) string, work func(T, *tools.Runner)) bool {
	// A lone writer has nothing to be isolated from, so it works in the project tree
	// as before — an overlay would only add a copy-up per file and an integration
	// step to reach the same result.
	parallel := e.prof.MaxWorkers > 1 && len(items) > 1
	if !parallel {
		return dispatch(e, ctx, items, role, func(item T) { work(item, nil) })
	}

	// The §0.3 rollout flag, now meaning "skip isolation and let the writers share
	// the project tree" — the pre-isolation behaviour, kept reachable for one
	// release so a user blocked by an isolation bug is not blocked by the CLI.
	if e.unsafeParallelWrites {
		e.emit(Event{Kind: EvInfo, Text: fmt.Sprintf(
			"unsafe: unsafe_parallel_writes is set, so the %d writers share one working tree "+
				"instead of each getting its own. Two writers editing one file both report success "+
				"and one edit is lost. Unset it to get per-writer isolation.", len(items))})
		return dispatch(e, ctx, items, role, func(item T) { work(item, nil) })
	}

	// No runner at all: only reachable from tests that drive the fan-out without a
	// filesystem. There is nothing to isolate and nothing to lose.
	if e.run == nil {
		return dispatch(e, ctx, items, role, func(item T) { work(item, nil) })
	}

	// One isolated tree per writer, created before any model call so a failure
	// costs nothing.
	isolated := make([]*tools.Runner, len(items))
	for i, item := range items {
		iso, err := e.run.Isolated(role(item) + "-" + strconv.Itoa(i))
		if err != nil {
			for _, done := range isolated[:i] {
				if done != nil {
					_ = done.Discard()
				}
			}
			// Fall back to read-only, never to shared writing. The two possible
			// answers here are "do not write" and "write into one tree anyway", and
			// the second is the one measured to lose work (ledger §40.2).
			e.sharedTree.Store(true)
			defer e.sharedTree.Store(false)
			e.emit(Event{Kind: EvError, Text: "cannot give each writer its own tree (" +
				clientcfg.DisplaySafe(err.Error()) + "), so this step runs read-only: the writers " +
				"report changes as text instead of applying them. Run with one worker to have them applied."})
			return dispatch(e, ctx, items, role, func(item T) { work(item, nil) })
		}
		isolated[i] = iso
	}
	e.emit(Event{Kind: EvInfo, Text: fmt.Sprintf(
		"isolation: each of the %d writers works in its own tree; their changes are integrated "+
			"in plan order after the step, and a path two writers changed is reported instead of "+
			"one edit silently winning (roadmap §4.1)", len(items))})

	// Index the item so the goroutine reaches its own tree without depending on
	// role uniqueness — two subtasks can carry the same role.
	type indexed struct {
		item T
		i    int
	}
	wrapped := make([]indexed, len(items))
	for i, item := range items {
		wrapped[i] = indexed{item: item, i: i}
	}
	earlyStop := dispatch(e, ctx, wrapped,
		func(w indexed) string { return role(w.item) },
		func(w indexed) { work(w.item, isolated[w.i]) })

	integrate(e, items, role, isolated)
	return earlyStop
}

// integrate collects each writer's changes and applies them to the project tree in
// the order the plan listed the writers — not the order they finished. A path two
// writers changed is reported and nothing is applied, because choosing a winner
// silently is the defect this row exists to remove.
func integrate[T any](e *Engine, items []T, role func(T) string, isolated []*tools.Runner) {
	contribs := make([]tools.Contribution, 0, len(isolated))
	for i, iso := range isolated {
		if iso == nil {
			continue
		}
		changes, err := iso.Changes()
		if err != nil {
			e.emit(Event{Kind: EvError, Role: role(items[i]),
				Text: "cannot read this writer's changes, so they are not integrated: " + err.Error()})
			continue
		}
		if len(changes) == 0 {
			continue
		}
		contribs = append(contribs, tools.Contribution{Writer: role(items[i]), Changes: changes})
	}
	defer func() {
		for _, iso := range isolated {
			if iso != nil {
				_ = iso.Discard()
			}
		}
	}()
	if len(contribs) == 0 {
		return
	}

	if conflicts := tools.Conflicts(contribs); len(conflicts) > 0 {
		var b strings.Builder
		for i, c := range conflicts {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(clientcfg.DisplaySafe(c.String()))
		}
		e.emit(Event{Kind: EvError, Text: "conflicting writers — nothing was applied: " + b.String() +
			". Re-run with a plan that gives each writer its own files, or with one worker."})
		return
	}

	applied, err := e.run.Integrate(contribs)
	if err != nil {
		e.emit(Event{Kind: EvError, Text: "integration failed after applying " +
			strconv.Itoa(len(applied)) + " file(s): " + clientcfg.DisplaySafe(err.Error()) +
			". Use undo to roll the applied files back."})
		return
	}
	e.emit(Event{Kind: EvInfo, Text: fmt.Sprintf("integrated %d file(s) from %d writer(s) in plan order",
		len(applied), len(contribs))})
}

// It is a function rather than a method because Go does not allow type
// parameters on methods.
func dispatch[T any](e *Engine, ctx context.Context, items []T, role func(T) string, work func(T)) bool {
	sem := make(chan struct{}, e.prof.MaxWorkers)
	var wg sync.WaitGroup
	var earlyStop bool

	// dispatch is concurrency and credit policy only. Whether a concurrent step may
	// write is decided by dispatchWriters, which is the one place that knows whether
	// each writer got its own tree (roadmap-v3 §4.1).

	for _, it := range items {
		if e.depleted() {
			e.emitDepleted()
			earlyStop = true
			break
		}

		wg.Add(1)
		go func(item T) {
			defer wg.Done()

			if ctx.Err() != nil || e.depleted() {
				e.emitSkipped(role(item))
				return
			}

			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil || e.depleted() {
				e.emitSkipped(role(item))
				return
			}

			work(item)
		}(it)
	}

	wg.Wait()
	return earlyStop
}

// emitDepleted reports the out-of-credits stop to the UI once.
func (e *Engine) emitDepleted() {
	e.emit(Event{Kind: EvError, Role: "lead",
		Text: fmt.Sprintf("credits exhausted (balance %.1f) — team stopped", e.balanceSnapshot())})
}

// emitSkipped сообщает об отказе запустить воркер из-за отмены или исчерпания
// кредитов. Role санитизируется через DisplaySafe, потому что приходит из
// плана, сгенерированного моделью (.ai/SECURITY.md:33).
func (e *Engine) emitSkipped(role string) {
	e.emit(Event{Kind: EvInfo, Role: clientcfg.DisplaySafe(role),
		Text: "skipped (credits exhausted or cancelled)"})
}

// localSummary assembles a final report from worker results without an LLM call
// (used when credits are exhausted, so stopping costs nothing more).
func (e *Engine) localSummary(results []workerResult, note string) string {
	var b strings.Builder
	b.WriteString("⚠ " + note + "\n\nWhat the team completed:\n")
	if len(results) == 0 {
		b.WriteString("— nothing (stopped at start).")
	}
	for _, r := range results {
		fmt.Fprintf(&b, "• [%s] %s\n", r.role, firstLine(r.summary))
	}
	b.WriteString("\nTop up your balance or switch to a paid plan for a full run.")
	return b.String()
}

// ---- phases -------------------------------------------------------------

// clarify asks the lead/planner whether the task is specific enough. If not, it
// surfaces 2-4 questions to the user (via e.ask) and returns their answer to
// fold into the task. Returns "" when no clarification is needed or available.
func (e *Engine) clarify(ctx context.Context, task string) string {
	sys := "You are the team lead at the briefing stage. Assess whether the task has enough specifics so the team avoids guesswork: " +
		"target audience, platform/stack, scope, key requirements, style. " +
		"If the task is already clear enough, return {\"needs\":false,\"questions\":[]}. " +
		"If something important is missing, return 2–4 SHORT specific questions: {\"needs\":true,\"questions\":[\"...\",\"...\"]}. " +
		"Do not ask questions for their own sake. JSON only."
	text, err := e.runAgent(ctx, agentSpec{
		role: "lead", model: e.prof.ManagerModel, effort: e.prof.ManagerEffort,
		system: sys, user: "User task:\n" + task, allowTools: nil,
		autoApprove: false, maxSteps: 2,
	})
	if err != nil {
		return ""
	}
	var v struct {
		Needs     bool     `json:"needs"`
		Questions []string `json:"questions"`
	}
	if raw := extractJSON(text); raw != "" {
		_ = json.Unmarshal([]byte(raw), &v)
	}
	if !v.Needs || len(v.Questions) == 0 {
		return ""
	}
	e.emit(Event{Kind: EvPhase, Text: "Task clarification"})
	var b strings.Builder
	b.WriteString("Before we start, please clarify:\n")
	for i, q := range v.Questions {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(q))
	}
	answer := strings.TrimSpace(e.ask(strings.TrimRight(b.String(), "\n")))
	return answer
}

const planSchema = `{"summary":"one sentence describing what we build","test_command":"verification command or empty","subtasks":[{"role":"backend","skill_query":"keywords to find a skill","task":"what exactly to do"}]}`

func (e *Engine) plan(ctx context.Context, task string) (Plan, error) {
	sys := "You are the planner of an AI agent team. Study the task and (via read-only tools) the project, then split the work into roles. " +
		"Example roles: architect, backend, database, frontend, designer, marketing, tester, devops, content. " +
		"For each subtask give a short skill_query (keywords to find a library skill, e.g. 'go rest api postgres' or 'tailwind landing page'). " +
		"Return STRICTLY one JSON object matching the schema and nothing else:\n" + planSchema
	user := "User task:\n" + task + "\n\nInspect the project first if needed (list_dir, search_code, read_file), then return the JSON plan."

	text, err := e.runAgent(ctx, agentSpec{
		role: "planner", model: e.prof.ManagerModel, effort: e.prof.ManagerEffort,
		system: sys, user: user, allowTools: readOnlyTools,
		autoApprove: false, maxSteps: stepsPlanner,
	})
	if err != nil {
		return Plan{}, err
	}
	return parsePlan(text), nil
}

func (e *Engine) replan(ctx context.Context, task string, plan Plan, critique string) (Plan, error) {
	cur, _ := json.Marshal(plan)
	sys := "You are the planner. Improve the plan per the critic's remarks. Return STRICTLY one JSON object with the same schema:\n" + planSchema
	user := "Task:\n" + task + "\n\nCurrent plan:\n" + string(cur) + "\n\nCritic remarks:\n" + critique + "\n\nReturn the corrected JSON plan."
	text, err := e.runAgent(ctx, agentSpec{
		role: "planner", model: e.prof.ManagerModel, effort: e.prof.ManagerEffort,
		system: sys, user: user, allowTools: nil, autoApprove: false, maxSteps: 2,
	})
	if err != nil {
		return Plan{}, err
	}
	return parsePlan(text), nil
}

// critiquePlan returns (critique, approved). approved=true means no changes needed.
func (e *Engine) critiquePlan(ctx context.Context, task string, plan Plan) (string, bool) {
	cur, _ := json.Marshal(plan)
	sys := "You are the architect-critic. Harshly but constructively assess the team plan: does it cover the task, are the roles right, is anything redundant, is the test command sensible. " +
		"If the plan is good, reply exactly 'APPROVED'. Otherwise list specific fixes."
	user := "Task:\n" + task + "\n\nPlan:\n" + string(cur)
	text, err := e.runAgent(ctx, agentSpec{
		role: "critic", model: e.prof.ManagerModel, effort: e.prof.ManagerEffort,
		system: sys, user: user, allowTools: nil, autoApprove: false, maxSteps: stepsCritic,
	})
	if err != nil {
		return "", true // on error, don't block the run
	}
	approved := strings.Contains(strings.ToUpper(text), "APPROVED")
	return text, approved
}

// runWorker runs one worker. runner is the tree it writes into: its own isolated
// worktree during a parallel fan-out, or nil to use the shared project tree when
// it is the only writer.
func (e *Engine) runWorker(ctx context.Context, st Subtask, plan Plan, feedback string, runner *tools.Runner) string {
	skillNote := ""
	if st.skillBody != "" {
		skillNote = "\n\n===== SKILL: " + st.skillName + " =====\n" + st.skillBody + "\n===== END SKILL =====\n"
	}
	sys := "You are a worker agent on the team. Your role: " + st.Role + ". " +
		"Complete your part of the task fully: read and edit files via tools, write real working code. " +
		"Shell commands are not available to you — a separate tester agent runs builds and tests, so just write correct code. " +
		"Make minimal precise edits (patch_file), do not duplicate what you already read. Do not ask for confirmations. " +
		"When done, give a SHORT summary (what you did, which files)."
	if isVisualRole(st.Role) {
		sys += antiSlopRule
	}
	sys += skillNote
	user := "Overall plan: " + plan.Summary + "\n\nYour subtask:\n" + st.Task
	if feedback != "" {
		user += "\n\nFix per review remarks:\n" + feedback
	}

	e.emit(Event{Kind: EvAgentStart, Role: st.Role, Text: st.skillName})
	out, err := e.runAgent(ctx, agentSpec{
		role: st.Role, model: e.prof.WorkerModel, effort: e.prof.WorkerEffort,
		system: sys, user: user, allowTools: workerTools,
		autoApprove: true, allowCommands: false, maxSteps: stepsWorker,
		runner: runner,
	})
	if err != nil {
		e.emit(Event{Kind: EvError, Role: st.Role, Text: err.Error()})
		return "error: " + err.Error()
	}
	e.emit(Event{Kind: EvAgentEnd, Role: st.Role, Text: firstLine(out)})
	return out
}

func (e *Engine) runTester(ctx context.Context, plan Plan) string {
	cmd := strings.TrimSpace(plan.TestCommand)
	sys := "You are the tester. Run the project verification via run_command (build/tests/lint). " +
		"If no command is given, determine it yourself from the project. Return a verdict: first line 'PASS' or 'FAIL', then key errors briefly."
	user := "Verification command: "
	if cmd == "" {
		user += "(not set — determine it yourself)"
	} else {
		user += cmd
	}
	e.emit(Event{Kind: EvAgentStart, Role: "tester", Text: cmd})
	out, err := e.runAgent(ctx, agentSpec{
		role: "tester", model: e.prof.WorkerModel, effort: e.prof.WorkerEffort,
		system: sys, user: user, allowTools: testerTools,
		autoApprove: true, allowCommands: true, maxSteps: stepsTester,
	})
	if err != nil {
		e.emit(Event{Kind: EvError, Role: "tester", Text: err.Error()})
		return "FAIL\n" + err.Error()
	}
	e.emit(Event{Kind: EvAgentEnd, Role: "tester", Text: firstLine(out)})
	return out
}

// fix is one corrective action the lead orders.
type fix struct {
	Role string `json:"role"`
	Task string `json:"task"`
}

// review asks the lead whether the result is acceptable. Returns (fixes, ok);
// ok=true means ship as-is.
func (e *Engine) review(ctx context.Context, task string, plan Plan, results []workerResult, testOut string) ([]fix, bool) {
	sys := "You are the team lead. Assess the result against the task and the test outcome. " +
		"If everything is good, return JSON {\"ok\":true,\"fixes\":[]}. " +
		"If fixes are needed, return {\"ok\":false,\"fixes\":[{\"role\":\"backend\",\"task\":\"what to fix\"}]}. " +
		"Assign fixes to roles from the plan. JSON only, no explanations."
	user := "Task:\n" + task + "\n\nWhat the workers did:\n" + summariesText(results) +
		"\n\nTest outcome:\n" + clip(testOut, 2000)
	text, err := e.runAgent(ctx, agentSpec{
		role: "lead", model: e.prof.ManagerModel, effort: e.prof.ManagerEffort,
		system: sys, user: user, allowTools: nil, autoApprove: false, maxSteps: stepsLead,
	})
	if err != nil {
		return nil, true
	}
	var v struct {
		OK    bool  `json:"ok"`
		Fixes []fix `json:"fixes"`
	}
	if raw := extractJSON(text); raw != "" {
		_ = json.Unmarshal([]byte(raw), &v)
	}
	return v.Fixes, v.OK
}

func (e *Engine) synthesize(ctx context.Context, task string, plan Plan, results []workerResult, testOut string) (string, error) {
	sys := "You are the team lead. Summarize for the user briefly and to the point: what was built, key files/decisions, how to run it, test status and what remains (if anything). Write in English, no fluff."
	user := "Task:\n" + task + "\n\nTeam output:\n" + summariesText(results) + "\n\nTests:\n" + clip(testOut, 1500)
	return e.runAgent(ctx, agentSpec{
		role: "lead", model: e.prof.ManagerModel, effort: e.prof.ManagerEffort,
		system: sys, user: user, allowTools: nil, autoApprove: false, maxSteps: stepsLead,
	})
}

// ---- skill resolution ---------------------------------------------------

// visualSkillName is the anti-AI-slop design skill forced on every visual role
// (designer, frontend, ui, …) so those workers never produce generic AI-slop.
const visualSkillName = "frontend-design"

// antiSlopRule is a hard design constraint appended to visual roles' prompts.
const antiSlopRule = "\n\nHARD DESIGN RULE (MUST NOT BE BROKEN): generic AI slop is strictly forbidden. " +
	"Do NOT use template AI aesthetics: Inter/Roboto/Arial/system fonts, purple gradients on white, " +
	"predictable grids and components, cream background + serif + terracotta, dark background + one acid accent. " +
	"Commit to ONE bold deliberate aesthetic direction for the specific brief: distinctive typography (a display+body pair), " +
	"a considered palette, non-template composition, one memorable signature element. Follow the frontend-design skill below."

// isVisualRole reports whether a role produces user-facing visual design.
func isVisualRole(role string) bool {
	r := strings.ToLower(role)
	for _, k := range []string{"design", "дизайн", "frontend", "front-end", "front", "ui", "ux", "web", "веб", "landing", "лендинг"} {
		if strings.Contains(r, k) {
			return true
		}
	}
	return false
}

func (e *Engine) resolveSkill(st *Subtask) {
	if e.lib == nil {
		return
	}
	// Visual roles are forced onto the anti-slop design skill, ignoring whatever
	// the planner queried — design quality is non-negotiable for these.
	if isVisualRole(st.Role) {
		if body, err := e.lib.Body(visualSkillName); err == nil && body != "" {
			st.skillName = visualSkillName
			st.skillBody = body
			e.emit(Event{Kind: EvInfo, Text: st.Role + " → skill «" + visualSkillName + "» (anti-slop, enforced)"})
			return
		}
	}
	q := st.SkillQuery
	if q == "" {
		q = st.Role + " " + st.Task
	}
	matches := e.lib.Find(q, 1)
	if len(matches) == 0 {
		return
	}
	body, err := e.lib.Body(matches[0].Name)
	if err != nil {
		return
	}
	st.skillName = matches[0].Name
	st.skillBody = body
	e.emit(Event{Kind: EvInfo, Text: st.Role + " → skill «" + matches[0].Name + "»"})
}

func (e *Engine) subtaskForRole(plan Plan, role string) Subtask {
	for _, st := range plan.Subtasks {
		if strings.EqualFold(st.Role, role) {
			return st
		}
	}
	// Unknown role: synthesize a generalist subtask carrying the role label.
	return Subtask{Role: role, Task: "rework per review"}
}

// ---- the headless agent loop -------------------------------------------

type agentSpec struct {
	role   string
	model  string
	effort string
	system string
	user   string

	// allowTools is the authoritative list of tool names this agent may call.
	// Empty means no tools at all (pure completion).
	//
	// It replaced a pre-rendered json.RawMessage of definitions, and the change is
	// the point: definitions are now derived from this list (toolDefs) and the
	// dispatch gate checks against the same list (toolGate). Before, the list
	// existed only inside the definitions blob, so the gate had nothing to check
	// and any name the model produced was executed — a definition set is what the
	// model is *offered*, never what it is *limited to*.
	allowTools []string

	maxTokens int
	// autoApprove lets file-mutating tools (write_file/patch_file) run without a
	// human prompt. It deliberately does NOT cover run_command: arbitrary command
	// execution is never auto-approved in team mode (H1), because there is no
	// human in the loop here. run_command runs only when allowCommands is set
	// (the tester), and only after passing the deny-list (tools.ScreenCommand).
	autoApprove bool
	// allowCommands permits run_command for this agent (the tester, whose job is
	// to run the build/tests). Workers never get it, so a worker cannot shell out.
	allowCommands bool
	maxSteps      int

	// runner is the tool runner this agent executes through. nil means the shared
	// project runner, which is right for every read-only role and for a lone
	// writer. A writer in a parallel fan-out gets its own isolated worktree here
	// (roadmap-v3 §4.1), so its edits land in a tree no other agent can see.
	runner *tools.Runner
}

// toolRunner returns the runner an agent's calls go through.
func (e *Engine) toolRunner(spec agentSpec) *tools.Runner {
	if spec.runner != nil {
		return spec.runner
	}
	return e.run
}

// runAgent drives one agent through a tool-calling loop until it returns a final
// text answer (no more tool calls) or hits its step budget.
func (e *Engine) runAgent(ctx context.Context, spec agentSpec) (string, error) {
	maxTokens := spec.maxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAgentMaxTokens(spec)
	}
	msgs := []client.Message{
		{Role: "system", Content: spec.system},
		{Role: "user", Content: spec.user},
	}
	var final string
	for step := 0; step < spec.maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return final, err
		}
		text, calls, err := e.once(ctx, client.ChatRequest{
			SessionID: e.sessionID + "-" + shortHash(spec.role),
			Mode:      "chat",
			Model:     spec.model,
			Effort:    spec.effort,
			// Trim stale tool I/O before re-sending (token economy): a worker can
			// run many steps, and without this the whole growing transcript of
			// file reads/writes was re-sent verbatim every step.
			Messages:  client.TrimMessages(msgs),
			MaxTokens: maxTokens,
			Tools:     toolDefs(spec.allowTools...),
		})
		if err != nil {
			return final, err
		}
		if strings.TrimSpace(text) != "" {
			final = text
		}
		if len(calls) == 0 {
			return final, nil
		}
		msgs = append(msgs, client.Message{Role: "assistant", Content: text, ToolCalls: calls})
		for _, c := range calls {
			if skip := e.toolGate(spec, c); skip != "" {
				msgs = append(msgs, client.Message{
					Role: "tool", ToolCallID: c.ID, Name: c.Function.Name, Content: skip,
				})
				continue
			}
			res, _ := e.toolRunner(spec).Execute(c.Function.Name, c.Function.Arguments)
			e.emit(Event{Kind: EvTool, Role: spec.role, Text: tools.Summary(c.Function.Name, c.Function.Arguments)})
			msgs = append(msgs, client.Message{
				Role: "tool", ToolCallID: c.ID, Name: c.Function.Name, Content: res,
			})
		}
	}
	return final, nil
}

func defaultAgentMaxTokens(spec agentSpec) int {
	if len(spec.allowTools) == 0 {
		switch spec.role {
		case "planner":
			return 1200
		case "critic", "lead":
			return 900
		default:
			return 1200
		}
	}
	switch spec.role {
	case "planner", "tester":
		return 1200
	default:
		return 2500
	}
}

// toolGate enforces the headless team-mode tool policy (H1). It returns "" if
// the call may run, or a tool-result string (delivered back to the model) when
// it is refused. Because no human can approve a call in team mode, the gate is
// the only thing standing between the model and the user's machine:
//
//   - the call must be a tool this agent was given. This check is first and
//     fails closed. It used to be absent: the gate ended in `return ""`, so any
//     name that was not run_command and not approval-gated ran, and the
//     per-role lists were only ever applied when *building definitions*. A model
//     that named a tool it had not been offered — its own invention, or one
//     belonging to another role — was executed.
//   - run_command is never auto-approved for ordinary workers; only an agent
//     explicitly granted allowCommands (the tester) may run commands at all.
//   - every run_command — even the tester's — must pass the deny-list, so a
//     model cannot pipe a download into a shell or wipe a directory.
//   - file-mutating tools run only when the agent has autoApprove; read-only
//     tools always run.
//   - MCP tools never run here. They require human approval by policy, and team
//     mode has no human, so the answer is always no rather than sometimes.
func (e *Engine) toolGate(spec agentSpec, c client.ToolCall) string {
	name := c.Function.Name

	if !allowed(spec.allowTools, name) {
		if !tools.Known(name) {
			return "skipped: no tool named " + shortName(name) + " exists"
		}
		return "skipped: " + shortName(name) + " is not available to this role in team mode"
	}
	if tools.IsMCPName(name) {
		// Unreachable through allowTools today, since no role lists an MCP tool.
		// It is here because "team mode has no human, and every MCP tool requires
		// a human" is the invariant, and a future role definition should fail
		// against it rather than quietly gain third-party reach.
		return "skipped: MCP tools require explicit human approval, which team mode cannot obtain"
	}

	// roadmap-v3 §4.1. Concurrency and writing are mutually exclusive until every
	// writer has its own tree. This is checked before the per-role rules below so
	// the model is told the real reason: run after them, a worker — which does
	// hold write grants — would be refused with a message about its role, and a
	// correct grant would look like a misconfiguration.
	if e.sharedTree.Load() && tools.TouchesWorkTree(name) {
		return "skipped: " + shortName(name) + " changes the working tree, and " +
			strconv.Itoa(e.prof.MaxWorkers) + " agents are sharing one tree right now. Parallel " +
			"steps are read-only until each writer gets its own worktree: describe the change you " +
			"would make, in full, as text — it is collected and applied afterwards. A run with one " +
			"worker writes directly."
	}

	if name == tools.ToolRunCommand {
		if !spec.allowCommands {
			return "skipped: command execution is not available to this role in team mode (no human approval)"
		}
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(c.Function.Arguments), &a)
		if reason := tools.ScreenCommand(a.Command); reason != "" {
			return "skipped: command rejected by security policy — " + reason
		}
		return ""
	}
	if !spec.autoApprove && tools.RequiresApproval(name) {
		return "skipped: writing is disabled for this role (read-only)"
	}
	return ""
}

// allowed reports whether name is in the agent's list.
func allowed(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// shortName bounds and flattens a model-supplied tool name before it is echoed
// back in a message. The name comes from model output, and the message is
// rendered to the user's terminal (.ai/RULES.md:22).
func shortName(name string) string {
	const max = 48
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	if b.Len() == 0 {
		return "(unnamed)"
	}
	return b.String()
}

// once performs a single chat round, draining the SSE stream and returning the
// accumulated text and any requested tool calls.
func (e *Engine) once(ctx context.Context, req client.ChatRequest) (string, []client.ToolCall, error) {
	ch, err := e.cli.Chat(ctx, req)
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	var calls []client.ToolCall
	var streamErr error
	for ev := range ch {
		switch ev.Kind {
		case client.EventToken:
			sb.WriteString(ev.Text)
		case client.EventToolCalls:
			calls = ev.ToolCalls
		case client.EventUsage:
			// Track both the backend balance and the optional local budget. This runs
			// on parallel SSE readers, so one mutex makes both limits visible before
			// dispatch starts another worker.
			if ev.Usage != nil {
				e.recordUsage(ev.Usage)
			}
		case client.EventError:
			// Keep the FIRST error: it is the root cause, whereas later events
			// (e.g. a generic stream-closed) would mask it (L7).
			if streamErr == nil {
				streamErr = errors.New(ev.ErrMsg)
			}
		}
	}
	return sb.String(), calls, streamErr
}

// depleted reports whether the user's known credit balance is exhausted. Until
// the first usage event arrives the balance is unknown and we do not block.
func (e *Engine) depleted() bool {
	e.balanceMu.Lock()
	defer e.balanceMu.Unlock()
	return (e.balanceKnown && e.balance <= 0) || (e.budget > 0 && e.spent >= e.budget)
}

// recordUsage updates the backend balance and consumes the caller's local
// ceiling. Credits is the billed amount for this step; malformed negative
// reports are ignored for the local ceiling but never make it larger.
func (e *Engine) recordUsage(usage *client.Usage) {
	e.balanceMu.Lock()
	defer e.balanceMu.Unlock()
	e.balance = usage.Balance
	e.balanceKnown = true
	if e.budget > 0 && usage.Credits > 0 {
		e.spent += usage.Credits
	}
}

func (e *Engine) setBalance(bal float64) {
	e.balanceMu.Lock()
	e.balance = bal
	e.balanceKnown = true
	e.balanceMu.Unlock()
}

// balanceSnapshot returns the current balance for logging (safe read).
func (e *Engine) balanceSnapshot() float64 {
	e.balanceMu.Lock()
	defer e.balanceMu.Unlock()
	return e.balance
}

// ---- tool definition subsets -------------------------------------------

var (
	readOnlyTools = []string{tools.ToolReadFile, tools.ToolListDir, tools.ToolSearchCode, tools.ToolRecall}
	testerTools   = []string{tools.ToolRunCommand, tools.ToolReadFile, tools.ToolListDir, tools.ToolSearchCode}
	// workerTools are what a build worker may use: it reads, searches and edits
	// files, but it CANNOT run commands (H1) — running the build/tests is the
	// tester's job, the one role allowed to shell out (and only via the deny-list).
	workerTools = []string{
		tools.ToolReadFile, tools.ToolListDir, tools.ToolSearchCode,
		tools.ToolPatchFile, tools.ToolWriteFile,
		tools.ToolRemember, tools.ToolRecall,
	}
)

// toolDefs returns the function definitions for the named tools, filtered from
// the full set the runner advertises.
func toolDefs(allow ...string) json.RawMessage {
	want := map[string]bool{}
	for _, n := range allow {
		want[n] = true
	}
	var defs []map[string]any
	if err := json.Unmarshal(tools.Definitions(), &defs); err != nil {
		return nil
	}
	out := defs[:0]
	for _, d := range defs {
		if fn, ok := d["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); want[name] {
				out = append(out, d)
			}
		}
	}
	b, _ := json.Marshal(out)
	return b
}

// ---- small helpers ------------------------------------------------------

func parsePlan(text string) Plan {
	var p Plan
	if raw := extractJSON(text); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

// extractJSON returns the first balanced {...} object found in s, or "".
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func summariesText(rs []workerResult) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "• [%s] %s\n", r.role, clip(r.summary, 600))
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 120)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
