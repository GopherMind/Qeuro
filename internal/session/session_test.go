package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate points the journal directory at a temp dir. clientcfg.ConfigDir reads
// the OS config dir, so both platform variables are set: the test must not touch
// the developer's real ~/.qeuro/sessions.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	got := Dir()
	if got == "" {
		t.Fatal("journal dir should be resolvable with the config dir set")
	}
	if !strings.HasPrefix(got, dir) {
		t.Fatalf("journal dir %q escaped the temp config dir %q", got, dir)
	}
	return got
}

func TestNewIDIsSortableAndUnique(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 5, 1, 0, time.UTC)
	a := NewID(now)
	b := NewID(now)
	if a == b {
		t.Fatal("two ids from the same instant must differ: two processes started in " +
			"the same second would otherwise share one journal")
	}
	if !validID(a) || !validID(b) {
		t.Fatalf("NewID must produce ids validID accepts: %q %q", a, b)
	}
	if !strings.HasPrefix(a, "20260813-090501-") {
		t.Fatalf("id should start with the sortable UTC timestamp, got %q", a)
	}
	// Sortability is what makes "the newest session" a name sort.
	later := NewID(now.Add(time.Second))
	if !(a < later) {
		t.Fatalf("later id %q must sort after %q", later, a)
	}
}

func TestValidIDRejectsPathAndDeviceNames(t *testing.T) {
	bad := []string{
		"", "..", "../escape", "a/b", `a\b`, "C:evil", "id:stream",
		"UPPER", "sp ace", "tilde~", "-", "--", strings.Repeat("a", 65),
		"na\x00me",
	}
	for _, id := range bad {
		if validID(id) {
			t.Errorf("validID(%q) = true, want false", id)
		}
	}
	for _, id := range []string{"20260813-090501-abc123", "20260813-090501"} {
		if !validID(id) {
			t.Errorf("validID(%q) = false, want true", id)
		}
	}
}

func TestJournalIsNotCreatedUntilFirstRecord(t *testing.T) {
	dir := isolate(t)
	id := NewID(time.Now())
	j, err := New(id, time.Now(), Record{Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if j == nil {
		t.Fatal("New should return a journal when a config dir exists")
	}
	t.Cleanup(func() { _ = j.Close(time.Now()) })
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("New must touch no disk; sessions dir exists: %v", err)
	}
	if err := j.Append(Record{Kind: KindUser, Text: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+ext)); err != nil {
		t.Fatalf("the first record must create the journal: %v", err)
	}
}

func TestCloseOnUnstartedJournalLeavesNoFile(t *testing.T) {
	dir := isolate(t)
	id := NewID(time.Now())
	j, _ := New(id, time.Now(), Record{Version: "test"})
	if err := j.Close(time.Now()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A launch that only opened the TUI and quit must not consume one of the
	// capped journal slots, or a few hundred such launches would evict the real
	// transcripts.
	if _, err := os.Stat(filepath.Join(dir, id+ext)); !os.IsNotExist(err) {
		t.Fatalf("Close on an unused journal must not create a file: %v", err)
	}
}

func TestMetaRecordCarriesSessionContext(t *testing.T) {
	dir := isolate(t)
	id := NewID(time.Now())
	j, _ := New(id, time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC),
		Record{Version: "9.9.9", Dir: "/work/repo", Model: "some-model", Text: "ignored"})
	t.Cleanup(func() { _ = j.Close(time.Now()) })
	if err := j.Append(Record{Kind: KindUser, Text: "x"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, id+ext))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var meta Record
	first := strings.SplitN(string(data), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &meta); err != nil {
		t.Fatalf("first line must be the meta record: %v", err)
	}
	if meta.Kind != KindMeta {
		t.Fatalf("first record kind = %q, want meta", meta.Kind)
	}
	if meta.Version != "9.9.9" || meta.Dir != "/work/repo" || meta.Model != "some-model" {
		t.Fatalf("meta lost context: %+v", meta)
	}
	if meta.Text != "" {
		t.Fatalf("meta must carry no text, got %q", meta.Text)
	}
}

func TestEveryRecordIsFlushedToDisk(t *testing.T) {
	isolate(t)
	// The flush is what makes the journal survive a power loss, and a reader in
	// this process cannot see the difference — the page cache answers either way.
	// So the call itself is what gets asserted.
	var syncs int
	real := syncFile
	syncFile = func(f *os.File) error {
		syncs++
		return real(f)
	}
	t.Cleanup(func() { syncFile = real })

	j, err := New(NewID(time.Now()), time.Now(), Record{Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = j.Close(time.Now()) })

	if syncs != 0 {
		t.Fatalf("syncs = %d before any record; New must touch no disk", syncs)
	}
	if err := j.Append(Record{Kind: KindUser, Text: "one"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// The first Append writes meta plus the record: two records, two flushes.
	if syncs != 2 {
		t.Fatalf("syncs = %d after the first record, want 2 (meta + record)", syncs)
	}
	for i := 0; i < 3; i++ {
		if err := j.Append(Record{Kind: KindAssistant, Text: "r"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if syncs != 5 {
		t.Fatalf("syncs = %d after 4 records, want one flush per record", syncs)
	}
}

func TestAppendIsDurableToAnIndependentReader(t *testing.T) {
	dir := isolate(t)
	id := NewID(time.Now())
	j, _ := New(id, time.Now(), Record{Version: "test"})
	t.Cleanup(func() { _ = j.Close(time.Now()) })

	// Read the file back through a separate handle after each Append. The point
	// of the row is that a record is on disk before the next one is written, so
	// an independent reader must see turn N while turn N+1 has not happened.
	for i, text := range []string{"one", "two", "three"} {
		if err := j.Append(Record{Kind: KindUser, Text: text}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, id+ext))
		if err != nil {
			t.Fatalf("read after Append %d: %v", i, err)
		}
		if !strings.Contains(string(data), `"text":"`+text+`"`) {
			t.Fatalf("record %q not durable after Append: %s", text, data)
		}
	}
}
