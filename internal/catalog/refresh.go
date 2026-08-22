package catalog

import (
	"context"
	"fmt"
	"sync"
)

// This file is the seam between the two halves of roadmap §8's catalogue cache:
// the backend serves the document with a validator, and cache.go stores it. What
// connects them is a conversion and a refresh, and both live here rather than in
// internal/tui because neither is about rendering — putting them in the TUI would
// make them untestable without a Bubble Tea model.
//
// catalog does not import client: client is the transport layer and this package
// is imported by nearly every other one, so the dependency would run the wrong
// way and drag net/http into the startup path of anything that renders a model
// name. The fetch is therefore passed in as a function.

// Fetcher performs one conditional catalogue request. etag is the validator held
// with the cached copy, or "" when nothing is cached.
//
// An interface over one method rather than a *client.Client: it keeps the
// dependency inverted, and it is what lets the tests drive every branch —
// unchanged, changed, and failed — without a server.
type Fetcher interface {
	Fetch(ctx context.Context, etag string) (Document, bool, string, error)
}

// Document is a catalogue as the backend describes it — a flat list of models
// each naming its brand, which is the shape GET /v1/models returns.
type Document struct {
	Models []DocumentModel
}

// DocumentModel is one entry of the served catalogue.
type DocumentModel struct {
	Brand   string
	ID      string
	Label   string
	Note    string
	Efforts []string
}

// FromDocument groups a served catalogue into brands.
//
// The server sends a flat list and the UI drills down brand → model, so someone
// has to group it. Order is taken from the document rather than sorted: the
// backend's list is deliberately curated (strongest first within a brand), and
// sorting alphabetically would put "Haiku" above "Opus" in every selector.
//
// A brand key with no display name of its own falls back to the compiled-in
// catalogue's name, then to the key itself. The served document carries no brand
// label — that is a rendering concern the backend has no reason to know about.
func FromDocument(doc Document) Snapshot {
	names := brandNames()
	order := make([]string, 0, 8)
	byKey := make(map[string]*Brand, 8)

	for _, m := range doc.Models {
		key := clean(m.Brand)
		if key == "" {
			// A model with no brand still belongs somewhere; "other" keeps it visible
			// rather than dropping it, and matches nothing in the built-in list.
			key = "other"
		}
		b, ok := byKey[key]
		if !ok {
			name := names[key]
			if name == "" {
				name = key
			}
			byKey[key] = &Brand{Key: key, Name: name}
			b = byKey[key]
			order = append(order, key)
		}
		efforts := make([]Effort, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			efforts = append(efforts, Effort(e))
		}
		b.Models = append(b.Models, Model{
			ID:      m.ID,
			Label:   m.Label,
			Note:    m.Note,
			Efforts: efforts,
		})
	}

	out := Snapshot{Brands: make([]Brand, 0, len(order))}
	for _, key := range order {
		out.Brands = append(out.Brands, *byKey[key])
	}
	// sanitizeSnapshot does the rest: it drops id-less models and model-less
	// brands, bounds the effort levels, and strips control characters. Running it
	// here as well as in SaveCache means a caller that renders the result of a
	// refresh without saving it gets the same guarantees.
	return sanitizeSnapshot(out)
}

