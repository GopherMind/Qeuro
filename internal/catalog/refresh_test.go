package catalog

import (
	"context"
	"errors"
	"testing"
)

// Refresh is where the two halves of roadmap §8's cache meet, so the properties
// under test are the ones that decide what the user sees after a failed, an
// unchanged, and a changed fetch. The rule throughout: never render less than we
// already had.

// fakeFetcher drives every branch of Refresh without a server.
type fakeFetcher struct {
	doc      Document
	modified bool
	etag     string
	err      error

	calls    int
	sentETag string
}

func (f *fakeFetcher) Fetch(_ context.Context, etag string) (Document, bool, string, error) {
	f.calls++
	f.sentETag = etag
	return f.doc, f.modified, f.etag, f.err
}

func sampleDocument() Document {
	return Document{Models: []DocumentModel{
		{Brand: "anthropic", ID: "anthropic/claude-opus-4.8", Label: "Opus 4.8", Note: "architecture",
			Efforts: []string{"low", "medium", "high", "xhigh"}},
		{Brand: "anthropic", ID: "anthropic/claude-haiku-4.5", Label: "Haiku 4.5", Note: "fast",
			Efforts: []string{"low", "medium"}},
		{Brand: "openai", ID: "openai/gpt-5.5", Label: "GPT-5.5", Note: "all-rounder",
			Efforts: []string{"medium", "high"}},
	}}
}

func TestFromDocumentGroupsByBrandInServedOrder(t *testing.T) {
	snap := FromDocument(sampleDocument())

	if len(snap.Brands) != 2 {
		t.Fatalf("brands = %d, want 2", len(snap.Brands))
	}
	if snap.Brands[0].Key != "anthropic" || snap.Brands[1].Key != "openai" {
		t.Errorf("brand order = %q/%q, want the document's order",
			snap.Brands[0].Key, snap.Brands[1].Key)
	}
	// Order within a brand is the backend's curation (strongest first). Sorting
	// would put Haiku above Opus in every selector.
	ids := []string{snap.Brands[0].Models[0].ID, snap.Brands[0].Models[1].ID}
	if ids[0] != "anthropic/claude-opus-4.8" || ids[1] != "anthropic/claude-haiku-4.5" {
		t.Errorf("model order = %v, want the document's order", ids)
	}
	if got := snap.Brands[0].Models[0].Efforts; len(got) != 4 {
		t.Errorf("efforts = %v, want four levels", got)
	}
}

// The served document has no brand label, so the display name comes from the
// compiled-in catalogue. Without this every cached brand would render as its bare
// key ("zai" instead of "Z.AI (GLM)").
func TestFromDocumentTakesBrandNamesFromTheBuiltInCatalogue(t *testing.T) {
	snap := FromDocument(Document{Models: []DocumentModel{
		{Brand: "zai", ID: "z-ai/glm-5.2", Label: "GLM 5.2", Efforts: []string{"high"}},
	}})

	if len(snap.Brands) != 1 {
		t.Fatalf("brands = %d, want 1", len(snap.Brands))
	}
	if snap.Brands[0].Name != "Z.AI (GLM)" {
		t.Errorf("brand name = %q, want the built-in display name", snap.Brands[0].Name)
	}
}

// An unknown brand key is rendered under its key rather than dropped: a model the
// backend added under a brand this build has never heard of is exactly what the
// cache exists to deliver.
func TestFromDocumentKeepsUnknownBrands(t *testing.T) {
	snap := FromDocument(Document{Models: []DocumentModel{
		{Brand: "brand-new", ID: "brand-new/model-1", Label: "Model 1", Efforts: []string{"medium"}},
	}})

	if len(snap.Brands) != 1 {
		t.Fatalf("brands = %d, want the unknown brand kept", len(snap.Brands))
	}
	if snap.Brands[0].Key != "brand-new" || snap.Brands[0].Name != "brand-new" {
		t.Errorf("brand = %+v, want the key used as the name", snap.Brands[0])
	}
}

func TestFromDocumentSanitisesServedStrings(t *testing.T) {
	snap := FromDocument(Document{Models: []DocumentModel{
		{Brand: "evil\x1b[31m", ID: "evil/\x1b]0;pwned\x07model", Label: "L\x00abel", Efforts: []string{"medium"}},
		{Brand: "anthropic", ID: "", Label: "no id", Efforts: []string{"medium"}},
	}})

	for _, b := range snap.Brands {
		if b.Key == "" || b.Key != clean(b.Key) {
			t.Errorf("brand key %q was not sanitised", b.Key)
		}
		for _, m := range b.Models {
			if m.ID == "" {
				t.Error("a model with no id survived")
			}
			if m.ID != clean(m.ID) || m.Label != clean(m.Label) {
				t.Errorf("model %+v was not sanitised", m)
			}
		}
	}
}

