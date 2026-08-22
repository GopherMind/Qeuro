package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/catalog"
	"qeuro/internal/client"
	"qeuro/internal/state"
)

// Roadmap §8 "Startup" asks for a cached catalogue with an ETag and no network
// calls at startup. Those two pull against each other — a catalogue fetched from
// the backend has to be fetched at some point — so the resolution has to be tested
// rather than asserted: the fetch is a tea.Cmd, which Bubble Tea runs after Init
// returns and the first frame is drawn.

func isolateCatalogCache(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("AppData", d)
	t.Setenv("XDG_CONFIG_HOME", d)
}

// catalogServer serves a two-model catalogue and honours If-None-Match.
func catalogServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	const etag = `"sha256:catalogue-one"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` +
			`{"brand":"anthropic","id":"anthropic/claude-opus-4.8","label":"Opus 4.8",` +
			`"note":"architecture","efforts":["low","medium","high","xhigh"]},` +
			`{"brand":"brand-new","id":"brand-new/model-1","label":"Model 1",` +
			`"note":"added after this build","efforts":["medium"]}]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The headline requirement. Init must not perform the fetch itself; it may only
// return a command that does. Counting requests made *by Init* is the only way to
// tell those apart — a test that ran the commands would pass either way.
func TestInitDoesNotFetchTheCatalogue(t *testing.T) {
	isolateCatalogCache(t)
	hits := 0
	srv := catalogServer(t, &hits)

	m := model{
		app: state.New(), width: 80, version: "test",
		loggedIn: true,
		baseURL:  srv.URL,
		cli:      client.New(srv.URL, "tok"),
	}
	_ = m.Init()

	if hits != 0 {
		t.Errorf("Init made %d catalogue requests; §8 requires none before the prompt", hits)
	}
}

// Laziness must not have become "never happens": the refresh has to actually be
// scheduled, or the cache would only ever hold what the first manual command put
// there.
func TestInitSchedulesTheCatalogueRefresh(t *testing.T) {
	isolateCatalogCache(t)
	hits := 0
	srv := catalogServer(t, &hits)

	m := model{
		app: state.New(), width: 80, version: "test",
		loggedIn: true,
		baseURL:  srv.URL,
		cli:      client.New(srv.URL, "tok"),
	}
	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init message = %T, want tea.BatchMsg", m.Init()())
	}

	var got *catalogMsg
	for _, cmd := range batch {
		if cmd == nil {
			continue
		}
		if msg, ok := cmd().(catalogMsg); ok {
			got = &msg
			break
		}
	}
	if got == nil {
		t.Fatal("Init scheduled no catalogue refresh, so the cache would never fill")
	}
	if got.err != nil {
		t.Fatalf("refresh failed: %v", got.err)
	}
	if !got.quiet {
		t.Error("the startup refresh must be quiet; a backend outage is not news mid-typing")
	}
	if hits == 0 {
		t.Error("the scheduled command made no request")
	}
}

// A signed-out CLI has no token, so there is nothing to authenticate with and no
// refresh to schedule. It must still start.
func TestSignedOutSessionSchedulesNoRefresh(t *testing.T) {
	isolateCatalogCache(t)
	hits := 0
	srv := catalogServer(t, &hits)

	m := model{app: state.New(), width: 80, version: "test", baseURL: srv.URL}
	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init message = %T, want tea.BatchMsg", m.Init()())
	}
	for _, cmd := range batch {
		if cmd == nil {
			continue
		}
		if _, isCatalog := cmd().(catalogMsg); isCatalog {
			t.Fatal("a signed-out session scheduled a catalogue refresh")
		}
	}
	if hits != 0 {
		t.Errorf("a signed-out session made %d requests", hits)
	}
}

// The refresh has to reach the selector, or the cache is decoration: a model the
// backend added after this binary was built must become selectable.
func TestRefreshMakesNewModelsSelectable(t *testing.T) {
	isolateCatalogCache(t)
	srv := catalogServer(t, nil)

	if _, _, ok := catalog.FindModel("brand-new/model-1"); ok {
		t.Fatal("the test model is already in the compiled-in catalogue; pick another")
	}

	msg := catalogCmd(client.New(srv.URL, "tok"), false)()
	if cm, ok := msg.(catalogMsg); !ok || cm.err != nil {
		t.Fatalf("refresh failed: %+v", msg)
	}

	if _, _, ok := catalog.FindModel("brand-new/model-1"); !ok {
		t.Error("a model added by the backend is still not selectable after a refresh")
	}
	labels := make([]string, 0, len(brandItems()))
	for _, it := range brandItems() {
		labels = append(labels, it.label)
	}
	if !strings.Contains(strings.Join(labels, " "), "brand-new") {
		t.Errorf("brand selector = %v, want the refreshed catalogue's brands", labels)
	}
}

// A second refresh revalidates instead of re-downloading — the reason for storing
// the ETag at all.
func TestSecondRefreshRevalidates(t *testing.T) {
	isolateCatalogCache(t)
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const etag = `"sha256:catalogue-one"`
		sent = append(sent, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"brand":"anthropic","id":"anthropic/claude-opus-4.8",` +
			`"label":"Opus 4.8","note":"n","efforts":["high"]}]`))
	}))
	t.Cleanup(srv.Close)

	cli := client.New(srv.URL, "tok")
	if msg, ok := catalogCmd(cli, false)().(catalogMsg); !ok || msg.err != nil {
		t.Fatalf("first refresh: %+v", msg)
	}
	msg, ok := catalogCmd(cli, false)().(catalogMsg)
	if !ok || msg.err != nil {
		t.Fatalf("second refresh: %+v", msg)
	}
	if msg.changed {
		t.Error("changed = true for an unchanged catalogue")
	}
	if len(sent) != 2 || sent[0] != "" || sent[1] == "" {
		t.Errorf("If-None-Match per request = %q, want none then the stored tag", sent)
	}
}

