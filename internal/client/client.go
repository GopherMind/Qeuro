// Package client is the CLI's HTTP/SSE client for the Qeuro backend. It
// speaks the contract from backend.txt §3: a streaming POST /v1/chat plus the
// JSON GET /v1/me and GET /v1/models endpoints. Only the standard library is
// used.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to one backend instance with one bearer token.
type Client struct {
	baseURL string
	token   string
	// tokenFn supplies the bearer token on first use instead of at construction.
	// It exists for the startup path: the TUI builds its client before drawing
	// the first frame, and on Linux reading the token means a D-Bus round trip to
	// the OS keychain, so resolving it eagerly puts a wait in front of the prompt
	// (roadmap §8 "Startup"). When nil, token is used as-is.
	tokenFn func() string
	// tokenOnce guards the resolution above. A Client is shared across the TUI's
	// commands, which Bubble Tea runs in separate goroutines, so two concurrent
	// requests could otherwise resolve at once.
	tokenOnce sync.Mutex
	httpc     *http.Client
}

// idleReadTimeout bounds how long the SSE reader waits for the next byte from
// the backend before giving up (M10). A hung/half-open upstream would otherwise
// block parseSSE forever; a live stream sends tokens well within this window.
const idleReadTimeout = 90 * time.Second

// New builds a client. baseURL has no trailing slash requirement.
func New(baseURL, token string) *Client {
	return newClient(baseURL, token, nil)
}

// NewLazy builds a client that asks tokenFn for the bearer token the first time
// it makes a request, rather than at construction. Callers on the startup path
// use this so a keychain read happens when the user's first request needs it, not
// before the prompt is drawn.
func NewLazy(baseURL string, tokenFn func() string) *Client {
	return newClient(baseURL, "", tokenFn)
}

func newClient(baseURL, token string, tokenFn func() string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		tokenFn: tokenFn,
		// No overall client timeout: chat streams are long-lived and bounded by
		// the caller's context. We DO bound connection setup and the wait for
		// response headers, and apply a per-read idle watchdog in parseSSE, so a
		// stalled backend cannot wedge the CLI indefinitely (M10).
		httpc: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// Message is one chat message in a request. It can carry assistant tool-call
// requests, or a tool-role result (ToolCallID + Name + Content).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name + JSON-string arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest mirrors the backend POST /v1/chat body.
type ChatRequest struct {
	ProjectID   string          `json:"project_id,omitempty"`
	ProjectName string          `json:"project_name,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Mode        string          `json:"mode"`
	Model       string          `json:"model,omitempty"`
	Effort      string          `json:"effort,omitempty"`
	OutputMode  string          `json:"output_mode,omitempty"`
	Messages    []Message       `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	// Providers carries the AI provider credentials linked on the web console
	// (Console -> Providers). The backend routes matching models through them,
	// exactly like chat from the web console does (M7).
	Providers []ProviderConfig `json:"providers,omitempty"`
}

// Route is the payload of the SSE "route" event.
type Route struct {
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Reason    string `json:"reason"`
	Escalated bool   `json:"escalated"`
}

// Usage is the payload of the SSE "usage" event.
type Usage struct {
	In                int     `json:"in"`
	Out               int     `json:"out"`
	CachedInputTokens int     `json:"cached_input_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd"`
	Credits           float64 `json:"credits"`
	SavedUSD          float64 `json:"saved_usd"`
	Balance           float64 `json:"balance"`
}

// EventKind enumerates SSE event types.
type EventKind string

const (
	EventRoute     EventKind = "route"
	EventToken     EventKind = "token"
	EventToolCalls EventKind = "tool_calls"
	EventUsage     EventKind = "usage"
	EventError     EventKind = "error"
	EventDone      EventKind = "done"
)

// Event is one decoded SSE event handed to the caller.
type Event struct {
	Kind      EventKind
	Route     *Route
	Text      string
	ToolCalls []ToolCall
	Usage     *Usage
	ErrCode   string
	ErrMsg    string
	Retryable bool
}

// MeResponse is the GET /v1/me body.
type MeResponse struct {
	Tier           string     `json:"tier"`
	CreditsBalance float64    `json:"credits_balance"`
	CreditsTotal   float64    `json:"credits_total"`
	PeriodEnd      *time.Time `json:"period_end"`
	SavedUSDMonth  float64    `json:"saved_usd_month"`
}

// ProviderModel is one model exposed by a linked AI provider.
type ProviderModel struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Efforts []string `json:"efforts,omitempty"`
}