// Refresh revalidates the cached catalogue and stores it when it changed.
//
// It returns the catalogue to render and whether it differs from what was
// cached. A failed refresh is not an error the caller has to handle: the
// compiled-in catalogue is always a correct answer, so the error is returned for
// logging and the Snapshot is still usable.
//
// This never runs before the prompt. Roadmap §8 requires zero network calls at
// startup, so the caller schedules it after the first frame.
func Refresh(ctx context.Context, f Fetcher) (Snapshot, bool, error) {
	cached, hasCache := LoadCache()

	doc, modified, etag, err := f.Fetch(ctx, cached.ETag)
	if err != nil {
		if hasCache {
			return cached, false, err
		}
		return Snapshot{Brands: Brands()}, false, err
	}

	if !modified {
		// The server confirmed the cache. Persist the validator only if it moved —
		// a 304 normally echoes the tag we sent, so this is usually a no-op, and
		// rewriting the file on every launch would be a write for nothing.
		if hasCache {
			if etag != "" && etag != cached.ETag {
				cached.ETag = etag
				_ = SaveCache(cached)
			}
			return cached, false, nil
		}
		// 304 with nothing cached: the server answered a validator we do not hold.
		// Report it rather than rendering an empty catalogue — the caller falls back
		// to the built-in list and the next refresh will be unconditional.
		return Snapshot{Brands: Brands()}, false, fmt.Errorf("catalog: server reported no change but nothing is cached")
	}

	snap := FromDocument(doc)
	snap.ETag = etag
	if !snap.usable() {
		// An empty served catalogue is a server-side fault. Keeping what we have is
		// strictly better than emptying the selector.
		if hasCache {
			return cached, false, fmt.Errorf("catalog: server returned an empty catalogue")
		}
		return Snapshot{Brands: Brands()}, false, fmt.Errorf("catalog: server returned an empty catalogue")
	}

	// Install it before saving: the catalogue is valid regardless of whether the
	// file write succeeds, and the session should use it either way.
	setActive(snap.Brands)

	if err := SaveCache(snap); err != nil {
		// The catalogue is still good; only its persistence failed. Render it now
		// and let the next launch fall back.
		return snap, true, err
	}
	return snap, !sameCatalogue(cached, snap) || !hasCache, nil
}

// active is the catalogue in use: the cache when it is usable, the compiled-in
// list otherwise. Read once and replaced by a successful Refresh.
//
// Memoised because FindModel is called per rendered row, and a file read per row
// would turn the cache from an optimisation into a cost. Guarded by a mutex rather
// than sync.OnceValue because Refresh installs a new catalogue mid-session: a CLI
// left open for hours should see a model added an hour ago, not on next launch.
//
// The memo is keyed by cache path. In a real process that path never changes, so
// the key costs a string compare; what it buys is correctness when it does change
// — under test, where each case gets its own config directory. Keying it means the
// package needs no test-only invalidation hook, and no test can inherit another's
// catalogue.
var (
	activeMu   sync.RWMutex
	activeList []Brand
	activePath string
)

// Current returns the catalogue to render. The result is read-only: it is the
// shared memoised slice, not a copy, because it is called per rendered row and
// copying it there would cost more than the file read it replaces. Callers that
// need to modify a catalogue should copy it first (Brands already returns a copy).
//
// No network, no keychain, and at most one file read per cache location — safe on
// the startup path, which is the whole point of the cache.
func Current() []Brand {
	p := CachePath()

	activeMu.RLock()
	if activeList != nil && activePath == p {
		defer activeMu.RUnlock()
		return activeList
	}
	activeMu.RUnlock()

	activeMu.Lock()
	defer activeMu.Unlock()
	if activeList == nil || activePath != p { // another goroutine may have won the race
		if snap, ok := LoadCache(); ok {
			activeList = snap.Brands
		} else {
			activeList = Brands()
		}
		activePath = p
	}
	return activeList
}

// setActive installs a catalogue for the current cache location.
func setActive(brands []Brand) {
	p := CachePath()
	activeMu.Lock()
	activeList = brands
	activePath = p
	activeMu.Unlock()
}

// brandNames maps brand key → display name from the compiled-in catalogue.
func brandNames() map[string]string {
	out := make(map[string]string, len(brands)+1)
	for _, b := range Brands() {
		out[b.Key] = b.Name
	}
	return out
}

// sameCatalogue reports whether two snapshots list the same models, ignoring the
// validator. Used to answer "did anything the user can see change?" — a validator
// that moved because the backend rebuilt is not worth telling anyone about.
func sameCatalogue(a, b Snapshot) bool {
	if len(a.Brands) != len(b.Brands) {
		return false
	}
	for i := range a.Brands {
		if a.Brands[i].Key != b.Brands[i].Key || len(a.Brands[i].Models) != len(b.Brands[i].Models) {
			return false
		}
		for j := range a.Brands[i].Models {
			if a.Brands[i].Models[j].ID != b.Brands[i].Models[j].ID {
				return false
			}
		}
	}
	return true
}
