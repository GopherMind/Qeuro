package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNilJournalIsUsable(t *testing.T) {
	var j *Journal
	// A nil journal is the "no config directory" case, and every call site is a
	// turn in progress: journalling must degrade to nothing, never to a failure.
	if err := j.Append(Record{Kind: KindUser, Text: "x"}); err != nil {
		t.Fatalf("Append on nil journal: %v", err)
	}
	if err := j.Close(time.Now()); err != nil {
		t.Fatalf("Close on nil journal: %v", err)
	}
	if j.ID() != "" || j.Path() != "" || j.Err() != "" {
		t.Fatal("nil journal must report empty id/path/err")
	}
}

func TestNewWithoutConfigDirYieldsNoJournal(t *testing.T) {
	// No config dir: on Windows os.UserConfigDir reads %AppData%, elsewhere
	// $XDG_CONFIG_HOME or $HOME. Emptying all three makes ConfigDir fail, which
	// must mean "not journalled", not "journalled somewhere else".
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if Dir() != "" {
		t.Skip("this platform still reports a config dir without those variables")
	}
	j, err := New(NewID(time.Now()), time.Now(), Record{})
	if err != nil {
		t.Fatalf("New must not fail when there is nowhere to write: %v", err)
	}
	if j != nil {
		t.Fatal("New should return a nil journal when there is no config dir")
	}
}

func TestNewRejectsInvalidID(t *testing.T) {
	isolate(t)
	if _, err := New("../escape", time.Now(), Record{}); err == nil {
		t.Fatal("New must reject an id that is not a journal name")
	}
}

