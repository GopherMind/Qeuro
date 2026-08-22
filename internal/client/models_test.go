package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Roadmap §8 "Startup" pairs the on-disk catalogue cache with an ETag, so the
// client half has to do two things the plain Models() never did: send the stored
// validator, and report "unchanged" distinguishably from "here is the catalogue".
// Conflating those two is the bug that matters — a 304 decoded as a body yields an
// empty catalogue, which overwrites a good cache with nothing.

// headerAbsent is what seen records when the request carried no If-None-Match at
// all. It has to be distinguishable from an empty value: Header.Set(k, "") sends
// the header with an empty value, and Header.Get cannot tell the two apart — so a
// test comparing against "" would pass for a client that always sends it.
const headerAbsent = "<absent>"

func modelsServer(t *testing.T, etag string, hits *int, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		if seen != nil {
			if _, ok := r.Header["If-None-Match"]; ok {
				*seen = append(*seen, r.Header.Get("If-None-Match"))
			} else {
				*seen = append(*seen, headerAbsent)
			}
		}
		w.Header().Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"brand":"anthropic","id":"anthropic/claude-opus-4.8",` +
			`"label":"Opus 4.8","note":"architecture","efforts":["low","medium","high","xhigh"]}]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelsWithETagReturnsTheCatalogueAndValidator(t *testing.T) {
	const etag = `"sha256:deadbeef"`
	srv := modelsServer(t, etag, nil, nil)

	res, err := New(srv.URL, "tok").ModelsWithETag(context.Background(), "")
	if err != nil {
		t.Fatalf("ModelsWithETag: %v", err)
	}
	if res.NotModified {
		t.Fatal("NotModified on a first fetch")
	}
	if res.ETag != etag {
		t.Errorf("ETag = %q, want %q", res.ETag, etag)
	}
	if len(res.Models) != 1 || res.Models[0].ID != "anthropic/claude-opus-4.8" {
		t.Fatalf("models = %+v, want the one served", res.Models)
	}
	if got := res.Models[0].Efforts; len(got) != 4 {
		t.Errorf("efforts = %v, want four levels", got)
	}
}

func TestModelsWithETagSendsTheStoredValidator(t *testing.T) {
	const etag = `"sha256:deadbeef"`
	var seen []string
	srv := modelsServer(t, etag, nil, &seen)

	if _, err := New(srv.URL, "tok").ModelsWithETag(context.Background(), etag); err != nil {
		t.Fatalf("ModelsWithETag: %v", err)
	}
	if len(seen) != 1 || seen[0] != etag {
		t.Errorf("If-None-Match sent = %q, want %q", seen, etag)
	}
}

// The property the cache depends on: 304 is reported as "unchanged", with no
// models, and no error. Returning an error would make the caller log a failure on
// every launch of a healthy CLI; returning an empty list would erase the cache.
func TestModelsWithETagReportsNotModified(t *testing.T) {
	const etag = `"sha256:deadbeef"`
	srv := modelsServer(t, etag, nil, nil)

	res, err := New(srv.URL, "tok").ModelsWithETag(context.Background(), etag)
	if err != nil {
		t.Fatalf("ModelsWithETag: %v", err)
	}
	if !res.NotModified {
		t.Error("NotModified = false for a 304")
	}
	if len(res.Models) != 0 {
		t.Errorf("models = %+v, want none on a 304", res.Models)
	}
	if res.ETag != etag {
		t.Errorf("ETag = %q, want the validator echoed on the 304", res.ETag)
	}
}

// A server that answers 304 without echoing the ETag must not blank the caller's
// stored validator — otherwise the next fetch is unconditional and the cache
// degrades to no cache.
func TestNotModifiedWithoutAnETagKeepsTheRequestedOne(t *testing.T) {
	const etag = `"sha256:deadbeef"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)

	res, err := New(srv.URL, "tok").ModelsWithETag(context.Background(), etag)
	if err != nil {
		t.Fatalf("ModelsWithETag: %v", err)
	}
	if !res.NotModified {
		t.Fatal("NotModified = false for a 304")
	}
	if res.ETag != etag {
		t.Errorf("ETag = %q, want the requested validator preserved", res.ETag)
	}
}

// An empty stored validator means "nothing cached", and must not send the header
// at all: `If-None-Match: ""` is a syntactically valid entity-tag that matches
// nothing, so a server could plausibly answer 304 to it.
func TestNoStoredValidatorSendsNoHeader(t *testing.T) {
	var seen []string
	srv := modelsServer(t, `"sha256:deadbeef"`, nil, &seen)

	if _, err := New(srv.URL, "tok").ModelsWithETag(context.Background(), ""); err != nil {
		t.Fatalf("ModelsWithETag: %v", err)
	}
	if len(seen) != 1 || seen[0] != headerAbsent {
		t.Errorf("If-None-Match = %q, want the header absent entirely", seen)
	}
}

// An error status is still an error: a 401 from an expired token must not be
// mistaken for "unchanged", or the CLI would render a stale catalogue and never
// tell the user their token stopped working.
func TestModelsWithETagSurfacesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"token revoked"}}`))
	}))
	t.Cleanup(srv.Close)

	res, err := New(srv.URL, "tok").ModelsWithETag(context.Background(), `"sha256:x"`)
	if err == nil {
		t.Fatalf("no error for a 401; got %+v", res)
	}
	if !strings.Contains(err.Error(), "token revoked") {
		t.Errorf("error = %v, want the server message", err)
	}
}

// Models() keeps working unchanged: it has callers, and the roadmap row adds a
// capability rather than replacing one.
func TestModelsStillWorks(t *testing.T) {
	srv := modelsServer(t, `"sha256:deadbeef"`, nil, nil)

	models, err := New(srv.URL, "tok").Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
}

// The conditional request must carry the bearer token like any other: /v1/models
// is authenticated, and a lazily-constructed client resolves it on first use.
func TestModelsWithETagAuthenticates(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"brand":"b","id":"b/m","label":"M","efforts":["low"]}]`))
	}))
	t.Cleanup(srv.Close)

	c := NewLazy(srv.URL, func() string { return "lazy-token" })
	if _, err := c.ModelsWithETag(context.Background(), ""); err != nil {
		t.Fatalf("ModelsWithETag: %v", err)
	}
	if auth != "Bearer lazy-token" {
		t.Errorf("Authorization = %q, want the lazily resolved bearer", auth)
	}
}
