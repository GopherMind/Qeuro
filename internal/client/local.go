package client

// Offline mode (roadmap §8, row "Offline"). LocalProvider is the second
// implementation of Provider: instead of the Qeuro backend it talks to a model
// running on this machine (or on a host inside the same closed network), so
// commit messages, explanations and reviews work with no internet at all.
//
// Two server dialects cover what the roadmap row names. Ollama speaks its own
// newline-delimited-JSON protocol on /api/chat; llama.cpp's server speaks the
// OpenAI-compatible /v1/chat/completions with SSE. Both are auto-detected, so a
// user only has to say where the server is.
//
// Nothing here sends a bearer token. That is the point of the row: a closed
// contour must not be handed the user's credential, and a request that carries
// no token cannot leak one even if the endpoint is misconfigured.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultLocalURL is where Ollama listens out of the box.
var DefaultLocalURL = "http://localhost:11434"

// LocalDialect selects the wire protocol of the local server.
type LocalDialect string

const (
	// DialectAuto probes the endpoint once and picks one of the two below.
	DialectAuto LocalDialect = "auto"
	// DialectOllama is Ollama's own POST /api/chat (newline-delimited JSON).
	DialectOllama LocalDialect = "ollama"
	// DialectOpenAI is the OpenAI-compatible POST /v1/chat/completions (SSE),
	// which llama.cpp's server, vLLM and LM Studio all speak.
	DialectOpenAI LocalDialect = "openai"
)

// localIdleTimeout bounds the wait for the next byte of a local stream. It is
// far more generous than the backend's: a laptop CPU running a 7B model can take
// tens of seconds to produce the first token, and cutting that off would make
// offline mode look broken on exactly the hardware it exists for.
const localIdleTimeout = 10 * time.Minute

// LocalProvider streams completions from a model on this host.
type LocalProvider struct {
	baseURL string
	model   string
	httpc   *http.Client

	// dialectMu guards dialect, which starts as the configured value and is
	// replaced by the probe's answer. A mutex rather than sync.Once because a
	// failed probe must not be memoised — the server may simply not be up yet, and
	// the next turn has to try again — and a Once cannot be un-done safely while
	// other goroutines may be calling it. The TUI shares one provider across
	// Bubble Tea commands, which run on separate goroutines.
	dialectMu sync.Mutex
	dialect   LocalDialect
}