// ProviderConfig is one AI provider credential managed on the web console
// (Providers page). GET {console}/api/cli/providers returns the caller's
// list; the CLI forwards it verbatim in ChatRequest.Providers so both
// surfaces stay linked to the same records (M7).
type ProviderConfig struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Kind     string            `json:"kind"`
	Compat   string            `json:"compat,omitempty"`
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Models   []ProviderModel   `json:"models,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Enabled  bool              `json:"enabled"`
}

// ConsoleProviders fetches the provider credentials linked on the web console
// using the CLI token. consoleURL is the console origin (Config.ConsoleURL).
func (c *Client) ConsoleProviders(ctx context.Context, consoleURL string) ([]ProviderConfig, error) {
	base := strings.TrimRight(strings.TrimSpace(consoleURL), "/")
	if base == "" {
		return nil, fmt.Errorf("console url is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/cli/providers", nil)
	if err != nil {
		return nil, err
	}
	// bearer(), not c.token: this endpoint is on a different origin so it does
	// not go through newRequest, and reading the field directly would send an
	// empty header on a lazily-constructed client.
	req.Header.Set("Authorization", "Bearer "+c.bearer())
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("console providers: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Providers []ProviderConfig `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("console providers: decode: %w", err)
	}
	return out.Providers, nil
}

// Model is one entry of GET /v1/models.
type Model struct {
	Brand   string   `json:"brand"`
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Note    string   `json:"note"`
	Efforts []string `json:"efforts"`
}

// apiError is the backend's JSON error envelope.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if tok := c.bearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req, nil
}

// bearer returns the token, resolving it through tokenFn on first use.
//
// The result is memoised in c.token: a turn makes several requests, and each one
// paying for a keychain round trip would move the cost off the startup path only
// to spread it over the session. Config.Secret already memoises, so this is belt
// and braces for callers that pass some other function.
func (c *Client) bearer() string {
	c.tokenOnce.Lock()
	defer c.tokenOnce.Unlock()
	if c.tokenFn != nil {
		c.token = c.tokenFn()
		c.tokenFn = nil
	}
	return c.token
}

// Me fetches the account summary.
func (c *Client) Me(ctx context.Context) (*MeResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var me MeResponse
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, err
	}
	return &me, nil
}

// StarBonusResponse is the POST /v1/star-bonus result.
type StarBonusResponse struct {
	Granted        bool    `json:"granted"`
	Credits        float64 `json:"credits"`
	CreditsBalance float64 `json:"credits_balance"`
	Repo           string  `json:"repo"`
	Message        string  `json:"message"`
}

