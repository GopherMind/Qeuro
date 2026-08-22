package tui

import (
	"context"
	"testing"

	"qeuro/internal/agentcore"
	"qeuro/internal/client"
	"qeuro/internal/tools"
)

// TestAgentHostBasicFlow проверяет, что адаптер правильно конвертирует события.
func TestAgentHostBasicFlow(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]client.Event{
			{
				{Kind: client.EventRoute, Route: &client.Route{Model: "test-model", Effort: "low"}},
				{Kind: client.EventToken, Text: "Hello"},
				{Kind: client.EventToken, Text: " world"},
			},
		},
	}

	host, cmd := startAgentHost(
		context.Background(),
		provider,
		nil, // no runner needed for text-only turn
		"test prompt",
		"auto",
		0,
	)
	defer host.stop()

	// Первое событие — route
	msg := cmd()
	if msg == nil {
		t.Fatal("expected route event, got nil")
	}
	evMsg, ok := msg.(agentEventMsg)
	if !ok {
		t.Fatalf("expected agentEventMsg, got %T", msg)
	}
	if evMsg.ev.Kind != agentcore.KindRoute {
		t.Errorf("expected route, got %s", evMsg.ev.Kind)
	}

	// Следующие события — токены
	for i := 0; i < 2; i++ {
		cmd = host.listenEvents()
		msg = cmd()
		if msg == nil {
			t.Fatal("expected token event, got nil")
		}
		evMsg, ok = msg.(agentEventMsg)
		if !ok {
			t.Fatalf("expected agentEventMsg, got %T", msg)
		}
		if evMsg.ev.Kind != agentcore.KindToken {
			t.Errorf("expected token, got %s", evMsg.ev.Kind)
		}
	}

	// Последнее событие — assistant + usage + done
	for {
		cmd = host.listenEvents()
		msg = cmd()
		if msg == nil {
			continue
		}
		if done, ok := msg.(agentDoneMsg); ok {
			if done.status != agentcore.DoneOK {
				t.Errorf("expected ok status, got %s", done.status)
			}
			break
		}
	}
}

// TestAgentHostApproval проверяет flow подтверждения.
func TestAgentHostApproval(t *testing.T) {
	repo := t.TempDir()
	runner, err := tools.NewRunner(repo)
	if err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{
		turns: [][]client.Event{
			toolTurn("call-1", tools.ToolWriteFile, `{"path":"test.txt","content":"hello"}`),
			{
				{Kind: client.EventToken, Text: "Done"},
			},
		},
	}

	host, cmd := startAgentHost(
		context.Background(),
		provider,
		runner,
		"write a file",
		"auto",
		0,
	)
	defer host.stop()

	// Ищем approval_request
	var approvalID string
	for {
		msg := cmd()
		if msg == nil {
			t.Fatal("no approval request received")
		}
		if done, ok := msg.(agentDoneMsg); ok {
			t.Fatalf("got done before approval: %s", done.status)
		}
		evMsg, ok := msg.(agentEventMsg)
		if !ok {
			continue
		}
		if evMsg.ev.Kind == agentcore.KindApprovalRequest {
			approvalID = evMsg.ev.ID
			break
		}
		cmd = host.listenEvents()
	}

	// Подтверждаем
	host.approve(approvalID, true)

	// Ждём file_write
	foundWrite := false
	for i := 0; i < 20; i++ {
		cmd = host.listenEvents()
		msg := cmd()
		if msg == nil {
			continue
		}
		if doneMsg, ok := msg.(agentDoneMsg); ok {
			if !foundWrite {
				t.Error("got done without seeing file_write")
			}
			if doneMsg.status != agentcore.DoneOK {
				t.Errorf("expected ok status, got %s", doneMsg.status)
			}
			break
		}
		if evMsg, ok := msg.(agentEventMsg); ok && evMsg.ev.Kind == agentcore.KindFileWrite {
			foundWrite = true
		}
	}

	if !foundWrite {
		t.Error("never received file_write event")
	}
}

// TestAgentHostCancellation проверяет, что stop() прерывает engine.
func TestAgentHostCancellation(t *testing.T) {
	// Провайдер, который блокируется навсегда
	provider := &blockingProvider{}

	host, cmd := startAgentHost(
		context.Background(),
		provider,
		nil,
		"test",
		"auto",
		0,
	)

	// Отменяем сразу
	host.stop()

	// Должны получить done с cancelled
	msg := cmd()
	for msg == nil {
		cmd = host.listenEvents()
		msg = cmd()
	}

	done, ok := msg.(agentDoneMsg)
	if !ok {
		t.Fatalf("expected agentDoneMsg after cancel, got %T", msg)
	}
	if done.status != agentcore.DoneCancelled {
		t.Errorf("expected cancelled status, got %s", done.status)
	}
}

// fakeProvider — тестовый провайдер со скриптованными ответами.
type fakeProvider struct {
	turns [][]client.Event
	turn  int
}

func (p *fakeProvider) Chat(ctx context.Context, req client.ChatRequest) (<-chan client.Event, error) {
	if p.turn >= len(p.turns) {
		// Финальный ход без тулов
		ch := make(chan client.Event, 2)
		ch <- client.Event{Kind: client.EventToken, Text: "All done"}
		ch <- client.Event{Kind: client.EventUsage, Usage: &client.Usage{In: 10, Out: 5, CostUSD: 0.001}}
		close(ch)
		return ch, nil
	}

	events := p.turns[p.turn]
	p.turn++

	ch := make(chan client.Event, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	// Всегда добавляем usage
	ch <- client.Event{Kind: client.EventUsage, Usage: &client.Usage{In: 100, Out: 50, CostUSD: 0.01}}
	close(ch)
	return ch, nil
}

func toolTurn(id, name, args string) []client.Event {
	return []client.Event{
		{Kind: client.EventToolCalls, ToolCalls: []client.ToolCall{
			{ID: id, Function: client.FunctionCall{Name: name, Arguments: args}},
		}},
	}
}

// blockingProvider никогда не возвращает события — для теста cancellation.
type blockingProvider struct{}

func (p *blockingProvider) Chat(ctx context.Context, req client.ChatRequest) (<-chan client.Event, error) {
	ch := make(chan client.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