func TestIDAndPathReportTheJournalLocation(t *testing.T) {
	dir := isolate(t)
	id := "20260813-091000-aaaaaa"
	j, err := New(id, time.Now(), Record{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if j.ID() != id {
		t.Fatalf("ID = %q, want %q", j.ID(), id)
	}
	// Path is reported before the file exists: the /sessions screen shows where
	// the transcript will go, and a path that appears only after the first turn
	// would read as "not being recorded".
	if want := filepath.Join(dir, id+ext); j.Path() != want {
		t.Fatalf("Path = %q, want %q", j.Path(), want)
	}
}

func TestOpeningTheSameIDTwiceFails(t *testing.T) {
	isolate(t)
	id := "20260813-091100-aaaaaa"
	j1, _ := New(id, time.Now(), Record{})
	if err := j1.Append(Record{Kind: KindUser, Text: "first process"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	t.Cleanup(func() { _ = j1.Close(time.Now()) })

	j2, _ := New(id, time.Now(), Record{})
	// O_EXCL: appending this session's turns to another session's transcript is
	// worse than not recording them.
	if err := j2.Append(Record{Kind: KindUser, Text: "second process"}); err == nil {
		t.Fatal("a second journal on the same id must fail rather than interleave")
	}
	if j2.Err() == "" {
		t.Fatal("the failure must be reportable through Err")
	}
}

func TestErrKeepsTheFirstFailureNotTheLast(t *testing.T) {
	isolate(t)
	j, _ := New("20260813-091200-aaaaaa", time.Now(), Record{})
	if j.Err() != "" {
		t.Fatalf("a healthy journal must report no error, got %q", j.Err())
	}
	t.Cleanup(func() { _ = j.Close(time.Now()) })

	// Two *distinct* failures are needed to tell "keep the first" from "keep the
	// last". The first is a flush failure — what a full disk actually looks like;
	// the second is a write to a descriptor closed underneath the journal.
	real := syncFile
	syncFile = func(*os.File) error { return errFakeDiskFull }
	if err := j.Append(Record{Kind: KindUser, Text: "first"}); err == nil {
		t.Fatal("a failing flush must be reported to the caller")
	}
	syncFile = real
	t.Cleanup(func() { syncFile = real })

	first := j.Err()
	if !strings.Contains(first, "disk is full") {
		t.Fatalf("Err = %q, want the flush failure", first)
	}

	j.mu.Lock()
	inner := j.f
	j.mu.Unlock()
	_ = inner.Close()
	if err := j.Append(Record{Kind: KindUser, Text: "second"}); err == nil {
		t.Fatal("a write to a closed file must return an error")
	}
	if j.Err() != first {
		t.Fatalf("Err changed to %q: the first failure is the one the user needs, "+
			"since every later record fails for a consequence of it", j.Err())
	}
}

// errFakeDiskFull stands in for the failure that motivates Err at all.
var errFakeDiskFull = errors.New("simulated: the disk is full")

func TestJournalStopsAtItsSizeCap(t *testing.T) {
	dir := isolate(t)
	id := "20260813-091300-aaaaaa"
	j, _ := New(id, time.Now(), Record{})
	t.Cleanup(func() { _ = j.Close(time.Now()) })

	big := strings.Repeat("a", maxRecordBytes)
	// The cap has to be reported once and then stop talking: the conversation on
	// screen keeps going, so a warning on every turn from here on would bury it,
	// and no warning at all means the user finds out by trying to resume.
	reported := 0
	// Keep going well past the cap: one report on the record that fills the
	// journal, then silence for the rest. A loop that stops at the first drop
	// could not tell "reported once" from "reported every time".
	for i := 0; i < (maxJournalBytes/maxRecordBytes)+20; i++ {
		if err := j.Append(Record{Kind: KindUser, Text: big}); err != nil {
			reported++
			if !errors.Is(err, errJournalFull) {
				t.Fatalf("Append %d: %v, want errJournalFull", i, err)
			}
		}
	}
	if reported != 1 {
		t.Fatalf("the size cap was reported %d times, want exactly 1", reported)
	}
	info, err := os.Stat(filepath.Join(dir, id+ext))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > maxJournalBytes {
		t.Fatalf("journal grew to %d bytes, past the %d cap", info.Size(), maxJournalBytes)
	}
	// Silently dropping turns is the failure this reports: a full journal has to
	// say so, since the conversation on screen keeps going.
	if !strings.Contains(j.Err(), "size limit") {
		t.Fatalf("Err = %q, want a size-limit report", j.Err())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := isolate(t)
	id := "20260813-091400-aaaaaa"
	j, _ := New(id, time.Now(), Record{})
	if err := j.Append(Record{Kind: KindUser, Text: "q"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := j.Close(time.Now()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Both `qeuro resume` and Run reach the same shutdown path; a second Close
	// must not error or write a second end marker.
	if err := j.Close(time.Now()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, id+ext))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(data), `"kind":"end"`); n != 1 {
		t.Fatalf("end markers = %d, want exactly 1", n)
	}
	// A record after Close is dropped, not appended to a closed file.
	if err := j.Append(Record{Kind: KindUser, Text: "after close"}); err != nil {
		t.Fatalf("Append after Close: %v", err)
	}
	data2, _ := os.ReadFile(filepath.Join(dir, id+ext))
	if strings.Contains(string(data2), "after close") {
		t.Fatal("a record appended after Close must not reach the journal")
	}
}

func TestLabelDescribesTheSession(t *testing.T) {
	at := time.Date(2026, 8, 13, 9, 15, 0, 0, time.UTC)
	s := Session{ID: "20260813-091500-aaaaaa", Meta: Record{At: at}}
	label := s.Label()
	if !strings.HasPrefix(label, s.ID) {
		t.Fatalf("Label = %q, want it to start with the id", label)
	}
	if !strings.Contains(label, at.Local().Format("2006-01-02 15:04")) {
		t.Fatalf("Label = %q, want the local timestamp", label)
	}
	// A journal whose meta line was lost still has to be nameable.
	bare := Session{ID: "20260813-091600-aaaaaa"}
	if bare.Label() != bare.ID {
		t.Fatalf("Label without a timestamp = %q, want the bare id", bare.Label())
	}
	fromRecord := Session{ID: "x", Records: []Record{{Kind: KindUser, At: at}}}
	if !strings.Contains(fromRecord.Label(), "2026") {
		t.Fatalf("Label should fall back to the first record's time, got %q", fromRecord.Label())
	}
}

func TestReplayIsBounded(t *testing.T) {
	isolate(t)
	id := "20260813-091700-aaaaaa"
	j, _ := New(id, time.Now(), Record{})
	for i := 0; i < maxReplayRecords+50; i++ {
		if err := j.Append(Record{Kind: KindUser, Text: "t"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := j.Close(time.Now()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err := Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Records) != maxReplayRecords {
		t.Fatalf("records = %d, want the %d cap", len(s.Records), maxReplayRecords)
	}
	if !s.Truncated {
		t.Fatal("a capped replay must report that older turns were left out")
	}
}

// TestClipTextLeavesTextAtTheLimitAlone pins both sides of the cap, not just a
// value far above it. A guard that is one byte too generous truncates nothing
// visible in a large-input test, but it writes a record one byte over the limit
// and marks a message that fits as intact — so the boundary itself needs a case.
func TestClipTextLeavesTextAtTheLimitAlone(t *testing.T) {
	exact := strings.Repeat("a", maxRecordBytes)
	got, cut := clipText(exact)
	if cut {
		t.Error("text of exactly maxRecordBytes must not be reported as clipped")
	}
	if len(got) != maxRecordBytes {
		t.Errorf("len = %d, want %d: text at the limit must pass through whole", len(got), maxRecordBytes)
	}

	over := exact + "a"
	got, cut = clipText(over)
	if !cut {
		t.Error("one byte over the limit must be reported as clipped")
	}
	if len(got) != maxRecordBytes {
		t.Errorf("len = %d, want %d: the clip must bring text down to the limit", len(got), maxRecordBytes)
	}
}
