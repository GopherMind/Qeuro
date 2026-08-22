package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Streamable HTTP transport (roadmap §4.8, increment 2).
//
// The stdio transport supervises a process; this one supervises nothing. One
// call is one POST carrying one JSON-RPC message, and the answer arrives either
// as a single JSON body or inside an SSE stream. That is why there is no read
// loop and no id-keyed pending map here: HTTP already pairs a response with the
// request that caused it.
//
// Four properties are security decisions rather than protocol details.
//
//   - The endpoint is fixed at construction and never taken from a response. A
//     cross-origin redirect is refused instead of followed, because the token
//     the user configured belongs to the origin they wrote down, and following
//     the server's choice of host is unrestricted egress (.ai/RULES.md:24).
//   - Cleartext http:// is refused unless the host is loopback: the token would
//     otherwise be readable on the wire, and so would every tool result.
//   - The bearer token is the only credential ever sent, it is read from a named
//     environment variable, and mcp.json holds the name rather than the value.
//     Nothing else from the CLI's environment is attached to the request, which
//     is the HTTP counterpart of BaseEnv's allow-list.
//   - Bodies are bounded. A remote server that streams without end would
//     otherwise be a memory-exhaustion vector reachable from the network.
const (
	// sessionHeader carries the server-assigned session id. It is echoed on every
	// later request and is the only piece of server-chosen data this transport
	// stores.
	sessionHeader = "Mcp-Session-Id"

	// protocolHeader duplicates _meta.protocolVersion at the HTTP layer, where a
	// proxy or gateway can route on it without parsing the body. A server that
	// finds the two disagreeing answers CodeHeaderMismatch; this client always
	// sends them from the same constant.
	protocolHeader = "MCP-Protocol-Version"

	// maxHTTPBodyBytes bounds one response, whether it arrives as a single JSON
	// body or as an SSE stream. Same value as the stdio line cap: the limit
	// belongs to the message, not to the transport carrying it.
	maxHTTPBodyBytes = maxLineBytes

	// maxRedirects bounds same-origin redirects. Servers do redirect /mcp to
	// /mcp/, so refusing every redirect would fail against real deployments.
	maxRedirects = 3

	// deleteTimeout bounds the best-effort session teardown in Close. Short on
	// purpose: exiting the CLI must not wait on a server that stopped answering.
	deleteTimeout = 3 * time.Second
)

// errSessionExpired is the internal signal that the server rejected our session
// id. It never reaches a caller: Call clears the session and retries once.
var errSessionExpired = errors.New("mcp: session expired")

// HTTPConfig describes a streamable HTTP server.
type HTTPConfig struct {
	// URL is the MCP endpoint. It is validated by parseServerURL before use.
	URL string

	// Bearer is the token for the Authorization header. Empty means the header is
	// omitted entirely rather than sent empty — a server that treats an empty
	// bearer as anonymous and one that treats it as malformed both then behave
	// predictably.
	Bearer string
}

// httpTransport is one connection-less session with a streamable HTTP server.
type httpTransport struct {
	endpoint string
	origin   string // scheme://host[:port], for the redirect check
	bearer   string
	httpc    *http.Client
	diag     *boundedBuffer

	mu      sync.Mutex
	nextID  int64
	session string
	closed  bool
}

// StartHTTP validates the endpoint and returns a transport for it. Nothing is
// contacted here: "the server is unreachable" is a call failure, and making it a
// startup failure would mean one unreachable server costs the user the CLI.
func StartHTTP(cfg HTTPConfig) (Transport, error) {
	u, err := parseServerURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	t := &httpTransport{
		endpoint: u.String(),
		origin:   u.Scheme + "://" + u.Host,
		// Trimmed, and whitespace-only counts as absent: a variable set to an empty
		// or blank value would otherwise be sent as "Bearer    ", which a server
		// rejects as malformed rather than treating as anonymous — and the 401 would
		// then say the configured token was rejected when none was configured.
		bearer: strings.TrimSpace(cfg.Bearer),
		diag:   &boundedBuffer{limit: stderrLimit},
	}
	t.httpc = &http.Client{
		// No overall timeout: an SSE response is long-lived and every call is
		// already bounded by the caller's context, the same split internal/client
		// uses for the backend stream.
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: t.checkRedirect,
	}
	return t, nil
}