// NewLocalProvider builds a provider for a local model server.
//
// baseURL is the server origin ("" uses DefaultLocalURL). model is the model to
// ask for, which for a local server is the name it was loaded under
// ("qwen2.5-coder:7b"); "" lets the server pick, which llama.cpp does by default
// and Ollama does not — LocalProvider asks Ollama for its first model in that
// case rather than failing.
func NewLocalProvider(baseURL, model string, dialect LocalDialect) *LocalProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultLocalURL
	}
	if dialect == "" {
		dialect = DialectAuto
	}
	return &LocalProvider{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		model:   strings.TrimSpace(model),
		dialect: dialect,
		// No overall timeout, same reasoning as Client: a generation is long-lived
		// and bounded by the caller's context plus the per-read watchdog below.
		httpc: &http.Client{
			// Redirects are not followed. A 307/308 re-sends the POST body, so a
			// server at the endpoint the user chose could forward the whole prompt to
			// a host of its choosing — the one thing this mode promises will not
			// happen. The redirect is surfaced as the response instead, which
			// statusError turns into an error naming the status, so a gateway that
			// needs a canonical URL is a configuration fix rather than a silent hop.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				// No Proxy. A local endpoint reached through a corporate proxy would
				// send the prompt off the machine, which is the one thing this mode
				// promises not to do.
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 2 * time.Minute,
				ExpectContinueTimeout: 1 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// BaseURL reports the endpoint, for status lines and error messages.
func (p *LocalProvider) BaseURL() string { return p.baseURL }

// Model reports the configured model name ("" when the server chooses).
func (p *LocalProvider) Model() string { return p.model }

// Dialect reports the configured or detected wire protocol.
func (p *LocalProvider) Dialect() LocalDialect {
	p.dialectMu.Lock()
	defer p.dialectMu.Unlock()
	return p.dialect
}

// ErrLocalUnavailable marks a failure to reach the local server, as opposed to a
// request the server rejected. The two need different advice — "start the
// server" versus "the server said no" — and callers should not have to match on
// message text to tell them apart.
var ErrLocalUnavailable = errors.New("local model server is not reachable")

// localUnavailable wraps a transport error with the endpoint and the fix.
func (p *LocalProvider) localUnavailable(err error) error {
	return fmt.Errorf("%w at %s: %v (start it, e.g. `ollama serve`, or set QEURO_LOCAL_URL)",
		ErrLocalUnavailable, p.baseURL, err)
}

// Chat streams a completion from the local server. It satisfies Provider, so
// every caller that already streams from the backend works unchanged.
func (p *LocalProvider) Chat(ctx context.Context, cr ChatRequest) (<-chan Event, error) {
	dialect, err := p.resolveDialect(ctx)
	if err != nil {
		return nil, err
	}
	model, err := p.resolveModel(ctx, cr, dialect)
	if err != nil {
		return nil, err
	}
	if dialect == DialectOpenAI {
		return p.chatOpenAI(ctx, cr, model)
	}
	return p.chatOllama(ctx, cr, model)
}

// resolveDialect probes the endpoint the first time it is needed.
//
// GET /api/tags is Ollama-only and answers 404 on llama.cpp, which makes it a
// cheap discriminator. A probe that fails to connect is reported as unavailable
// rather than guessed at: guessing would produce a confusing 404 from the wrong
// path instead of "the server is not running".
func (p *LocalProvider) resolveDialect(ctx context.Context) (LocalDialect, error) {
	p.dialectMu.Lock()
	defer p.dialectMu.Unlock()
	if p.dialect != DialectAuto {
		return p.dialect, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/tags", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		// Left as DialectAuto on purpose, so the next attempt probes again rather
		// than committing to a guess made while the server was down.
		return "", p.localUnavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		p.dialect = DialectOllama
	} else {
		// Anything else — 404 from llama.cpp, 401 from a gateway — means "not
		// Ollama", and the OpenAI shape is what every other local server speaks.
		p.dialect = DialectOpenAI
	}
	return p.dialect, nil
}

// resolveModel decides which model name to send.
//
// The configured local model wins. ChatRequest.Model deliberately does not:
// the TUI always puts its cloud-catalogue selection there (for example a Claude
// id), and forwarding that to Ollama would turn `qeuro --local` into "model not
// found" on every default session. With no local_model setting, ask Ollama what
// is installed; llama.cpp serves the weights it started with.
func (p *LocalProvider) resolveModel(ctx context.Context, _ ChatRequest, dialect LocalDialect) (string, error) {
	if p.model != "" {
		return p.model, nil
	}
	if dialect != DialectOllama {
		// llama.cpp serves whatever weights it was started with and ignores the
		// field; sending a placeholder keeps the JSON valid.
		return "local-model", nil
	}
	models, err := p.Models(ctx)
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", fmt.Errorf("no model is installed on %s: pull one first, e.g. `ollama pull qwen2.5-coder:7b`", p.baseURL)
	}
	return models[0].ID, nil
}

// Models lists what the local server has available. `qeuro --local` uses it to
// name the model in the status line and to fail with useful advice when the
// server has no weights at all.
func (p *LocalProvider) Models(ctx context.Context) ([]Model, error) {
	dialect, err := p.resolveDialect(ctx)
	if err != nil {
		return nil, err
	}
	path := "/v1/models"
	if dialect == DialectOllama {
		path = "/api/tags"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return nil, p.localUnavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, p.statusError(resp)
	}
	// Bounded read: a malformed or hostile endpoint must not be able to make the
	// CLI allocate without limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if dialect == DialectOllama {
		var out struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("local models: %w", err)
		}
		models := make([]Model, 0, len(out.Models))
		for _, m := range out.Models {
			id := m.Model
			if id == "" {
				id = m.Name
			}
			if id == "" {
				continue
			}
			models = append(models, Model{Brand: "local", ID: id, Label: id, Note: "local"})
		}
		return models, nil
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("local models: %w", err)
	}
	models := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, Model{Brand: "local", ID: m.ID, Label: m.ID, Note: "local"})
	}
	return models, nil
}