// StarBonus claims the GitHub star bonus for the given GitHub username.
func (c *Client) StarBonus(ctx context.Context, githubUser string) (*StarBonusResponse, error) {
	body, err := json.Marshal(map[string]string{"github_user": githubUser})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/star-bonus", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out StarBonusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ModelsResult is a conditional catalogue fetch.
//
// NotModified is separate from an empty Models on purpose: the two are opposite
// instructions to the caller. "Unchanged" means keep the cached catalogue; an
// empty list would mean replace it with nothing, which is how a cache gets erased
// by a healthy server.
type ModelsResult struct {
	Models      []Model
	ETag        string
	NotModified bool
}

// ModelsWithETag fetches the catalogue, revalidating against a stored validator.
//
// Roadmap §8 "Startup" wants the catalogue cached on disk so nothing has to be
// fetched before the prompt appears; that only pays off if a later refresh can
// confirm the cache cheaply, which is what the conditional request buys.
//
// etag is the validator held with the cached copy, or "" when nothing is cached.
func (c *Client) ModelsWithETag(ctx context.Context, etag string) (ModelsResult, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return ModelsResult{}, err
	}
	if etag != "" {
		// Only when there is something to revalidate. `If-None-Match: ""` is a
		// syntactically valid entity-tag rather than an absent header, and a server
		// could answer 304 to it — which would report "unchanged" for a cache that
		// does not exist.
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return ModelsResult{}, err
	}
	defer resp.Body.Close()

	served := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		if served == "" {
			// RFC 9110 §15.4.5 asks for the validator on a 304, but a server that
			// omits it must not cost us the one we already hold: blanking it would
			// make every later refresh unconditional, degrading the cache to no cache.
			served = etag
		}
		return ModelsResult{ETag: served, NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		// Not "unchanged": a revoked token answers 401, and treating that as a cache
		// hit would render a stale catalogue and never mention the token stopped
		// working.
		return ModelsResult{}, decodeError(resp)
	}
	var models []Model
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return ModelsResult{}, err
	}
	return ModelsResult{Models: models, ETag: served}, nil
}

// Models fetches the model catalogue unconditionally.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	res, err := c.ModelsWithETag(ctx, "")
	if err != nil {
		return nil, err
	}
	return res.Models, nil
}

// UsageTotals is the whole-window aggregate of GET /v1/usage.
type UsageTotals struct {
	Requests  int     `json:"requests"`
	InTokens  int64   `json:"in_tokens"`
	OutTokens int64   `json:"out_tokens"`
	CostUSD   float64 `json:"cost_usd"`
	Credits   float64 `json:"credits"`
	SavedUSD  float64 `json:"saved_usd"`
}

// UsageModel is one model's share of the window.
type UsageModel struct {
	Model     string  `json:"model"`
	Requests  int     `json:"requests"`
	InTokens  int64   `json:"in_tokens"`
	OutTokens int64   `json:"out_tokens"`
	CostUSD   float64 `json:"cost_usd"`
	Credits   float64 `json:"credits"`
	SavedUSD  float64 `json:"saved_usd"`
}