// checkRedirect allows a same-origin redirect and refuses everything else.
//
// Go already strips the Authorization header on a cross-host redirect, so this
// is not only about the token: following a redirect means the tool result the
// model receives came from a host the user never configured, and no later check
// can tell that it did.
func (t *httpTransport) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("mcp: %s redirected more than %d times", t.origin, maxRedirects)
	}
	if got := req.URL.Scheme + "://" + req.URL.Host; got != t.origin {
		t.note("refused redirect to " + got)
		return fmt.Errorf("mcp: %s redirected to %s, which is a different origin; "+
			"configure the final URL in %s instead", t.origin, got, ConfigFileName)
	}
	return nil
}

// parseServerURL validates a configured endpoint. It is exported through
// config validation as well, so a bad URL is reported when the file is read
// rather than on the first call.
func parseServerURL(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("mcp: server url is empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("mcp: url %q is unparseable: %w", sanitizeOneLine(s), err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		// A loopback server is a local process reached over a socket instead of a
		// pipe. TLS there protects nothing that the process boundary does not.
	case u.Scheme == "http":
		return nil, fmt.Errorf("mcp: url %q uses cleartext http to a non-loopback host; "+
			"the token and every tool result would travel unencrypted", sanitizeOneLine(s))
	default:
		return nil, fmt.Errorf("mcp: url %q must use https (or http on loopback), not %q",
			sanitizeOneLine(s), sanitizeOneLine(u.Scheme))
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mcp: url %q has no host", sanitizeOneLine(s))
	}
	if u.User != nil {
		// Credentials in a URL end up in shell history, process listings and any
		// log that records the endpoint. authFrom exists for exactly this.
		return nil, fmt.Errorf("mcp: url for %s must not embed credentials; use authFrom", sanitizeOneLine(u.Host))
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("mcp: url %q has a fragment, which is not sent to the server", sanitizeOneLine(s))
	}
	return u, nil
}

// isLoopbackHost reports whether a host is unambiguously this machine. A name
// that merely resolves to a loopback address does not count: resolution can
// change between the check and the connection.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Call sends one request and returns its result.
//
// Unlike the stdio transport, no notifications/cancelled is sent when the context
// ends. There it is necessary: the pipe stays open, so the server has no other way
// to learn that nobody is waiting. Here cancellation aborts the request and closes
// the connection, which the server observes directly — and a cancellation
// notification would have to be a *second* POST to an endpoint we just gave up on,
// on a context that is already done.
func (t *httpTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	payload, err := withMeta(params)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode params: %w", err)
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	t.nextID++
	id := t.nextID
	t.mu.Unlock()

	body, err := json.Marshal(request{JSONRPC: "2.0", ID: &id, Method: method, Params: payload})
	if err != nil {
		return nil, fmt.Errorf("mcp: encode request: %w", err)
	}

	raw, err := t.roundTrip(ctx, body, id)
	if errors.Is(err, errSessionExpired) {
		// The server forgot the session. One retry without the stale id is the
		// specified recovery, and it is exactly one: the retry sends no session, so
		// a 404 to it is classified as a wrong endpoint rather than as another
		// expiry, and there is nothing here that can retry again.
		t.setSession("")
		t.note("session rejected by the server; retried once without it")
		raw, err = t.roundTrip(ctx, body, id)
	}
	return raw, err
}

// roundTrip performs one POST and extracts the response with the wanted id.
func (t *httpTransport) roundTrip(ctx context.Context, body []byte, wantID int64) (json.RawMessage, error) {
	// The session that goes on this request is read once, here, and the same value
	// decides how a 404 is classified. Re-reading it after the response would ask
	// the wrong question: a server may issue a new session id on the very response
	// that rejects one, and "do we hold a session now" would then be true even for
	// a request that carried none — turning a wrong endpoint into an endless
	// sequence of session retries.
	sent := t.currentSession()

	req, err := t.newRequest(ctx, http.MethodPost, body, sent)
	if err != nil {
		return nil, err
	}
	resp, err := t.httpc.Do(req)
	if err != nil {
		return nil, t.requestError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if id := resp.Header.Get(sessionHeader); id != "" {
		t.setSession(id)
	}
	if err := t.checkStatus(resp, sent); err != nil {
		return nil, err
	}

	kind := mediaType(resp.Header.Get("Content-Type"))
	switch kind {
	case "application/json":
		return t.readJSONResponse(resp.Body, wantID)
	case "text/event-stream":
		return t.readSSEResponse(ctx, resp.Body, wantID)
	default:
		return nil, fmt.Errorf("mcp: %s answered with content type %q, which is neither application/json nor text/event-stream",
			t.origin, sanitizeOneLine(kind))
	}
}

// checkStatus turns a non-2xx status into a named failure. Each case is a
// distinct operator problem, and collapsing them into "HTTP 4xx" is what makes
// a misconfigured token look like a broken server.
//
// sentSession is the session id this request actually carried, not whatever is
// stored now: only a request that presented a session can have had one rejected.
func (t *httpTransport) checkStatus(resp *http.Response, sentSession string) error {
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode == http.StatusNotFound && sentSession != "":
		return errSessionExpired
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		t.note(fmt.Sprintf("HTTP %d from %s", resp.StatusCode, t.origin))
		hint := "no token was sent: set authFrom in " + ConfigFileName
		if t.bearer != "" {
			hint = "the configured token was rejected"
		}
		return fmt.Errorf("mcp: %s refused the request with HTTP %d — %s", t.origin, resp.StatusCode, hint)
	default:
		snippet := readSnippet(resp.Body)
		t.note(fmt.Sprintf("HTTP %d from %s: %s", resp.StatusCode, t.origin, snippet))
		if snippet == "" {
			return fmt.Errorf("mcp: %s answered HTTP %d", t.origin, resp.StatusCode)
		}
		return fmt.Errorf("mcp: %s answered HTTP %d: %s", t.origin, resp.StatusCode, snippet)
	}
}

