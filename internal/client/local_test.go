package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLocalProviderOllamaStreamsSharedEvents(t *testing.T) {
	var got ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"models":[{"name":"qwen2.5-coder:7b","model":"qwen2.5-coder:7b"}]}`)
		case "/api/chat":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request: %v", err)
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			flusher, _ := w.(http.Flusher)
			for _, line := range []string{
				`{"message":{"role":"assistant","content":"Hello"},"done":false}`,
				`{"message":{"role":"assistant","content":" world"},"done":false}`,
				`{"message":{"role":"assistant"},"done":true}`,
			} {
				_, _ = fmt.Fprintln(w, line)
				flusher.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "", DialectAuto)
	ch, err := p.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 123,
	})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []EventKind
	var text string
	for ev := range ch {
		kinds = append(kinds, ev.Kind)
		text += ev.Text
	}
	if want := []EventKind{EventRoute, EventToken, EventToken, EventDone}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	if text != "Hello world" {
		t.Fatalf("text = %q", text)
	}
	if got.Model != "qwen2.5-coder:7b" || !got.Stream {
		t.Fatalf("request model/stream = %q/%v", got.Model, got.Stream)
	}
	if got.Options["num_predict"] != float64(123) && got.Options["num_predict"] != 123 {
		t.Fatalf("num_predict = %#v, want 123", got.Options["num_predict"])
	}
}

func TestLocalProviderOpenAIAccumulatesFragmentedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-","type":"function","function":{"name":"read_","arguments":"{\"path\":"}}]}}]}`)
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"1","function":{"name":"file","arguments":"\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`)
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "local", DialectOpenAI)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "inspect"}}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	var done bool
	for ev := range ch {
		if ev.Kind == EventToolCalls {
			calls = ev.ToolCalls
		}
		if ev.Kind == EventDone {
			done = true
		}
	}
	if !done || len(calls) != 1 {
		t.Fatalf("done=%v calls=%+v", done, calls)
	}
	if calls[0].ID != "call-1" || calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("tool call = %+v", calls[0])
	}
}

func TestLocalProviderIncompleteStreamIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"partial"},"done":false}`)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "model", DialectOllama)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got Event
	for ev := range ch {
		if ev.Kind == EventError {
			got = ev
		}
	}
	if got.ErrCode != "local_stream_incomplete" {
		t.Fatalf("error = %+v", got)
	}
}

func TestLocalProviderUnavailableIsTypedAndDoesNotUseProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:2")
	p := NewLocalProvider("http://127.0.0.1:1", "model", DialectOllama)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "secret"}}})
	if !errors.Is(err, ErrLocalUnavailable) {
		t.Fatalf("error = %v, want ErrLocalUnavailable", err)
	}
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error lacks safe endpoint/action: %v", err)
	}
}

func TestValidateLocalURL(t *testing.T) {
	for _, good := range []string{"", "http://localhost:11434", "https://models.internal:8443"} {
		if err := ValidateLocalURL(good); err != nil {
			t.Errorf("ValidateLocalURL(%q): %v", good, err)
		}
	}
	for _, bad := range []string{"localhost:11434", "file:///tmp/model", "http://user:pass@localhost:11434", "://bad"} {
		if err := ValidateLocalURL(bad); err == nil {
			t.Errorf("ValidateLocalURL(%q) succeeded", bad)
		}
	}
}

// ollamaStub is an Ollama stand-in: /api/tags identifies the dialect, /api/chat
// streams the given NDJSON lines. models is what /api/tags reports.
func ollamaStub(t *testing.T, models string, lines []string, capture *ollamaRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, models)
		case "/api/chat":
			if capture != nil {
				if err := json.NewDecoder(r.Body).Decode(capture); err != nil {
					t.Errorf("decode request: %v", err)
				}
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, l := range lines {
				_, _ = fmt.Fprintln(w, l)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const oneOllamaModel = `{"models":[{"name":"qwen2.5-coder:7b","model":"qwen2.5-coder:7b"}]}`

func drain(ch <-chan Event) []Event {
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// TestLocalProviderIgnoresCloudModelID guards the failure that would make
// `qeuro --local` unusable by default: the TUI always carries its cloud
// catalogue selection in ChatRequest.Model, and forwarding that id to Ollama asks
// for a model no local server can have.
func TestLocalProviderIgnoresCloudModelID(t *testing.T) {
	var got ollamaRequest
	srv := ollamaStub(t, oneOllamaModel, []string{`{"message":{"content":"ok"},"done":true}`}, &got)

	p := NewLocalProvider(srv.URL, "", DialectAuto)
	ch, err := p.Chat(context.Background(), ChatRequest{
		Model:    "anthropic/claude-opus-4",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)

	if got.Model != "qwen2.5-coder:7b" {
		t.Fatalf("request model = %q, want the local server's own model", got.Model)
	}
}

// TestLocalProviderConfiguredModelWins: --local-model / QEURO_LOCAL_MODEL has to
// beat the server's first model, or the setting silently does nothing.
func TestLocalProviderConfiguredModelWins(t *testing.T) {
	var got ollamaRequest
	srv := ollamaStub(t, oneOllamaModel, []string{`{"message":{"content":"ok"},"done":true}`}, &got)

	p := NewLocalProvider(srv.URL, "deepseek-coder:33b", DialectAuto)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)

	if got.Model != "deepseek-coder:33b" {
		t.Fatalf("request model = %q, want the configured one", got.Model)
	}
}

// TestLocalProviderOllamaToolCallsGetIDs keeps the inspect/edit/verify loop
// working offline. Ollama omits call ids, and the tool loop matches each result
// back to a call by id — without one the next request is invalid.
func TestLocalProviderOllamaToolCallsGetIDs(t *testing.T) {
	var got ollamaRequest
	srv := ollamaStub(t, oneOllamaModel, []string{
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"main.go"}}}]},"done":true}`,
	}, &got)

	p := NewLocalProvider(srv.URL, "m", DialectOllama)
	ch, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read it"}},
		Tools:    json.RawMessage(`[{"type":"function","function":{"name":"read_file"}}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	for _, ev := range drain(ch) {
		if ev.Kind == EventToolCalls {
			calls = ev.ToolCalls
		}
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID == "" {
		t.Error("tool call has no id: a result cannot be matched to it")
	}
	if calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("tool call = %+v", calls[0])
	}
	if len(got.Tools) == 0 {
		t.Error("tool definitions were not forwarded to the local server")
	}
}

// TestLocalProviderModelErrorSurfaces: Ollama reports an out-of-memory model in
// an "error" field with HTTP 200, so checking only the status would show the user
// a successful, empty answer.
func TestLocalProviderModelErrorSurfaces(t *testing.T) {
	srv := ollamaStub(t, oneOllamaModel, []string{`{"error":"model requires more system memory"}`}, nil)

	p := NewLocalProvider(srv.URL, "m", DialectOllama)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(ch)
	last := events[len(events)-1]
	if last.Kind != EventError || !strings.Contains(last.ErrMsg, "system memory") {
		t.Fatalf("last event = %+v, want the server's error text", last)
	}
}

// TestLocalProviderDetectsOpenAIDialect covers the second server the roadmap row
// names: llama.cpp answers 404 on Ollama's paths, so a provider that assumed one
// protocol fails on half the supported setups.
func TestLocalProviderDetectsOpenAIDialect(t *testing.T) {
	var got openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r) // including /api/tags: "not Ollama"
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"all good"},"finish_reason":"stop"}]}`)
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "", DialectAuto)
	ch, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "review"}}, MaxTokens: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, ev := range drain(ch) {
		text += ev.Text
	}
	if p.Dialect() != DialectOpenAI {
		t.Errorf("dialect = %q, want openai", p.Dialect())
	}
	if text != "all good" {
		t.Errorf("text = %q", text)
	}
	if got.MaxTokens != 256 {
		t.Errorf("max_tokens = %d, want it forwarded", got.MaxTokens)
	}
}

// TestLocalProviderFailedProbeIsNotMemoised: a probe that failed because the
// server was not up yet must not fix the dialect for the session's lifetime —
// the next turn, after `ollama serve`, has to detect the real protocol.
func TestLocalProviderFailedProbeIsNotMemoised(t *testing.T) {
	dead := NewLocalProvider("http://127.0.0.1:1", "m", DialectAuto)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dead.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("Chat to a dead endpoint succeeded")
	}
	if d := dead.Dialect(); d != DialectAuto {
		t.Fatalf("failed probe was memoised as %q, want it left unresolved", d)
	}

	live := ollamaStub(t, oneOllamaModel, []string{`{"message":{"content":"ok"},"done":true}`}, nil)
	p := NewLocalProvider(live.URL, "m", DialectAuto)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	if d := p.Dialect(); d != DialectOllama {
		t.Errorf("dialect = %q, want ollama after a successful probe", d)
	}
}

// TestLocalProviderRejectedRequestIsNotUnavailable: a 400 from a running server
// needs different advice than a stopped one. Telling the user to start a server
// that is already running sends them the wrong way.
func TestLocalProviderRejectedRequestIsNotUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = fmt.Fprint(w, oneOllamaModel)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"model \"ghost\" not found, try pulling it first"}`)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "ghost", DialectAuto)
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if errors.Is(err, ErrLocalUnavailable) {
		t.Error("a rejected request was reported as an unreachable server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not carry the server's message", err)
	}
}

// TestLocalProviderCancellationClosesStream: Esc has to end a local turn too, and
// a parser goroutine left blocked on a send would leak for the whole session.
func TestLocalProviderCancellationClosesStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = fmt.Fprint(w, oneOllamaModel)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(w, `{"message":{"content":"start"},"done":false}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	p := NewLocalProvider(srv.URL, "m", DialectAuto)
	ch, err := p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		drain(ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancelling the context did not close the stream")
	}
}