// statusError turns a non-200 into an error carrying the server's own message,
// truncated. The body is not sanitised here: this value reaches the terminal
// through callers that apply clientcfg.DisplaySafe, and doing it twice would
// double-escape the text.
func (p *LocalProvider) statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	msg := strings.TrimSpace(string(body))
	// Prefer the structured message both dialects use when they have one.
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		var s string
		if json.Unmarshal(envelope.Error, &s) == nil && s != "" {
			msg = s
		} else {
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Error, &obj) == nil && obj.Message != "" {
				msg = obj.Message
			}
		}
	}
	if msg == "" {
		msg = resp.Status
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return fmt.Errorf("local model server: %s: %s", resp.Status, msg)
}

// post sends a streaming request and hands back the body on success.
func (p *LocalProvider) post(ctx context.Context, path string, payload []byte, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	resp, err := p.httpc.Do(req)
	if err != nil {
		return nil, p.localUnavailable(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, p.statusError(resp)
	}
	return resp, nil
}

// ValidateLocalURL checks an endpoint before it is used.
//
// The scheme allowlist is the security control of this file. A `file://` or
// `unix://` URL would make the "endpoint" a local read, and a URL with
// credentials in it would put them in a request line; both are refused here so
// no later code has to consider them.
func ValidateLocalURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil // the default applies
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("local url %q is not a URL: %v", s, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return fmt.Errorf("local url %q needs a scheme, e.g. http://localhost:11434", s)
	default:
		return fmt.Errorf("local url %q must be http or https, not %q", s, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("local url %q has no host", s)
	}
	if u.User != nil {
		return fmt.Errorf("local url must not contain credentials")
	}
	return nil
}

type ollamaRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type ollamaChunk struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
	Error   string        `json:"error,omitempty"`
}

// chatOllama maps the shared request to Ollama's native streaming shape.
// Tools are forwarded rather than stripped: current Ollama releases support
// function calling, which lets a capable local coding model keep the CLI's
// inspect/edit/verify loop. A model that does not support tools returns a normal
// server error; it does not silently answer a task it could not execute.
func (p *LocalProvider) chatOllama(ctx context.Context, cr ChatRequest, model string) (<-chan Event, error) {
	messages := make([]ollamaMessage, 0, len(cr.Messages))
	for _, m := range cr.Messages {
		om := ollamaMessage{Role: m.Role, Content: m.Content}
		for _, tc := range m.ToolCalls {
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("local request: tool arguments for %s: %w", tc.Function.Name, err)
			}
			otc := ollamaToolCall{}
			otc.Function.Name = tc.Function.Name
			otc.Function.Arguments = args
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		// Ollama recognises only system/user/assistant/tool roles. The shared
		// Message.Name and ToolCallID are OpenAI metadata; the content is what
		// matters to its tool role.
		messages = append(messages, om)
	}
	reqBody := ollamaRequest{
		Model:    model,
		Messages: messages,
		Tools:    cr.Tools,
		Stream:   true,
	}
	if cr.MaxTokens > 0 {
		reqBody.Options = map[string]any{"num_predict": cr.MaxTokens}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, "/api/chat", payload, "application/x-ndjson")
	if err != nil {
		return nil, err
	}
	out := make(chan Event, 16)
	go p.parseOllama(ctx, resp, out, model)
	return out, nil
}