// readJSONResponse decodes a single JSON-RPC response body.
func (t *httpTransport) readJSONResponse(body io.Reader, wantID int64) (json.RawMessage, error) {
	raw, truncated, err := readBounded(body, maxHTTPBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("mcp: reading the response from %s: %w", t.origin, err)
	}
	if truncated {
		return nil, fmt.Errorf("mcp: %s sent a response larger than %d bytes", t.origin, maxHTTPBodyBytes)
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mcp: %s sent an unreadable JSON-RPC response: %w", t.origin, err)
	}
	return t.resultOf(resp, wantID)
}

// readSSEResponse reads the stream until the response with the wanted id
// arrives. Other messages are counted and skipped: a server may interleave its
// own notifications, and this client implements none of them, so being unable to
// find our own answer is the only failure that matters.
func (t *httpTransport) readSSEResponse(ctx context.Context, body io.Reader, wantID int64) (json.RawMessage, error) {
	// Closing the body is what unblocks a Read on cancellation; the http package
	// does it for the request context, and this covers a body that outlives it.
	stop := context.AfterFunc(ctx, func() {
		if c, ok := body.(io.Closer); ok {
			_ = c.Close()
		}
	})
	defer stop()

	counted := &countingReader{r: body, limit: maxHTTPBodyBytes}
	sc := bufio.NewScanner(counted)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	var data []string
	skipped := 0
	flush := func() (json.RawMessage, bool, error) {
		if len(data) == 0 {
			return nil, false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		var resp response
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			// A comment, a keep-alive or a non-MCP event. Not fatal for the same
			// reason a junk stdout line is not: it cannot be the answer we want.
			skipped++
			return nil, false, nil
		}
		if resp.ID == nil || *resp.ID != wantID {
			skipped++
			return nil, false, nil
		}
		raw, err := t.resultOf(resp, wantID)
		return raw, true, err
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			raw, done, err := flush()
			if done || err != nil {
				return raw, err
			}
		case strings.HasPrefix(line, ":"):
			// An SSE comment, which is how servers keep the connection alive.
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// event:, id:, retry: and anything else. The revision uses the default
			// message event, so the names carry nothing this client needs.
		}
	}
	if raw, done, err := flush(); done || err != nil {
		return raw, err
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mcp: reading the event stream from %s: %w", t.origin, err)
	}
	if counted.exceeded {
		return nil, fmt.Errorf("mcp: %s streamed more than %d bytes without answering", t.origin, maxHTTPBodyBytes)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("mcp: %s closed the event stream after %d other message(s) without answering the request",
		t.origin, skipped)
}

// resultOf validates one response and returns its result.
//
// Error takes precedence over result when a malformed server sends both. That is
// the safe direction: treating a response that carries an error as successful
// would hand the model a partial result the server itself flagged as invalid.
func (t *httpTransport) resultOf(resp response, wantID int64) (json.RawMessage, error) {
	if resp.Error != nil {
		return nil, resp.Error
	}
	if resp.ID == nil || *resp.ID != wantID {
		return nil, fmt.Errorf("mcp: %s answered a different request than the one sent", t.origin)
	}
	if len(resp.Result) == 0 {
		return nil, fmt.Errorf("mcp: %s answered with neither a result nor an error", t.origin)
	}
	return resp.Result, nil
}

