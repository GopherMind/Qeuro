package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Roadmap §8 "Startup" asks for «кэш каталога моделей в ~/.qeuro/cache с ETag».
// The cache exists to make the backend catalogue usable without paying for a
// request on the startup path, so the properties that matter are: reading it costs
// one file read and no network, a corrupt or hostile file cannot break the CLI,
// and a stored ETag comes back so the next fetch can be revalidated.

func isolateCache(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("AppData", d)
	t.Setenv("XDG_CONFIG_HOME", d)
	if CacheDir() == "" {
		t.Skip("no config dir available on this platform")
	}
	return d
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		ETag: `"sha256:abc123"`,
		Brands: []Brand{{
			Key:  "anthropic",
			Name: "Anthropic",
			Models: []Model{
				{ID: "anthropic/claude-opus-4.8", Label: "Opus 4.8", Note: "architecture", Efforts: reasoning},
			},
		}},
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	isolateCache(t)
	want := sampleSnapshot()

	if err := SaveCache(want); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	got, ok := LoadCache()
	if !ok {
		t.Fatal("LoadCache reported nothing after a successful save")
	}
	if got.ETag != want.ETag {
		t.Errorf("ETag = %q, want %q", got.ETag, want.ETag)
	}
	if len(got.Brands) != 1 || len(got.Brands[0].Models) != 1 {
		t.Fatalf("brands = %+v, want one brand with one model", got.Brands)
	}
	m := got.Brands[0].Models[0]
	if m.ID != "anthropic/claude-opus-4.8" || m.Label != "Opus 4.8" {
		t.Errorf("model = %+v, want the saved one", m)
	}
	// Efforts drive the effort selector, so losing them would silently reduce every
	// cached model to its default.
	if len(m.Efforts) != len(reasoning) {
		t.Errorf("efforts = %v, want %v", m.Efforts, reasoning)
	}
}

