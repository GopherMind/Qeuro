package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// stdio framing per the specification: newline-delimited JSON on the child's
// stdin/stdout. Messages MUST NOT contain embedded newlines (json.Marshal
// escapes them, so this holds by construction). The server MUST NOT write
// anything but MCP messages to stdout; stderr is a free-form log and the client
// SHOULD NOT treat output there as an error — so stderr is collected for
// diagnostics only.
const (
	// maxLineBytes bounds one message. Generous because a tools/list response
	// with full JSON Schemas easily exceeds bufio's default 64 KiB — the same
	// asymmetry that is a latent defect in agentcore/host.go:22 — but bounded,
	// because an unbounded line buffer is a memory-exhaustion vector driven by a
	// third-party process.
	maxLineBytes = 4 << 20

	// stderrLimit bounds retained stderr, matching cloud-worker's supervisor.
	stderrLimit = 8 << 10

	// shutdownGrace is how long a server gets to exit after its stdin is closed
	// before it is killed. Windows has no signals, so Cmd.WaitDelay plus
	// Process.Kill is the portable mechanism; there is no SIGTERM path to write.
	shutdownGrace = 5 * time.Second
)

// ErrClosed is returned once a transport has been closed or its child has exited.
var ErrClosed = errors.New("mcp: transport is closed")

// Transport carries JSON-RPC messages to one server. The interface exists so the
// streamable HTTP transport can be added without reworking the client; stdio is
// the only implementation today.
type Transport interface {
	// Call sends a request and waits for its response.
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Notify sends a notification, which by specification is never answered.
	Notify(ctx context.Context, method string, params any) error
	// Close shuts the transport down and releases its resources.
	Close() error
	// Diagnostics returns whatever the server logged, for error messages.
	Diagnostics() string
}

// stdioTransport supervises one child process and multiplexes requests over its
// stdin/stdout.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *boundedBuffer

	writeMu sync.Mutex // serializes writes: one message per line, no interleaving

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	closed  bool
	readErr error

	closeOnce sync.Once
	done      chan struct{} // closed when the reader goroutine has exited
}

// StdioConfig describes how to start a server process.
type StdioConfig struct {
	Command string
	Args    []string
	Dir     string
	// Env is the child's complete environment. It REPLACES the parent's rather
	// than extending it, which is what keeps provider keys out of a third-party
	// process by construction instead of by an exclusion list that a new variable
	// name could slip past (roadmap §4.8, .ai/AI.md:49).
	Env []string
}

// StartStdio launches a server and returns a transport for it.
//
// The command is not passed through tools.SanitizeCommand. That allow-list
// exists to stop the *model* from obtaining arbitrary execution; mcp.json is
// written by the user by hand, where "npx" is a legitimate entry. The boundary
// enforced instead is that the config is never read from the working directory
// and that exec receives an argv vector with no shell (.ai/RULES.md:23).
func StartStdio(cfg StdioConfig) (Transport, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("mcp: server command is empty")
	}
	// #nosec G204 -- argv vector, never a shell string. cfg.Command comes from
	// the user's own ~/.qeuro/mcp.json (a project-local mcp.json is deliberately
	// ignored) and never from model output or repository content.
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	// Non-nil even when empty: a nil Env would make exec inherit the parent's.
	cmd.Env = cfg.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.WaitDelay = shutdownGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stderr := &boundedBuffer{limit: stderrLimit}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: cannot start %q: %w", cfg.Command, err)
	}

	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  stderr,
		pending: map[int64]chan response{},
		done:    make(chan struct{}),
	}
	go t.readLoop(stdout)
	return t, nil
}