// TestLocalProviderSendsNoCredentials is the promise offline mode makes about the
// bearer token: the endpoint is user-supplied, so a request carrying the token
// would hand the user's credential to whatever is listening there.
func TestLocalProviderSendsNoCredentials(t *testing.T) {
	var chatHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = fmt.Fprint(w, oneOllamaModel)
			return
		}
		chatHeaders = r.Header.Clone()
		_, _ = fmt.Fprintln(w, `{"message":{"content":"ok"},"done":true}`)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "m", DialectAuto)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)

	for _, h := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if got := chatHeaders.Get(h); got != "" {
			t.Errorf("local request carried %s: %q", h, got)
		}
	}
}

// TestLocalProviderNoInstalledModel: a fresh Ollama with nothing pulled is a
// common first-run state, and the command that fixes it is more use than the
// server's own 400.
func TestLocalProviderNoInstalledModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = fmt.Fprint(w, `{"models":[]}`)
			return
		}
		t.Errorf("unexpected request to %s: no model should have been sent", r.URL.Path)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "", DialectAuto)
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Chat succeeded with no model installed")
	}
	if !strings.Contains(err.Error(), "ollama pull") {
		t.Errorf("error %q does not say how to install a model", err)
	}
}

// TestBothProvidersSatisfyTheInterface keeps the substitution this row depends
// on: if either type drifts, the paths that were not updated keep using the
// backend while the user believes the session is offline.
func TestBothProvidersSatisfyTheInterface(t *testing.T) {
	var _ Provider = (*LocalProvider)(nil)
	var _ Provider = (*Client)(nil)
}

// A compliant SSE server may name its events and number them. Those fields carry
// nothing this reader needs, but failing on them would break offline mode against
// a server doing nothing wrong.
func TestLocalProviderSkipsSSEMetadataFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			": keep-alive comment",
			"event: message",
			"id: 1",
			"retry: 2000",
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			"data: [DONE]",
		} {
			_, _ = fmt.Fprintln(w, line)
		}
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "local", DialectOpenAI)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var done bool
	for _, ev := range drain(ch) {
		switch ev.Kind {
		case EventToken:
			text += ev.Text
		case EventDone:
			done = true
		case EventError:
			t.Fatalf("SSE metadata field reported as an error: %s", ev.ErrMsg)
		}
	}
	if text != "ok" || !done {
		t.Fatalf("text=%q done=%v", text, done)
	}
}

// An unrecognised line still has to fail. Skipping everything unknown would let a
// truncated or foreign stream pass as a clean finish.
func TestLocalProviderUnknownSSELineIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, "garbage: not an sse field")
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "local", DialectOpenAI)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var code string
	for _, ev := range drain(ch) {
		if ev.Kind == EventError {
			code = ev.ErrCode
		}
	}
	if code != "local_stream_invalid" {
		t.Fatalf("error code = %q, want local_stream_invalid", code)
	}
}

// Tool-call indices are the server's, not ours. A server numbering from 1 (or
// skipping an index) sent every call in full, so reporting "incomplete tool call"
// would fail a working model.
func TestLocalProviderToolCallIndicesNeedNotStartAtZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"name":"second","arguments":"{}"}}]}}]}`)
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"first","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "local", DialectOpenAI)
	ch, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	for _, ev := range drain(ch) {
		switch ev.Kind {
		case EventToolCalls:
			calls = ev.ToolCalls
		case EventError:
			t.Fatalf("unexpected error: %s (%s)", ev.ErrMsg, ev.ErrCode)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want 2", calls)
	}
	// Index order, not arrival order: the server's numbering is the model's
	// intended order, and a synthesised id must be unique within the turn.
	if calls[0].Function.Name != "first" || calls[1].Function.Name != "second" {
		t.Errorf("calls out of index order: %+v", calls)
	}
	if calls[0].ID != "local-1" || calls[1].ID != "local-2" {
		t.Errorf("synthesised ids = %q, %q; want local-1, local-2", calls[0].ID, calls[1].ID)
	}
}

// A redirect is not followed: 307/308 re-sends the POST body, so honouring one
// would forward the prompt to a host the user never named.
func TestLocalProviderDoesNotFollowRedirects(t *testing.T) {
	var elsewhere int
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"leaked"},"finish_reason":"stop"}]}`)
	}))
	defer away.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, away.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	p := NewLocalProvider(srv.URL, "local", DialectOpenAI)
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "secret"}}})
	if err == nil {
		t.Fatal("Chat followed a redirect instead of reporting it")
	}
	if elsewhere != 0 {
		t.Errorf("prompt reached the redirect target %d time(s)", elsewhere)
	}
}