// The file is written under the config dir, in a `cache` subdirectory, because
// that is what the roadmap names and because a catalogue is derived data — it must
// be safe to delete without losing configuration.
func TestCacheLivesUnderTheConfigDir(t *testing.T) {
	d := isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	p := CachePath()
	if !strings.HasPrefix(p, d) {
		t.Errorf("cache path %q is outside the isolated config dir %q", p, d)
	}
	if filepath.Base(filepath.Dir(p)) != "cache" {
		t.Errorf("cache path %q is not in a cache subdirectory", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("stat %s: %v", p, err)
	}
}

// A missing cache is the first-launch case, not an error: it must report "nothing
// cached" so the caller falls back to the built-in catalogue.
func TestLoadWithNoCacheReportsNothing(t *testing.T) {
	isolateCache(t)
	if snap, ok := LoadCache(); ok {
		t.Errorf("LoadCache found something on a clean machine: %+v", snap)
	}
}

// Corrupt JSON is tolerated the same way clientcfg tolerates a corrupt config:
// the CLI keeps working on the built-in catalogue. A cache is an optimisation, and
// an optimisation that can refuse to start the program is a defect.
func TestCorruptCacheIsIgnored(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if err := os.WriteFile(CachePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt the cache: %v", err)
	}

	if snap, ok := LoadCache(); ok {
		t.Errorf("corrupt cache was accepted: %+v", snap)
	}
}

// An empty catalogue is worse than no cache: it would render an empty model
// selector, which looks like the CLI broke rather than like a stale file.
func TestEmptyCatalogueIsRejected(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(Snapshot{ETag: `"sha256:empty"`}); err == nil {
		t.Error("SaveCache accepted an empty catalogue")
	}
	if snap, ok := LoadCache(); ok {
		t.Errorf("an empty catalogue reached the cache: %+v", snap)
	}
}

// The cached document arrives from the network, so it is untrusted input: it is
// rendered into a terminal and used as a model id. Control characters must not
// survive a round trip through the cache, or a hostile backend response would own
// the terminal on every subsequent launch — the stored copy is replayed without a
// server in the loop, so sanitising only at fetch time would leave the file as the
// persistence layer for an injection.
func TestControlCharactersAreStrippedOnTheWayIn(t *testing.T) {
	isolateCache(t)
	snap := Snapshot{
		ETag: "\"sha256:\x1b]0;pwned\x07\"",
		Brands: []Brand{{
			Key:  "evil\x1b[31m",
			Name: "Evil\rBrand",
			Models: []Model{
				{ID: "evil/\x1b[2Jmodel", Label: "L\x00abel", Note: "n\x07ote", Efforts: []Effort{EffortLow}},
			},
		}},
	}
	if err := SaveCache(snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	got, ok := LoadCache()
	if !ok {
		t.Fatal("LoadCache reported nothing")
	}

	raw, err := os.ReadFile(CachePath())
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	for _, b := range raw {
		if b < 0x20 && b != '\n' && b != '\t' || b == 0x7f {
			t.Fatalf("control byte %#x survived into the cache file", b)
		}
	}
	for _, field := range []string{
		got.ETag, got.Brands[0].Key, got.Brands[0].Name,
		got.Brands[0].Models[0].ID, got.Brands[0].Models[0].Label, got.Brands[0].Models[0].Note,
	} {
		if strings.ContainsAny(field, "\x00\x07\x1b\r") {
			t.Errorf("field %q still carries a control character", field)
		}
	}
}

// A cache file far larger than any catalogue is not decoded at all. The same
// stat-then-decode bound as internal/mcp: an unbounded decode of a file that
// something else wrote is a memory-exhaustion primitive.
func TestOversizedCacheIsRefusedWithoutDecoding(t *testing.T) {
	isolateCache(t)
	if err := os.MkdirAll(filepath.Dir(CachePath()), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := make([]byte, maxCacheBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(CachePath(), big, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if snap, ok := LoadCache(); ok {
		t.Errorf("oversized cache was decoded: %+v", snap)
	}
}

// Saving twice must leave one valid file, not a half-written one. The write is the
// only place a crash can produce a file that parses as JSON but describes half a
// catalogue.
func TestSaveOverwritesCleanly(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("first SaveCache: %v", err)
	}
	second := sampleSnapshot()
	second.ETag = `"sha256:second"`
	second.Brands[0].Models = append(second.Brands[0].Models,
		Model{ID: "anthropic/claude-haiku-4.5", Label: "Haiku 4.5", Efforts: []Effort{EffortLow}})
	if err := SaveCache(second); err != nil {
		t.Fatalf("second SaveCache: %v", err)
	}

	got, ok := LoadCache()
	if !ok {
		t.Fatal("LoadCache reported nothing after overwrite")
	}
	if got.ETag != `"sha256:second"` {
		t.Errorf("ETag = %q, want the second one", got.ETag)
	}
	if len(got.Brands[0].Models) != 2 {
		t.Errorf("models = %d, want 2", len(got.Brands[0].Models))
	}
}

// The cache holds no secret, but it lives beside the token, and a world-readable
// file in a 0700 directory is the kind of drift that later gets copied.
func TestCacheFileIsOwnerOnly(t *testing.T) {
	isolateCache(t)
	if err := SaveCache(sampleSnapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	info, err := os.Stat(CachePath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		// Windows does not honour unix bits; the check is meaningful on CI's linux
		// runner, which is where the permission would matter.
		t.Logf("cache mode = %#o", perm)
	}
}

// No config dir at all (a locked-down environment) must degrade to "no cache",
// never to a file written somewhere unexpected.
func TestNoConfigDirDisablesTheCache(t *testing.T) {
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if CacheDir() != "" {
		t.Skip("this platform still reports a config dir")
	}

	if _, ok := LoadCache(); ok {
		t.Error("LoadCache succeeded with no config dir")
	}
	if err := SaveCache(sampleSnapshot()); err == nil {
		t.Error("SaveCache succeeded with no config dir")
	} else if !errors.Is(err, ErrNoCacheDir) {
		t.Errorf("SaveCache error = %v, want ErrNoCacheDir", err)
	}
}
