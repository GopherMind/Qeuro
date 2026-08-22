package agentcore

import (
	"errors"
	"sync"
)

var (
	// ErrEnginePanic is returned after Run converts a dependency panic into the
	// protocol's error + done/error terminal sequence. The recovered value is
	// deliberately excluded: provider and tool errors can contain credentials or
	// model-controlled content and do not belong in a host-visible error string.
	ErrEnginePanic = errors.New("agent engine panic")
	// ErrTerminalMissing marks an internal return path that reached the Run
	// boundary without emitting done. The boundary still attempts done/error so a
	// host is never left waiting on an otherwise successful process exit.
	ErrTerminalMissing = errors.New("agent engine returned without a terminal event")
	ErrEmitterMissing  = errors.New("agent event emitter is required")
)

// terminalEmitter makes the terminal-event invariant a property of the run
// boundary rather than a convention at every return site. It serializes the
// downstream emitter, records done only after the downstream write succeeds,
// and drops every later event. A failed done write may therefore be retried by
// Run's deferred fallback without duplicating a terminal that was delivered.
type terminalEmitter struct {
	mu         sync.Mutex
	downstream EventEmitter
	terminal   bool
}

func newTerminalEmitter(downstream EventEmitter) *terminalEmitter {
	return &terminalEmitter{downstream: downstream}
}

func (e *terminalEmitter) Emit(ev Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal {
		return nil
	}
	if e.downstream == nil {
		return ErrEmitterMissing
	}
	if err := e.downstream.Emit(ev); err != nil {
		return err
	}
	if ev.Kind == KindDone {
		e.terminal = true
	}
	return nil
}

func (e *terminalEmitter) terminalSent() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.terminal
}