func (p *LocalProvider) parseOllama(ctx context.Context, resp *http.Response, out chan<- Event, model string) {
	defer close(out)
	defer func() { _ = resp.Body.Close() }()
	stopCtx, idle := closeOnIdle(ctx, resp.Body, localIdleTimeout)
	defer stopCtx()
	defer idle.Stop()

	if !sendEvent(ctx, out, Event{Kind: EventRoute, Route: &Route{
		Model: model, Effort: "local", Reason: "offline local model",
	}}) {
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		idle.Reset(localIdleTimeout)
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_invalid",
				ErrMsg: "local model returned invalid JSON: " + err.Error()})
			return
		}
		if chunk.Error != "" {
			sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_model_error", ErrMsg: chunk.Error})
			return
		}
		if chunk.Message.Content != "" && !sendEvent(ctx, out, Event{Kind: EventToken, Text: chunk.Message.Content}) {
			return
		}
		if len(chunk.Message.ToolCalls) > 0 {
			calls, err := fromOllamaToolCalls(chunk.Message.ToolCalls)
			if err != nil {
				sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_tool_call_invalid", ErrMsg: err.Error()})
				return
			}
			if !sendEvent(ctx, out, Event{Kind: EventToolCalls, ToolCalls: calls}) {
				return
			}
		}
		if chunk.Done {
			sendEvent(ctx, out, Event{Kind: EventDone})
			return
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_interrupted",
			ErrMsg: "local model stream was interrupted: " + err.Error()})
		return
	}
	// EOF without Ollama's done marker is not success: callers would otherwise
	// commit a truncated answer to history and possibly act on it.
	if ctx.Err() == nil {
		sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_incomplete",
			ErrMsg: "local model stream ended before completion"})
	}
}

func fromOllamaToolCalls(in []ollamaToolCall) ([]ToolCall, error) {
	out := make([]ToolCall, 0, len(in))
	for i, tc := range in {
		if strings.TrimSpace(tc.Function.Name) == "" {
			return nil, fmt.Errorf("local model returned a tool call without a name")
		}
		args, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("local model tool call %s: %w", tc.Function.Name, err)
		}
		out = append(out, ToolCall{
			// Ollama omits call ids. The CLI needs one to match the tool result to
			// the call on the next turn, so generate a stable id for this chunk.
			ID:   fmt.Sprintf("local-%d", i+1),
			Type: "function",
			Function: FunctionCall{
				Name: tc.Function.Name, Arguments: string(args),
			},
		})
	}
	return out, nil
}