// Notify sends a notification. A notification has no id, so there is nothing to
// read back: the specified answer is 202 with an empty body.
func (t *httpTransport) Notify(ctx context.Context, method string, params any) error {
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
	body, err := json.Marshal(request{JSONRPC: "2.0", Method: method, Params: payload})
	if err != nil {
		return fmt.Errorf("mcp: encode request: %w", err)
	}
	req, err := t.newRequest(ctx, http.MethodPost, body, t.currentSession())
	if err != nil {
		return err
	}
	resp, err := t.httpc.Do(req)
	if err != nil {
		return t.requestError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _, _ = readBounded(resp.Body, 4<<10) // drain so the connection can be reused
	return nil
}

// newRequest builds one POST to the fixed endpoint.
//
// The header set is short and deliberate. Accept names both response shapes
// because the server chooses; the protocol version is duplicated from the same
// constant that goes into _meta; the session id is passed in by the caller rather
// than read here, so the value sent and the value used to classify the response
// are the same one; and the Authorization header is the single credential
// attached. Nothing is copied from the CLI's environment.
func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte, session string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, t.endpoint, rdr)
	if err != nil {
		return nil, fmt.Errorf("mcp: build request for %s: %w", t.origin, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(protocolHeader, ProtocolVersion)
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	return req, nil
}

// requestError converts a transport failure into a message that names the cause
// without repeating the URL's query string, which can carry a server-side token.
func (t *httpTransport) requestError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return fmt.Errorf("mcp: %s: %w", t.origin, uerr.Err)
	}
	return fmt.Errorf("mcp: %s: %w", t.origin, err)
}

// Close ends the session. The DELETE is best-effort and short: the specification
// defines it as how a client releases server-side state, but a server that
// ignores or hangs on it must not delay the CLI's exit.
func (t *httpTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	session := t.session
	t.mu.Unlock()

	if session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()
		// Built here rather than through newRequest: this one carries no body and no
		// Accept, and closed is already set, so it is the single request that runs
		// after Close began.
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil); err == nil {
			req.Header.Set(protocolHeader, ProtocolVersion)
			req.Header.Set(sessionHeader, session)
			if t.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+t.bearer)
			}
			if resp, err := t.httpc.Do(req); err == nil {
				_, _, _ = readBounded(resp.Body, 4<<10)
				_ = resp.Body.Close()
			}
		}
	}
	if tr, ok := t.httpc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// Diagnostics returns what went wrong at the HTTP layer.
//
// stdio has the server's stderr; HTTP has nothing equivalent, and "EOF" with no
// context is the failure mode that makes a broken endpoint undebuggable. So the
// statuses and refusals this transport saw are kept, bounded, and shown the same
// way a server log would be.
func (t *httpTransport) Diagnostics() string { return t.diag.String() }

// note records one bounded diagnostic line.
func (t *httpTransport) note(msg string) {
	_, _ = t.diag.Write([]byte(sanitizeOneLine(msg) + "\n"))
}

func (t *httpTransport) currentSession() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session
}

func (t *httpTransport) setSession(id string) {
	// The id is server-chosen and goes back out as a header value, so anything
	// that could forge a header or a log line is removed rather than trusted.
	clean := sanitizeHeaderValue(id)
	t.mu.Lock()
	t.session = clean
	t.mu.Unlock()
}

// sanitizeHeaderValue keeps only printable ASCII, which is what a header value
// may contain. A session id with a newline in it would let a server inject a
// second header into every later request (.ai/RULES.md:22).
func sanitizeHeaderValue(s string) string {
	const max = 512
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < max; i++ {
		if c := s[i]; c >= 0x21 && c <= 0x7e {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// mediaType strips parameters from a Content-Type value and lower-cases it.
func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// readBounded reads at most limit bytes and reports whether more was available.
func readBounded(r io.Reader, limit int) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}

// readSnippet returns a short, single-line excerpt of an error body, for the
// message. It is third-party text, so it is sanitised and truncated.
func readSnippet(r io.Reader) string {
	b, _, err := readBounded(r, 512)
	if err != nil {
		return ""
	}
	return sanitizeOneLine(strings.TrimSpace(string(b)))
}

// countingReader stops after limit bytes so an endless stream cannot exhaust
// memory. exceeded distinguishes "the server stopped" from "we stopped it",
// which is the difference between two error messages the operator acts on
// differently.
type countingReader struct {
	r        io.Reader
	limit    int
	read     int
	exceeded bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.read >= c.limit {
		c.exceeded = true
		return 0, io.EOF
	}
	if room := c.limit - c.read; len(p) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}
