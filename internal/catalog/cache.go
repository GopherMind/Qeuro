package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"qeuro/internal/clientcfg"
)

// The model catalogue the CLI renders is compiled in (see brands), which means a
// model added on the backend is invisible until the user upgrades. Roadmap §8
// "Startup" resolves that without putting a request in front of the prompt: cache
// the backend's catalogue on disk, render from the cache, and revalidate with an
// ETag out of band.
//
// The cache is therefore *derived* data with a *built-in* fallback. That shapes
// every decision below: a missing, corrupt, oversized or empty cache is not an
// error to report but a reason to use the compiled-in list, and the file must be
// safe to delete at any time.

// cacheFileName is the catalogue snapshot inside CacheDir.
const cacheFileName = "models.json"

// maxCacheBytes bounds the decode. The real document is a few KiB; the limit is
// generous but finite, because this file is written by a network response and
// decoding an unbounded one is a memory-exhaustion primitive. Same bound and same
// stat-then-decode shape as internal/mcp's config reader.
const maxCacheBytes = 1 << 20

// ErrNoCacheDir means the OS reports no config directory, so there is nowhere to
// cache. Callers treat it as "caching is unavailable", not as a failure.
var ErrNoCacheDir = errors.New("catalog: no config directory available")

// Snapshot is a cached copy of the backend catalogue plus the validator that
// produced it.
//
// ETag is stored with the payload rather than in a sidecar file so the two cannot
// disagree: a validator that outlives the body it describes would make the CLI
// send If-None-Match, get a 304, and render a catalogue it no longer has.
type Snapshot struct {
	ETag   string  `json:"etag"`
	Brands []Brand `json:"brands"`
}

// CacheDir returns the directory holding cached, regenerable data, or "" when the
// OS reports no config directory.
func CacheDir() string {
	base := clientcfg.ConfigDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "cache")
}

// CachePath returns the catalogue cache file location, or "" when unavailable.
func CachePath() string {
	d := CacheDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, cacheFileName)
}

// LoadCache returns the cached catalogue. The bool is false whenever the cache
// cannot be used for any reason — absent, unreadable, oversized, malformed, or
// describing an empty catalogue — because every one of those has the same correct
// response: use the compiled-in catalogue.
//
// It makes no network call and does not touch the secret store, which is what
// makes it usable on the startup path.
func LoadCache() (Snapshot, bool) {
	p := CachePath()
	if p == "" {
		return Snapshot{}, false
	}
	// #nosec G304 -- p is this process's own cache path under the user's config
	// directory, derived by CachePath(); it is not caller-supplied.
	f, err := os.Open(p)
	if err != nil {
		return Snapshot{}, false
	}
	defer func() { _ = f.Close() }()

	// Stat before decoding, so an oversized file costs a stat rather than a read:
	// io.LimitReader would still stream a gigabyte through the decoder before
	// failing.
	info, err := f.Stat()
	if err != nil || info.Size() > maxCacheBytes {
		return Snapshot{}, false
	}

	var snap Snapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return Snapshot{}, false
	}
	// Sanitise on the way out as well as in. The file is not a trust boundary — it
	// can be edited by anything running as the user — so the render path must not
	// depend on SaveCache having been the writer.
	snap = sanitizeSnapshot(snap)
	if !snap.usable() {
		return Snapshot{}, false
	}
	return snap, true
}

// SaveCache writes the snapshot, replacing any previous one.
//
// An unusable snapshot is refused rather than written: an empty catalogue on disk
// would render an empty model selector on the next launch, which reads as a broken
// CLI rather than as a stale file.
func SaveCache(snap Snapshot) error {
	d := CacheDir()
	if d == "" {
		return ErrNoCacheDir
	}
	snap = sanitizeSnapshot(snap)
	if !snap.usable() {
		return fmt.Errorf("catalog: refusing to cache an empty catalogue")
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d, cacheFileName), data, 0o600); err != nil {
		return err
	}
	// Keep the memoised catalogue and the file in agreement: Current caches its
	// answer for the process, so a write that did not install it would leave the
	// session rendering a catalogue that is no longer on disk. The sanitised copy is
	// installed, not the argument, so memory and file hold the same thing.
	setActive(snap.Brands)
	return nil
}

// usable reports whether a snapshot can be rendered: at least one brand carrying
// at least one model with an id.
func (s Snapshot) usable() bool {
	for _, b := range s.Brands {
		for _, m := range b.Models {
			if m.ID != "" {
				return true
			}
		}
	}
	return false
}

// sanitizeSnapshot strips control characters from every string that came from the
// network, and drops entries that cannot be rendered.
//
// The catalogue is printed into a terminal (the model selector, the status line)
// and one field is used as a model id. A backend response — or a hand-edited cache
// file — carrying an escape sequence would otherwise address the terminal, and
// because this document is *replayed from disk* on later launches, sanitising only
// at fetch time would leave the cache as the persistence layer for that injection.
func sanitizeSnapshot(s Snapshot) Snapshot {
	out := Snapshot{ETag: clean(s.ETag), Brands: make([]Brand, 0, len(s.Brands))}
	for _, b := range s.Brands {
		nb := Brand{
			Key:    clean(b.Key),
			Name:   clean(b.Name),
			Models: make([]Model, 0, len(b.Models)),
		}
		for _, m := range b.Models {
			id := clean(m.ID)
			if id == "" {
				continue
			}
			nm := Model{
				ID:      id,
				Label:   clean(m.Label),
				Note:    clean(m.Note),
				Efforts: cleanEfforts(m.Efforts),
			}
			if nm.Label == "" {
				// A model with no label would render as a blank row in the selector.
				nm.Label = nm.ID
			}
			nb.Models = append(nb.Models, nm)
		}
		if len(nb.Models) == 0 {
			continue
		}
		out.Brands = append(out.Brands, nb)
	}
	return out
}

// cleanEfforts keeps only the effort levels this CLI understands, preserving order
// so the first remains the default. An unknown level from a newer backend is
// dropped rather than passed to the UI, which switches on the known set.
func cleanEfforts(in []Effort) []Effort {
	out := make([]Effort, 0, len(in))
	for _, e := range in {
		switch Effort(clean(string(e))) {
		case EffortLow:
			out = append(out, EffortLow)
		case EffortMedium:
			out = append(out, EffortMedium)
		case EffortHigh:
			out = append(out, EffortHigh)
		case EffortXHigh:
			out = append(out, EffortXHigh)
		}
	}
	if len(out) == 0 {
		// Every model must offer something, or the effort selector has no rows.
		return []Effort{EffortMedium}
	}
	return out
}

// clean removes control characters outright rather than escaping them.
//
// This differs from clientcfg.DisplaySafe on purpose. That function escapes,
// because a config value has to keep working even with a stray byte in it — the
// user typed it. These strings are supplied by a remote service and one of them is
// an identifier compared for equality, so the right move is to reject the bytes,
// not to render them visibly.
func clean(s string) string {
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
