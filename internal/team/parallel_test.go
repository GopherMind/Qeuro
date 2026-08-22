package team

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testEngine builds an Engine that can be driven through dispatch() without a
// backend: no client, no runner, and events collected into a slice.
//
// Дispatch — единственное место, где живёт политика запуска воркеров, поэтому
// тесты вызывают именно её, а не копию цикла. Копия проходила бы даже с
// выломанной проверкой кредитов (см. .ai/review-8-cli-parallel.md, «Mutation
// Gaps» 2 и 6).
func testEngine(t *testing.T, maxWorkers int) (*Engine, func() []Event) {
	t.Helper()

	var mu sync.Mutex
	var events []Event

	e := New(nil, nil, nil, Profile{Name: "test", MaxWorkers: maxWorkers}, func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}, nil)

	return e, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), events...)
	}
}

func subtasks(roles ...string) []Subtask {
	out := make([]Subtask, 0, len(roles))
	for _, r := range roles {
		out = append(out, Subtask{Role: r})
	}
	return out
}

func roleOf(st Subtask) string { return st.Role }

// TestParallelWorkerExecution проверяет, что воркеры выполняются параллельно
// с ограничением по MaxWorkers.
func TestParallelWorkerExecution(t *testing.T) {
	e, _ := testEngine(t, 2)

	var active, maxConcurrent int32
	var mu sync.Mutex
	var done []string

	tasks := subtasks("worker1", "worker2", "worker3", "worker4")

	earlyStop := dispatch(e, context.Background(), tasks, roleOf, func(st Subtask) {
		current := atomic.AddInt32(&active, 1)
		defer atomic.AddInt32(&active, -1)

		for {
			max := atomic.LoadInt32(&maxConcurrent)
			if current <= max || atomic.CompareAndSwapInt32(&maxConcurrent, max, current) {
				break
			}
		}

		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		done = append(done, st.Role)
		mu.Unlock()
	})

	if earlyStop {
		t.Error("earlyStop=true with an unknown balance: nothing should have stopped the run")
	}
	if len(done) != len(tasks) {
		t.Errorf("ran %d workers, want %d", len(done), len(tasks))
	}

	maxSeen := atomic.LoadInt32(&maxConcurrent)
	if maxSeen > int32(e.prof.MaxWorkers) {
		t.Errorf("maxConcurrent=%d exceeds MaxWorkers=%d", maxSeen, e.prof.MaxWorkers)
	}
	if maxSeen < 2 {
		t.Errorf("expected at least 2 concurrent workers, got %d (parallelism not working)", maxSeen)
	}

	seen := make(map[string]bool, len(done))
	for _, r := range done {
		seen[r] = true
	}
	for _, st := range tasks {
		if !seen[st.Role] {
			t.Errorf("role %q was not executed", st.Role)
		}
	}
}