// UsageDay is one UTC day of the window.
type UsageDay struct {
	Day      string  `json:"day"`
	Requests int     `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
	Credits  float64 `json:"credits"`
}

// UsageResponse is the GET /v1/usage body: the caller's own spend, three ways.
type UsageResponse struct {
	Days           int          `json:"days"`
	Since          time.Time    `json:"since"`
	Totals         UsageTotals  `json:"totals"`
	Models         []UsageModel `json:"models"`
	Series         []UsageDay   `json:"series"`
	CreditsBalance float64      `json:"credits_balance"`
}

// Usage fetches the caller's spend over the last `days` days. The server clamps
// the window, so an out-of-range value is corrected rather than rejected; the
// response carries the window it actually used.
func (c *Client) Usage(ctx context.Context, days int) (*UsageResponse, error) {
	path := "/v1/usage"
	if days > 0 {
		path += "?days=" + strconv.Itoa(days)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeToken invalidates the bearer token currently configured for this
// client. `qeuro logout` calls it before clearing local config, so a copied
// token stops working server-side too.
func (c *Client) RevokeToken(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/token/revoke", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	return nil
}

// Chat opens a streaming completion. It returns a channel of events that the
// caller must drain; the channel is closed when the stream ends (after a done
// or error event) or the context is cancelled. A non-nil error means the
// request failed before streaming began.
func (c *Client) Chat(ctx context.Context, cr ChatRequest) (<-chan Event, error) {
	payload, err := json.Marshal(cr)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}

	out := make(chan Event, 16)
	go parseSSE(ctx, resp, out)
	return out, nil
}

// parseSSE reads the event stream and forwards decoded events. It is bounded
// two ways (M10): the caller's ctx (cancel/Esc) and a per-read idle watchdog —
// if no byte arrives within idleReadTimeout the response body is closed, which
// unblocks the scanner with an error instead of hanging forever.
func parseSSE(ctx context.Context, resp *http.Response, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	// Idle watchdog: closing the body makes the in-flight Read return, so a
	// stalled upstream cannot block the scanner indefinitely. Each successful
	// scan resets the timer; ctx cancellation also trips it.
	idle := time.AfterFunc(idleReadTimeout, func() { _ = resp.Body.Close() })
	defer idle.Stop()
	stopCtx := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
	defer stopCtx()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventName string
	var dataLines []string
	emit := func() bool {
		if eventName == "" {
			return true
		}
		data := strings.Join(dataLines, "\n")
		ev := decodeEvent(eventName, data)
		eventName, dataLines = "", nil
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for scanner.Scan() {
		idle.Reset(idleReadTimeout)
		line := scanner.Text()
		switch {
		case line == "":
			if !emit() {
				return
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			dataLines = append(dataLines, value)
		}
	}
	// Flush any trailing event without a terminating blank line. If the scanner
	// stopped because the body was closed (idle/cancel), surface it as an error
	// event so the UI ends the turn instead of treating it as a clean finish.
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		select {
		case out <- Event{Kind: EventError, ErrCode: "stream_timeout",
			ErrMsg: "connection interrupted: no data from server"}:
		default:
		}
		return
	}
	emit()
}

// decodeEvent turns a raw SSE event into a typed Event. A malformed payload is
// reported as an error event rather than being silently dropped (M11) — a lost
// tool_calls payload would otherwise make the agent treat a tool turn as a final
// answer. Only the explicitly-known "done" event ends the stream cleanly; any
// other unknown event name is surfaced as an error instead of being mistaken for
// done.
func decodeEvent(name, data string) Event {
	switch EventKind(name) {
	case EventRoute:
		var r Route
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return decodeErr(name, err)
		}
		return Event{Kind: EventRoute, Route: &r}
	case EventToken:
		var t struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return decodeErr(name, err)
		}
		return Event{Kind: EventToken, Text: t.Text}
	case EventToolCalls:
		var tc struct {
			ToolCalls []ToolCall `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(data), &tc); err != nil {
			return decodeErr(name, err)
		}
		return Event{Kind: EventToolCalls, ToolCalls: tc.ToolCalls}
	case EventUsage:
		var u Usage
		if err := json.Unmarshal([]byte(data), &u); err != nil {
			return decodeErr(name, err)
		}
		return Event{Kind: EventUsage, Usage: &u}
	case EventError:
		var e struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return decodeErr(name, err)
		}
		return Event{Kind: EventError, ErrCode: e.Code, ErrMsg: e.Message, Retryable: e.Retryable}
	case EventDone:
		return Event{Kind: EventDone}
	default:
		return Event{Kind: EventError, ErrCode: "bad_event",
			ErrMsg: "unknown server event: " + name}
	}
}

// decodeErr wraps a malformed SSE payload as an error event.
func decodeErr(name string, err error) Event {
	return Event{Kind: EventError, ErrCode: "bad_event",
		ErrMsg: fmt.Sprintf("could not parse event %q: %v", name, err)}
}

// decodeError reads a JSON error envelope from a non-200 response. It drains any
// remainder of the body before returning so the connection can be put back in
// the keep-alive pool instead of being torn down (L9).
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	_, _ = io.Copy(io.Discard, resp.Body) // drain the rest for connection reuse
	var ae apiError
	if json.Unmarshal(body, &ae) == nil && ae.Error.Message != "" {
		return fmt.Errorf("%s (%s)", ae.Error.Message, ae.Error.Code)
	}
	return fmt.Errorf("backend returned %s", resp.Status)
}