// An unreachable backend must leave a usable CLI on the compiled-in catalogue. The
// quiet startup refresh reports nothing; only an explicit one says so.
func TestRefreshFailureIsSilentAtStartup(t *testing.T) {
	isolateCatalogCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := client.New(srv.URL, "tok")
	msg, ok := refreshCatalog(cli)().(catalogMsg)
	if !ok {
		t.Fatalf("message = %T, want catalogMsg", refreshCatalog(cli)())
	}
	if msg.err == nil {
		t.Error("a 500 was reported as success")
	}

	m := model{app: state.New(), width: 80}
	updated, _ := m.Update(msg)
	if notice := updated.(model).notice; notice != "" {
		t.Errorf("quiet refresh set a notice: %q", notice)
	}

	loud, _ := m.Update(catalogMsg{err: msg.err})
	if notice := loud.(model).notice; notice == "" {
		t.Error("an explicit refresh failure said nothing")
	}
	if len(catalog.Current()) == 0 {
		t.Error("a failed refresh left an empty catalogue")
	}
}

// A nil client (no session) must not produce a command that panics when run.
func TestCatalogCmdWithNoClient(t *testing.T) {
	if cmd := catalogCmd(nil, true); cmd != nil {
		t.Error("catalogCmd returned a command with no client")
	}
}

// The adapter is where the wire shape becomes the catalogue shape, and a dropped
// field here is invisible until a selector renders blank.
func TestFetcherCarriesEveryField(t *testing.T) {
	srv := catalogServer(t, nil)

	doc, modified, etag, err := catalogFetcher{cli: client.New(srv.URL, "tok")}.
		Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !modified || etag == "" {
		t.Fatalf("modified = %v, etag = %q, want a served catalogue", modified, etag)
	}
	if len(doc.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(doc.Models))
	}
	m := doc.Models[0]
	if m.Brand != "anthropic" || m.ID != "anthropic/claude-opus-4.8" ||
		m.Label != "Opus 4.8" || m.Note != "architecture" || len(m.Efforts) != 4 {
		t.Errorf("model = %+v, want every served field carried across", m)
	}
}
