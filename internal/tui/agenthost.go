package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/agentcore"
	"qeuro/internal/client"
	"qeuro/internal/tools"
)

// agentHostMsg — внутренние сообщения от agentcore.Engine к Bubble Tea UI.
// Эти типы приватные для TUI и не видны agentcore: адаптер переводит
// agentcore.Event в tea.Msg без импорта Bubble Tea в agentcore.
type (
	// agentEventMsg несёт одно событие от engine.
	agentEventMsg struct {
		ev agentcore.Event
	}

	// agentDoneMsg означает, что engine завершился (ok/cancelled/error).
	// Это terminal событие: после него больше не придёт agentEventMsg.
	agentDoneMsg struct {
		status string // "ok" | "cancelled" | "error"
	}
)

// agentHost — адаптер между agentcore.Engine и Bubble Tea.
// Запускает engine в фоне, получает события через канал, конвертирует в tea.Msg.
type agentHost struct {
	ctx      context.Context
	cancel   context.CancelFunc
	eventCh  chan agentcore.Event
	approval chan agentcore.HostCommand
	cancelCh chan struct{}
}

// startAgentHost запускает один solo агент через agentcore.Engine.
// Возвращает tea.Cmd, который будет посылать agentEventMsg и завершится agentDoneMsg.
func startAgentHost(
	ctx context.Context,
	provider client.Provider,
	runner *tools.Runner,
	prompt string,
	model string,
	budgetCredits float64,
) (*agentHost, tea.Cmd) {
	// Cancellable контекст: хост отменит его на Escape/Ctrl+C
	ctx, cancel := context.WithCancel(ctx)

	// Буферизованные каналы, чтобы engine не блокировался на emit
	eventCh := make(chan agentcore.Event, 32)
	approval := make(chan agentcore.HostCommand, 4)
	cancelCh := make(chan struct{}, 1)

	host := &agentHost{
		ctx:      ctx,
		cancel:   cancel,
		eventCh:  eventCh,
		approval: approval,
		cancelCh: cancelCh,
	}

	// Запускаем engine в фоне
	go func() {
		emitter := agentcore.NewChannelEmitter(eventCh, "tui-solo")
		engine := &agentcore.Engine{
			Emit:      emitter,
			Approvals: approval,
			Cancel:    cancelCh,
			Deps: agentcore.Deps{
				Provider: provider,
				Runner:   runner,
			},
			Opts: agentcore.Options{
				Model:         model,
				AutoApprove:   false,
				BudgetCredits: budgetCredits,
				MaxToolSteps:  0, // использует DefaultMaxToolSteps
			},
		}

		_ = engine.Run(ctx, prompt)
		// Engine.Run гарантированно эмитит done перед возвратом, и ChannelEmitter
		// закроет eventCh на done, поэтому здесь ничего закрывать не нужно.
	}()

	// tea.Cmd, который читает события из канала и превращает их в tea.Msg
	return host, host.listenEvents()
}

// listenEvents — Bubble Tea команда, которая читает события из канала.
func (h *agentHost) listenEvents() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-h.ctx.Done():
			// Контекст отменён, но engine ещё может отправить terminal done
			return nil
		case ev, ok := <-h.eventCh:
			if !ok {
				// Канал закрыт — это не должно случиться без done, но если случилось,
				// обрабатываем как отмену
				return agentDoneMsg{status: "cancelled"}
			}
			if ev.Kind == agentcore.KindDone {
				return agentDoneMsg{status: ev.Status}
			}
			return agentEventMsg{ev: ev}
		}
	}
}

// approve отправляет решение подтверждения в engine.
func (h *agentHost) approve(id string, approved bool) {
	decision := "deny"
	if approved {
		decision = "approve"
	}
	select {
	case h.approval <- agentcore.HostCommand{ID: id, Decision: decision}:
	case <-h.ctx.Done():
	}
}

// stop отменяет запущенный engine.
func (h *agentHost) stop() {
	select {
	case h.cancelCh <- struct{}{}:
	default:
	}
	h.cancel()
}