// An unknown effort level from a newer backend must not reach the UI, which
// switches on the four it knows. Dropping all of them leaves the default, so the
// effort selector still has a row.
func TestFromDocumentBoundsEffortLevels(t *testing.T) {
	snap := FromDocument(Document{Models: []DocumentModel{
		{Brand: "b", ID: "b/m", Label: "M", Efforts: []string{"ultra", "medium", "cosmic"}},
		{Brand: "b", ID: "b/n", Label: "N", Efforts: []string{"ultra"}},
	}})

	m := snap.Brands[0].Models[0]
	if len(m.Efforts) != 1 || m.Efforts[0] != EffortMedium {
		t.Errorf("efforts = %v, want only the known level", m.Efforts)
	}
	if n := snap.Brands[0].Models[1]; len(n.Efforts) != 1 || n.Efforts[0] != EffortMedium {
		t.Errorf("efforts = %v, want a default when none are known", n.Efforts)
	}
}

func TestRefreshStoresAChangedCatalogue(t *testing.T) {
	isolateCache(t)
	f := &fakeFetcher{doc: sampleDocument(), modified: true, etag: `"sha256:one"`}

	snap, changed, err := Refresh(context.Background(), f)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !changed {
		t.Error("changed = false for a first fetch")
	}
	if snap.ETag != `"sha256:one"` {
		t.Errorf("ETag = %q, want the served validator", snap.ETag)
	}
	stored, ok := LoadCache()
	if !ok {
		t.Fatal("nothing was cached")
	}
	if stored.ETag != `"sha256:one"` || len(stored.Brands) != 2 {
		t.Errorf("cached = %+v, want the served catalogue and validator", stored)
	}
}

// The point of the validator: a second refresh sends the stored tag.
func TestRefreshRevalidatesWithTheStoredETag(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	f := &fakeFetcher{modified: false, etag: `"sha256:abc123"`}

	if _, _, err := Refresh(context.Background(), f); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if f.sentETag != `"sha256:abc123"` {
		t.Errorf("sent ETag = %q, want the cached one", f.sentETag)
	}
}

// A 304 must keep the cached catalogue and report no change. Returning an empty
// snapshot here is the failure that would blank the model selector on every
// launch of a healthy CLI.
func TestRefreshKeepsTheCacheOnNotModified(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	f := &fakeFetcher{modified: false, etag: `"sha256:abc123"`}

	snap, changed, err := Refresh(context.Background(), f)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if changed {
		t.Error("changed = true for a 304")
	}
	if len(snap.Brands) != 1 || snap.Brands[0].Models[0].ID != "anthropic/claude-opus-4.8" {
		t.Errorf("snapshot = %+v, want the cached catalogue", snap.Brands)
	}
}

// A network failure is not a reason to show less. With a cache, render it; the
// error still comes back so a caller that wants to log it can.
func TestRefreshFallsBackToTheCacheOnError(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	wantErr := errors.New("dial tcp: refused")
	f := &fakeFetcher{err: wantErr}

	snap, changed, err := Refresh(context.Background(), f)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want the fetch error surfaced", err)
	}
	if changed {
		t.Error("changed = true for a failed fetch")
	}
	if len(snap.Brands) != 1 {
		t.Fatalf("snapshot = %+v, want the cached catalogue", snap.Brands)
	}
}

// Offline on a clean machine: the compiled-in catalogue, never an empty one.
func TestRefreshFallsBackToTheBuiltInCatalogue(t *testing.T) {
	isolateCache(t)
	f := &fakeFetcher{err: errors.New("offline")}

	snap, changed, err := Refresh(context.Background(), f)
	if err == nil {
		t.Error("no error for a failed fetch with no cache")
	}
	if changed {
		t.Error("changed = true for a failed fetch")
	}
	if len(snap.Brands) != len(Brands()) {
		t.Errorf("brands = %d, want the built-in %d", len(snap.Brands), len(Brands()))
	}
}

// An empty served catalogue is a server fault, and keeping the cache is strictly
// better than emptying the selector.
func TestRefreshRefusesAnEmptyServedCatalogue(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	f := &fakeFetcher{doc: Document{}, modified: true, etag: `"sha256:empty"`}

	snap, changed, err := Refresh(context.Background(), f)
	if err == nil {
		t.Error("no error for an empty served catalogue")
	}
	if changed {
		t.Error("changed = true for a refused catalogue")
	}
	if len(snap.Brands) != 1 || snap.Brands[0].Models[0].ID != "anthropic/claude-opus-4.8" {
		t.Errorf("snapshot = %+v, want the cache preserved", snap.Brands)
	}
	if stored, _ := LoadCache(); stored.ETag != `"sha256:abc123"` {
		t.Errorf("stored ETag = %q, want the cache untouched", stored.ETag)
	}
}

