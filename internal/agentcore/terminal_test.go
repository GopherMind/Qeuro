package agentcore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"qeuro/internal/client"
	"qeuro/internal/tools"
)

const panicSecretSentinel = "provider-secret-must-not-reach-events"

type panickingProvider struct{}

func (panickingProvider) Chat(context.Context, client.ChatRequest) (<-chan client.Event, error) {
	panic(panicSecretSentinel)
}

type oneTurnProvider struct {
	events []client.Event
}

func (p oneTurnProvider) Chat(context.Context, client.ChatRequest) (<-chan client.Event, error) {
	ch := make(chan client.Event, len(p.events))
	for _, ev := range p.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

type panickingToolRunner struct{}

func (panickingToolRunner) Execute(string, string) (string, bool) {
	panic(panicSecretSentinel)
}

func TestRunRecoversDependencyPanicsWithExactlyOneTerminal(t *testing.T) {
	tests := []struct {
		name string
		deps Deps
		opts Options
	}{
		{name: "provider", deps: Deps{Provider: panickingProvider{}}},
		{
			name: "tool runner",
			deps: Deps{
				Provider: oneTurnProvider{events: []client.Event{{
					Kind: client.EventToolCalls,
					ToolCalls: []client.ToolCall{{
						ID:       "panic-tool",
						Type:     "function",
						Function: client.FunctionCall{Name: tools.ToolListDir, Arguments: `{"path":"."}`},
					}},
				}}},
				Runner: panickingToolRunner{},
			},
			opts: Options{AutoApprove: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			engine := &Engine{
				Emit: NewEmitter(&output, "panic-run"),
				Deps: tc.deps,
				Opts: tc.opts,
			}

			err := engine.Run(context.Background(), "exercise panic handling")
			if !errors.Is(err, ErrEnginePanic) {
				t.Fatalf("dependency panic error = %v, want ErrEnginePanic", err)
			}
			events := decodeEvents(t, &output)
			assertOneTerminal(t, events, DoneError)
			var crashErrors int
			for _, ev := range events {
				if strings.Contains(ev.Text, panicSecretSentinel) {
					t.Fatalf("panic payload leaked into event: %+v", ev)
				}
				if ev.Kind == KindError && ev.Code == "engine_panic" {
					crashErrors++
				}
			}
			if crashErrors != 1 {
				t.Fatalf("engine_panic errors = %d, want 1: %+v", crashErrors, events)
			}
		})
	}
}

type failOnceEmitter struct {
	failKind string
	failed   bool
	events   []Event
}

func (e *failOnceEmitter) Emit(ev Event) error {
	if ev.Kind == e.failKind && !e.failed {
		e.failed = true
		return errors.New("injected event sink failure")
	}
	e.events = append(e.events, ev)
	return nil
}

func TestRunEmitterFailureStillAttemptsExactlyOneTerminal(t *testing.T) {
	sink := &failOnceEmitter{failKind: KindAssistant}
	engine := &Engine{
		Emit: sink,
		Deps: Deps{Provider: oneTurnProvider{events: []client.Event{{Kind: client.EventToken, Text: "done"}}}},
	}
	if err := engine.Run(context.Background(), "finish"); err == nil {
		t.Fatal("injected sink failure returned nil error")
	}
	assertOneTerminal(t, sink.events, DoneError)
}

func TestRunRetriesAFailedTerminalWriteWithoutDuplicatingDone(t *testing.T) {
	sink := &failOnceEmitter{failKind: KindDone}
	engine := &Engine{
		Emit: sink,
		Deps: Deps{Provider: oneTurnProvider{events: []client.Event{{Kind: client.EventToken, Text: "done"}}}},
	}
	if err := engine.Run(context.Background(), "finish"); err == nil {
		t.Fatal("injected terminal write failure returned nil error")
	}
	assertOneTerminal(t, sink.events, DoneError)
}

func TestTerminalEmitterDropsEveryEventAfterDone(t *testing.T) {
	sink := &failOnceEmitter{}
	emitter := newTerminalEmitter(sink)
	if err := emitter.Emit(Event{Kind: KindDone, Status: DoneOK}); err != nil {
		t.Fatal(err)
	}
	if err := emitter.Emit(Event{Kind: KindDone, Status: DoneError}); err != nil {
		t.Fatal(err)
	}
	if err := emitter.Emit(Event{Kind: KindError, Code: "late"}); err != nil {
		t.Fatal(err)
	}
	assertOneTerminal(t, sink.events, DoneOK)
}

func TestHostCancelEmitsExactlyOneTerminal(t *testing.T) {
	cancel := make(chan struct{})
	close(cancel)
	var output bytes.Buffer
	engine := &Engine{
		Emit:   NewEmitter(&output, "cancel-run"),
		Cancel: cancel,
		Deps:   Deps{Provider: oneTurnProvider{events: []client.Event{{Kind: client.EventToken, Text: "late"}}}},
	}
	if err := engine.Run(context.Background(), "cancel"); err != nil {
		t.Fatalf("cancel Run: %v", err)
	}
	assertOneTerminal(t, decodeEvents(t, &output), DoneCancelled)
}

func TestWithHostCancelAppliesQueuedCancelBeforeReturn(t *testing.T) {
	cancel := make(chan struct{})
	close(cancel)
	engine := &Engine{Cancel: cancel}
	ctx, stop := engine.withHostCancel(context.Background())
	defer stop()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("queued cancel was not applied synchronously: %v", ctx.Err())
	}
}

func assertOneTerminal(t *testing.T, events []Event, status string) {
	t.Helper()
	var terminal []Event
	for _, ev := range events {
		if ev.Kind == KindDone {
			terminal = append(terminal, ev)
		}
	}
	if len(terminal) != 1 || terminal[0].Status != status {
		t.Fatalf("terminal events = %+v, want exactly one done/%s (all=%+v)", terminal, status, events)
	}
	if len(events) == 0 || events[len(events)-1].Kind != KindDone {
		t.Fatalf("terminal event is not last: %+v", events)
	}
}