// TestParallelCancellation проверяет, что отмена контекста останавливает новые
// запуски.
func TestParallelCancellation(t *testing.T) {
	e, _ := testEngine(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var started int32
	tasks := subtasks("w1", "w2", "w3", "w4", "w5")

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	dispatch(e, ctx, tasks, roleOf, func(Subtask) {
		atomic.AddInt32(&started, 1)
		time.Sleep(100 * time.Millisecond)
	})

	if n := atomic.LoadInt32(&started); n >= int32(len(tasks)) {
		t.Errorf("expected cancellation to prevent some workers from starting, but all %d started", n)
	}
}

// TestParallelCancellationEfficiency проверяет, что отменённые воркеры не
// занимают слоты семафора: с MaxWorkers=1 и уже отменённым контекстом все
// задачи должны отказаться немедленно, а не по очереди.
func TestParallelCancellationEfficiency(t *testing.T) {
	e, _ := testEngine(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var started int32
	tasks := subtasks("w1", "w2", "w3", "w4", "w5")

	deadline := time.Now().Add(2 * time.Second)
	dispatch(e, ctx, tasks, roleOf, func(Subtask) {
		atomic.AddInt32(&started, 1)
		time.Sleep(500 * time.Millisecond)
	})

	if n := atomic.LoadInt32(&started); n != 0 {
		t.Errorf("%d workers ran under an already-cancelled context, want 0", n)
	}
	if time.Now().After(deadline) {
		t.Error("cancelled workers queued on the semaphore instead of exiting before acquiring it")
	}
}

// TestParallelResultsCollected проверяет, что результаты собираются корректно.
func TestParallelResultsCollected(t *testing.T) {
	e, _ := testEngine(t, 3)

	var mu sync.Mutex
	var results []workerResult
	tasks := subtasks("alpha", "beta", "gamma")

	dispatch(e, context.Background(), tasks, roleOf, func(st Subtask) {
		mu.Lock()
		results = append(results, workerResult{role: st.Role, summary: "result-" + st.Role})
		mu.Unlock()
	})

	if len(results) != len(tasks) {
		t.Fatalf("expected %d results, got %d", len(tasks), len(results))
	}

	counts := make(map[string]int)
	for _, r := range results {
		counts[r.role]++
		if want := "result-" + r.role; r.summary != want {
			t.Errorf("result for %q: got %q, want %q", r.role, r.summary, want)
		}
	}
	for _, st := range tasks {
		if counts[st.Role] != 1 {
			t.Errorf("role %q appeared %d times, want 1", st.Role, counts[st.Role])
		}
	}
}

// TestParallelBalanceDepletion: первый воркер обнуляет баланс так, как это
// делает usage-событие, и остальные должны отказаться, а не уйти в бэкенд за
// счёт, который уже нечем платить.
func TestParallelBalanceDepletion(t *testing.T) {
	e, events := testEngine(t, 1)
	e.setBalance(10)

	var ran []string
	var mu sync.Mutex
	tasks := subtasks("first", "second", "third", "fourth")

	// Оба выхода корректны и зависят только от планировщика, поэтому earlyStop
	// здесь НЕ проверяется:
	//
	//   - первый воркер обнуляет баланс до того, как цикл dispatch дошёл до
	//     следующего элемента -> цикл видит depleted() и прерывается,
	//     earlyStop=true;
	//   - цикл успевает запустить все 4 горутины -> воркеры 2-4 видят
	//     depleted() и пропускаются, earlyStop=false.
	//
	// Ранее тест требовал earlyStop=false и падал примерно в 7% прогонов
	// (22/300). Диагностика на 2000 прогонов: earlyStop=true в 2% случаев, при
	// этом число отработавших воркеров равно 1 во всех 2000. Инвариант, который
	// эта строка обязана защищать, — «после обнуления баланса не платит больше
	// никто», а не порядок, в котором планировщик разложил горутины.
	earlyStop := dispatch(e, context.Background(), tasks, roleOf, func(st Subtask) {
		mu.Lock()
		ran = append(ran, st.Role)
		mu.Unlock()

		// Как once(): баланс приходит от бэкенда после списания за шаг.
		e.setBalance(0)
	})

	mu.Lock()
	got := append([]string(nil), ran...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("ran %v after the balance hit zero, want only the first worker", got)
	}

	// Ни один воркер не выпадает молча, но событие зависит от того, какой из двух
	// путей выбрал планировщик: прерванный цикл сообщает об исчерпании кредитов
	// один раз, а полностью запущенный — по одному skip на каждого отказавшегося.
	var skipped, depleted int
	for _, ev := range events() {
		switch {
		case ev.Kind == EvInfo && strings.Contains(ev.Text, "skipped"):
			skipped++
		case ev.Kind == EvError && strings.Contains(ev.Text, "credits exhausted"):
			depleted++
		}
	}
	if earlyStop {
		if depleted == 0 {
			t.Error("dispatch stopped early without telling the user why")
		}
	} else if skipped == 0 {
		t.Error("no skip event emitted: workers dropped out invisibly")
	}
}

// TestParallelDepletionSeenAfterSemaphoreWait закрывает разрыв, найденный
// мутацией: удаление проверки кредитов ПОСЛЕ захвата семафора оставляло
// TestParallelBalanceDepletion зелёным, потому что пре-семафорная проверка
// успевала отсечь воркеров раньше и маскировала вторую.
//
// Здесь баланс ещё положителен, когда воркеры 2-4 проходят первую проверку и
// встают в очередь на семафор, а обнуляется уже во время их ожидания. Отсечь их
// может только проверка после семафора, поэтому её удаление обязано валить тест.
func TestParallelDepletionSeenAfterSemaphoreWait(t *testing.T) {
	e, _ := testEngine(t, 1)
	e.setBalance(10)

	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	queued := make(chan struct{}, 4)

	go func() {
		// Держим первого воркера внутри work() до тех пор, пока остальные не
		// упрутся в семафор с ещё положительным балансом.
		for i := 0; i < 3; i++ {
			<-queued
		}
		close(release)
	}()

	dispatch(e, context.Background(), subtasks("first", "second", "third", "fourth"), roleOf, func(st Subtask) {
		mu.Lock()
		first := len(ran) == 0
		ran = append(ran, st.Role)
		mu.Unlock()

		if !first {
			return
		}
		// Первый воркер занимает единственный слот семафора. Пока он держит его,
		// остальные три успевают пройти пре-семафорную проверку и заблокироваться.
		for i := 0; i < 3; i++ {
			queued <- struct{}{}
		}
		<-release
		// Баланс обнуляется, когда конкуренты уже ждут семафор.
		e.setBalance(0)
	})

	mu.Lock()
	got := append([]string(nil), ran...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("ran %v; workers waiting on the semaphore must re-check the balance after acquiring it", got)
	}
}

// TestParallelDepletedBeforeDispatch: при нулевом балансе на входе не должен
// стартовать ни один воркер, а пользователь должен увидеть причину.
func TestParallelDepletedBeforeDispatch(t *testing.T) {
	e, events := testEngine(t, 3)
	e.setBalance(0)

	var started int32
	earlyStop := dispatch(e, context.Background(), subtasks("a", "b", "c"), roleOf, func(Subtask) {
		atomic.AddInt32(&started, 1)
	})

	if !earlyStop {
		t.Error("earlyStop=false with a zero balance, want true")
	}
	if n := atomic.LoadInt32(&started); n != 0 {
		t.Errorf("%d workers ran with a zero balance, want 0", n)
	}

	var told bool
	for _, ev := range events() {
		if ev.Kind == EvError && strings.Contains(ev.Text, "credits exhausted") {
			told = true
		}
	}
	if !told {
		t.Error("no credits-exhausted event emitted before refusing to dispatch")
	}
}

// TestParallelUnknownBalanceRuns: до первого usage-события баланс неизвестен, и
// это не повод отказываться работать (иначе первый же запуск встал бы).
func TestParallelUnknownBalanceRuns(t *testing.T) {
	e, _ := testEngine(t, 2)

	var started int32
	if earlyStop := dispatch(e, context.Background(), subtasks("a", "b"), roleOf, func(Subtask) {
		atomic.AddInt32(&started, 1)
	}); earlyStop {
		t.Error("earlyStop=true with an unknown balance, want false")
	}
	if n := atomic.LoadInt32(&started); n != 2 {
		t.Errorf("ran %d workers with an unknown balance, want 2", n)
	}
}

// TestParallelMaxWorkersOne: MaxWorkers=1 должен означать строго
// последовательное выполнение, без наложения шагов.
func TestParallelMaxWorkersOne(t *testing.T) {
	e, _ := testEngine(t, 1)

	var active, overlaps int32
	tasks := subtasks("a", "b", "c", "d")

	dispatch(e, context.Background(), tasks, roleOf, func(Subtask) {
		if atomic.AddInt32(&active, 1) > 1 {
			atomic.AddInt32(&overlaps, 1)
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&active, -1)
	})

	if n := atomic.LoadInt32(&overlaps); n != 0 {
		t.Errorf("MaxWorkers=1 ran workers concurrently %d times, want strictly sequential", n)
	}
}

// TestParallelSkipEventSanitizesRole: роль приходит из плана, сгенерированного
// моделью, поэтому в событие она должна попадать без управляющих символов
// (.ai/SECURITY.md:33).
func TestParallelSkipEventSanitizesRole(t *testing.T) {
	e, events := testEngine(t, 2)
	e.setBalance(10)

	nasty := "back\x1b[2Jend\nrm -rf /"
	tasks := []Subtask{{Role: "first"}, {Role: nasty}}

	dispatch(e, context.Background(), tasks, roleOf, func(Subtask) {
		e.setBalance(0)
	})

	for _, ev := range events() {
		if strings.ContainsAny(ev.Role, "\x1b\n\r") {
			t.Errorf("event role %q carries control characters into the terminal", ev.Role)
		}
	}
}