// A 304 for a validator we do not hold means the server and the cache disagree
// about what exists. Reporting it beats rendering an empty catalogue, and the
// next refresh is unconditional because nothing was stored.
func TestRefreshReportsNotModifiedWithNoCache(t *testing.T) {
	isolateCache(t)
	f := &fakeFetcher{modified: false, etag: `"sha256:ghost"`}

	snap, changed, err := Refresh(context.Background(), f)
	if err == nil {
		t.Error("no error for a 304 with nothing cached")
	}
	if changed {
		t.Error("changed = true")
	}
	if len(snap.Brands) != len(Brands()) {
		t.Errorf("brands = %d, want the built-in catalogue", len(snap.Brands))
	}
	if _, ok := LoadCache(); ok {
		t.Error("a phantom 304 wrote a cache file")
	}
}

// Re-fetching the same catalogue is not a change worth telling anyone about, so
// changed must be false even though the fetch returned a body.
func TestRefreshReportsNoChangeForTheSameCatalogue(t *testing.T) {
	isolateCache(t)
	first := &fakeFetcher{doc: sampleDocument(), modified: true, etag: `"sha256:one"`}
	if _, _, err := Refresh(context.Background(), first); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	second := &fakeFetcher{doc: sampleDocument(), modified: true, etag: `"sha256:two"`}
	snap, changed, err := Refresh(context.Background(), second)
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if changed {
		t.Error("changed = true for an identical catalogue")
	}
	// The validator still moves, so the next revalidation uses the current one.
	if snap.ETag != `"sha256:two"` {
		t.Errorf("ETag = %q, want the new validator stored anyway", snap.ETag)
	}
}

func TestRefreshReportsAChangedModelList(t *testing.T) {
	isolateCache(t)
	first := &fakeFetcher{doc: sampleDocument(), modified: true, etag: `"sha256:one"`}
	if _, _, err := Refresh(context.Background(), first); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	doc := sampleDocument()
	doc.Models = append(doc.Models, DocumentModel{
		Brand: "openai", ID: "openai/gpt-6", Label: "GPT-6", Efforts: []string{"high"}})
	second := &fakeFetcher{doc: doc, modified: true, etag: `"sha256:two"`}

	_, changed, err := Refresh(context.Background(), second)
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if !changed {
		t.Error("changed = false after a model was added")
	}
}

// Current memoises, and the memo is keyed by cache path. Without the key the first
// caller in a process would pin the catalogue forever, which is invisible in
// production (the path never moves) and wrong under test — every case after the
// first would assert against the first one's catalogue.
func TestCurrentIsKeyedToTheCacheLocation(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if got := Current(); len(got) != 1 {
		t.Fatalf("Current = %d brands, want the cached one", len(got))
	}

	// Move the config directory: a different location holds a different catalogue.
	other := t.TempDir()
	t.Setenv("AppData", other)
	t.Setenv("XDG_CONFIG_HOME", other)
	if got := Current(); len(got) != len(Brands()) {
		t.Errorf("after relocating the cache Current returned %d brands, want the built-in %d",
			len(got), len(Brands()))
	}
}

// FindModel reads the active catalogue, so a model that exists only in the cache
// must resolve. Otherwise a refreshed selector would list a model that cannot be
// chosen.
func TestFindModelUsesTheActiveCatalogue(t *testing.T) {
	isolateCache(t)
	snap := sampleSnapshot()
	snap.Brands[0].Models = append(snap.Brands[0].Models,
		Model{ID: "anthropic/claude-opus-9", Label: "Opus 9", Efforts: []Effort{EffortHigh}})
	if err := SaveCache(snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	if _, _, ok := FindModel("anthropic/claude-opus-9"); !ok {
		t.Error("a cache-only model does not resolve")
	}
	// And a model only the built-in catalogue lists is no longer found, which is
	// the point: the backend is the source of truth once it has answered.
	if _, _, ok := FindModel("openai/gpt-5.5"); ok {
		t.Error("a model absent from the cached catalogue still resolves")
	}
}

// Default is looked up by id, and a cached catalogue need not list it. Returning a
// zero Model would start the session on an empty model id, which every request
// rejects.
func TestDefaultSurvivesACacheWithoutIt(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil { // anthropic only
		t.Fatalf("SaveCache: %v", err)
	}

	if _, _, ok := FindModel(defaultModelID); ok {
		t.Fatal("the sample cache unexpectedly lists the default model")
	}
	d := Default()
	if d.ID == "" {
		t.Fatal("Default returned a model with no id")
	}
	if d.ID != defaultModelID {
		t.Errorf("Default = %q, want the built-in default %q", d.ID, defaultModelID)
	}
}

// Current is what the render path calls, so it must never be empty and must
// prefer the cache — otherwise the cache buys nothing.
func TestCurrentPrefersTheCache(t *testing.T) {
	isolateCache(t)
	if got := Current(); len(got) != len(Brands()) {
		t.Errorf("with no cache Current returned %d brands, want the built-in %d", len(got), len(Brands()))
	}

	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	got := Current()
	if len(got) != 1 || got[0].Key != "anthropic" {
		t.Errorf("Current = %+v, want the cached catalogue", got)
	}
}