// readLoop reads responses until EOF or a read error, then fails every waiting
// call.
//
// That last part is the whole point. agentcore/host.go:23 reads with a default
// scanner, never consults sc.Err() and never closes its channels, so a consumer
// can wait forever on a child that died. For JSON-RPC that is not acceptable: a
// crashed server must surface as an error on the call, not as a hang.
func (t *stdioTransport) readLoop(stdout io.Reader) {
	defer close(t.done)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var resp response
		if err := json.Unmarshal(line, &resp); err != nil {
			// A server that writes non-MCP data to stdout violates the
			// specification, but killing the session over one bad line would be
			// worse than ignoring it: the line cannot be a response we are waiting
			// for, and the waiter still has its context deadline.
			continue
		}
		t.deliver(resp)
	}

	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	if errors.Is(err, bufio.ErrTooLong) {
		err = fmt.Errorf("mcp: server sent a message larger than %d bytes", maxLineBytes)
	}
	t.failAll(err)
}

// deliver routes a response to its waiter. A response whose ID matches nothing
// is dropped: it may be an answer to a call that already timed out.
func (t *stdioTransport) deliver(resp response) {
	if resp.ID == nil {
		return // a notification or a malformed response; nothing waits on it
	}
	t.mu.Lock()
	ch, ok := t.pending[*resp.ID]
	if ok {
		delete(t.pending, *resp.ID)
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	ch <- resp
	close(ch)
}

// failAll completes every waiting call with err and blocks further calls.
func (t *stdioTransport) failAll(err error) {
	t.mu.Lock()
	t.closed = true
	if t.readErr == nil {
		t.readErr = err
	}
	pending := t.pending
	t.pending = map[int64]chan response{}
	t.mu.Unlock()

	for id, ch := range pending {
		ch <- response{ID: &id, Error: &rpcError{
			Code:    CodeInternalError,
			Message: "server exited before answering: " + err.Error(),
		}}
		close(ch)
	}
}

// Call sends a request and waits for its response, the caller's context, or the
// server's death — whichever comes first.
func (t *stdioTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	payload, err := withMeta(params)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode params: %w", err)
	}

	t.mu.Lock()
	if t.closed {
		err := t.readErr
		t.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
	t.nextID++
	id := t.nextID
	ch := make(chan response, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	req := request{JSONRPC: "2.0", ID: &id, Method: method, Params: payload}
	if err := t.write(req); err != nil {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		// Tell the server to stop working, per the specification, and stop
		// waiting. The notification is best-effort: a wedged server that ignores
		// it is handled by the caller's timeout and, ultimately, by Close.
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		_ = t.Notify(context.Background(), MethodCancelled, map[string]any{
			"requestId": id,
			"reason":    "client cancelled",
		})
		return nil, ctx.Err()
	}
}

// Notify sends a notification. Notifications carry no ID and get no response.
func (t *stdioTransport) Notify(_ context.Context, method string, params any) error {
	payload, err := withMeta(params)
	if err != nil {
		return fmt.Errorf("mcp: encode params: %w", err)
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return t.write(request{JSONRPC: "2.0", Method: method, Params: payload})
}

// write emits one message as a single line. json.Marshal escapes newlines inside
// strings, so the "no embedded newlines" rule holds without a separate check.
func (t *stdioTransport) write(req request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("mcp: encode request: %w", err)
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("mcp: write to server: %w", err)
	}
	return nil
}

// Close shuts the server down: close stdin so it can exit on its own, wait
// briefly, then kill. Cmd.WaitDelay covers the kill, which is what makes this
// portable — Windows has no signals, so there is no graceful-signal step to add.
func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		t.failAll(ErrClosed)
		_ = t.stdin.Close()

		select {
		case <-t.done:
		case <-time.After(shutdownGrace):
		}
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.cmd.Wait()
	})
	return nil
}

// Diagnostics returns what the server wrote to stderr. Untrusted text: it is
// shown to the user as a quoted server log, never interpreted.
func (t *stdioTransport) Diagnostics() string { return t.stderr.String() }

// boundedBuffer collects at most limit bytes and drops the rest, so a chatty or
// hostile server cannot exhaust memory through stderr.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.buf = append(b.buf, "\n…[stderr truncated]"...)
		}
	}
	// Always report the full length: a short write would make the child's own
	// writes fail with io.ErrShortWrite.
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