// OpenAI-compatible request/stream types. Tool definitions and messages already
// use this shape in ChatRequest, so preserving them is mostly a direct marshal.
type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []Message       `json:"messages"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (p *LocalProvider) chatOpenAI(ctx context.Context, cr ChatRequest, model string) (<-chan Event, error) {
	payload, err := json.Marshal(openAIRequest{
		Model: model, Messages: cr.Messages, Tools: cr.Tools, Stream: true, MaxTokens: cr.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, "/v1/chat/completions", payload, "text/event-stream")
	if err != nil {
		return nil, err
	}
	out := make(chan Event, 16)
	go p.parseOpenAI(ctx, resp, out, model)
	return out, nil
}

// partialToolCall accumulates OpenAI's fragmented tool-call deltas.
type partialToolCall struct {
	id, typ, name, arguments string
}

func (p *LocalProvider) parseOpenAI(ctx context.Context, resp *http.Response, out chan<- Event, model string) {
	defer close(out)
	defer func() { _ = resp.Body.Close() }()
	stopCtx, idle := closeOnIdle(ctx, resp.Body, localIdleTimeout)
	defer stopCtx()
	defer idle.Stop()

	if !sendEvent(ctx, out, Event{Kind: EventRoute, Route: &Route{
		Model: model, Effort: "local", Reason: "offline local model",
	}}) {
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	partials := map[int]*partialToolCall{}
	done := false
	for scanner.Scan() {
		idle.Reset(localIdleTimeout)
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// The other SSE fields carry no payload for this protocol, and a
			// compliant server is free to send them (RFC 9110-era EventSource:
			// event/id/retry). Skipping them is required, not lenient — treating
			// `event: message` as a protocol error would break offline mode on a
			// server that is doing nothing wrong.
			if isSSEField(line) {
				continue
			}
			// Some local servers stream raw NDJSON despite advertising SSE.
			// Accept that useful subset; anything else is a protocol error.
			if line[0] != '{' {
				sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_invalid",
					ErrMsg: "local model returned an invalid SSE line"})
				return
			}
		} else {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			done = true
			break
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_invalid",
				ErrMsg: "local model returned invalid JSON: " + err.Error()})
			return
		}
		if chunk.Error != nil {
			sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_model_error", ErrMsg: chunk.Error.Message})
			return
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" && !sendEvent(ctx, out, Event{Kind: EventToken, Text: choice.Delta.Content}) {
				return
			}
			for _, delta := range choice.Delta.ToolCalls {
				part := partials[delta.Index]
				if part == nil {
					part = &partialToolCall{}
					partials[delta.Index] = part
				}
				part.id += delta.ID
				part.typ += delta.Type
				part.name += delta.Function.Name
				part.arguments += delta.Function.Arguments
			}
			if choice.FinishReason != nil {
				done = true
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_interrupted",
			ErrMsg: "local model stream was interrupted: " + err.Error()})
		return
	}
	if ctx.Err() != nil {
		return
	}
	if !done {
		sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_stream_incomplete",
			ErrMsg: "local model stream ended before completion"})
		return
	}
	if len(partials) > 0 {
		// Emit in index order, taken from the keys that actually arrived rather than
		// assumed to be 0..n-1: a server that numbers its calls from 1, or skips an
		// index, would otherwise report "incomplete tool call" for calls it sent in
		// full — turning a working local model into a broken-looking one.
		indices := make([]int, 0, len(partials))
		for i := range partials {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		calls := make([]ToolCall, 0, len(partials))
		for n, i := range indices {
			part := partials[i]
			if part.name == "" {
				sendEvent(ctx, out, Event{Kind: EventError, ErrCode: "local_tool_call_invalid",
					ErrMsg: "local model returned an incomplete tool call"})
				return
			}
			id := part.id
			if id == "" {
				// Numbered by position in the emitted slice, not by the server's index:
				// the id only has to be unique within this turn so the tool result can
				// be matched back, and a sparse index would leave gaps that look like
				// missing calls.
				id = fmt.Sprintf("local-%d", n+1)
			}
			typ := part.typ
			if typ == "" {
				typ = "function"
			}
			calls = append(calls, ToolCall{ID: id, Type: typ,
				Function: FunctionCall{Name: part.name, Arguments: part.arguments}})
		}
		if !sendEvent(ctx, out, Event{Kind: EventToolCalls, ToolCalls: calls}) {
			return
		}
	}
	sendEvent(ctx, out, Event{Kind: EventDone})
}

// isSSEField reports whether a line is one of the SSE fields this reader has no
// use for. `data:` is not listed because the only caller has already ruled it
// out; and only these three names are recognised, so an unknown line still fails
// as a protocol error rather than being skipped as if it were understood.
func isSSEField(line string) bool {
	for _, name := range [...]string{"event:", "id:", "retry:"} {
		if strings.HasPrefix(line, name) {
			return true
		}
	}
	return false
}

// closeOnIdle closes a stream when either its context is cancelled or no bytes
// arrive before timeout. Closing is what unblocks Scanner.Read.
func closeOnIdle(ctx context.Context, body io.Closer, timeout time.Duration) (func() bool, *time.Timer) {
	idle := time.AfterFunc(timeout, func() { _ = body.Close() })
	stopCtx := context.AfterFunc(ctx, func() { _ = body.Close() })
	return stopCtx, idle
}

func sendEvent(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
