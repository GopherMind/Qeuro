package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write is a test helper: a journal with the given records, closed or crashed.
func write(t *testing.T, id string, clean bool, recs ...Record) {
	t.Helper()
	j, err := New(id, time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC), Record{Version: "test", Dir: "/w"})
	if err != nil {
		t.Fatalf("New(%s): %v", id, err)
	}
	for _, r := range recs {
		if err := j.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if clean {
		if err := j.Close(time.Now()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return
	}
	// A crash: the process goes away without the end marker. Closing the file
	// descriptor without appending KindEnd is exactly that state on disk.
	j.mu.Lock()
	if j.f != nil {
		_ = j.f.Close()
		j.f = nil
	}
	j.mu.Unlock()
}

func TestCrashedSessionIsRecognisedAndReplayed(t *testing.T) {
	isolate(t)
	id := "20260813-090001-aaaaaa"
	write(t, id, false,
		Record{Kind: KindUser, Text: "first question"},
		Record{Kind: KindAssistant, Text: "first answer"},
		Record{Kind: KindUser, Text: "second question"},
		Record{Kind: KindPartial, Text: "half an ans"},
	)

	s, err := Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.Crashed {
		t.Fatal("a journal without an end marker must report Crashed")
	}
	turns := s.Turns()
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3 (partial is not a turn)", len(turns))
	}
	if turns[0].Kind != KindUser || turns[0].Text != "first question" {
		t.Fatalf("first turn wrong: %+v", turns[0])
	}
	if turns[2].Text != "second question" {
		t.Fatalf("replay must end on the unanswered question, got %q", turns[2].Text)
	}
	// The lost answer is recoverable for the user without being replayed as a
	// finished turn.
	if got := s.PartialTail(); got != "half an ans" {
		t.Fatalf("PartialTail = %q, want the cut-off text", got)
	}
}

func TestCleanSessionIsNotReportedAsCrashed(t *testing.T) {
	isolate(t)
	id := "20260813-090002-bbbbbb"
	write(t, id, true, Record{Kind: KindUser, Text: "q"}, Record{Kind: KindAssistant, Text: "a"})
	s, err := Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Crashed {
		t.Fatal("a journal closed with an end marker must not report Crashed")
	}
	if len(s.Turns()) != 2 {
		t.Fatalf("turns = %d, want 2", len(s.Turns()))
	}
}

func TestTruncatedTailIsSkippedNotFatal(t *testing.T) {
	dir := isolate(t)
	id := "20260813-090003-cccccc"
	write(t, id, false,
		Record{Kind: KindUser, Text: "kept"},
		Record{Kind: KindAssistant, Text: "also kept"},
	)
	// Simulate a process killed mid-write: append a half-encoded record.
	path := filepath.Join(dir, id+ext)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"at":"2026-08-13T09:00:04Z","kind":"user","text":"lo`); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	s, err := Load(id)
	if err != nil {
		t.Fatalf("a damaged tail must not fail the load: %v", err)
	}
	if len(s.Turns()) != 2 {
		t.Fatalf("turns = %d, want the 2 complete records", len(s.Turns()))
	}
	if s.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1: a dropped turn must be reported, not hidden", s.Skipped)
	}
}

func TestPartialTailOnlyWhenItIsTheTail(t *testing.T) {
	isolate(t)
	id := "20260813-090005-dddddd"
	write(t, id, true,
		Record{Kind: KindUser, Text: "q1"},
		Record{Kind: KindPartial, Text: "cancelled answer"},
		Record{Kind: KindUser, Text: "q2"},
		Record{Kind: KindAssistant, Text: "a2"},
	)
	s, _ := Load(id)
	// An earlier cancel that the user then worked past is history, not a lost
	// answer to offer back.
	if got := s.PartialTail(); got != "" {
		t.Fatalf("PartialTail = %q, want empty when a later turn completed", got)
	}
}

func TestLoadLatestSkipsCurrentAndEmptySessions(t *testing.T) {
	isolate(t)
	write(t, "20260813-090010-aaaaaa", true, Record{Kind: KindUser, Text: "old"})
	write(t, "20260813-090020-bbbbbb", true, Record{Kind: KindUser, Text: "wanted"})
	// A launch that recorded nothing but its resume marker: newest by name, but
	// stopping here would report "nothing to restore" with a real transcript one
	// file below.
	write(t, "20260813-090030-cccccc", true, Record{Kind: KindResume, Text: "resumed from x"})
	current := "20260813-090040-dddddd"
	write(t, current, false, Record{Kind: KindUser, Text: "this session"})

	s, err := LoadLatest(current)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if s.ID != "20260813-090020-bbbbbb" {
		t.Fatalf("LoadLatest picked %s, want the newest session with turns", s.ID)
	}
	if len(s.Turns()) != 1 || s.Turns()[0].Text != "wanted" {
		t.Fatalf("wrong session content: %+v", s.Turns())
	}
}

func TestLoadRejectsUnsafeIDAndMissingSession(t *testing.T) {
	isolate(t)
	if _, err := Load("../../etc/passwd"); err == nil {
		t.Fatal("Load must reject an id that is not a journal name")
	}
	_, err := Load("20260813-090099-eeeeee")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing session error = %v, want ErrNoSession", err)
	}
	if _, err := LoadLatest(""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("LoadLatest on an empty dir = %v, want ErrNoSession", err)
	}
}

func TestListIgnoresForeignFiles(t *testing.T) {
	dir := isolate(t)
	write(t, "20260813-090050-aaaaaa", true, Record{Kind: KindUser, Text: "real"})
	// A file dropped in by hand: its name would reach a path join, so it is not
	// offered as a resumable session.
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "UPPER.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := List(10)
	if len(got) != 1 || got[0].ID != "20260813-090050-aaaaaa" {
		t.Fatalf("List = %+v, want only the valid journal", got)
	}
}

func TestUnknownKindIsSkippedNotFatal(t *testing.T) {
	dir := isolate(t)
	id := "20260813-090060-aaaaaa"
	write(t, id, true, Record{Kind: KindUser, Text: "known"})
	path := filepath.Join(dir, id+ext)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	// A journal written by a newer CLI: the known records must still replay.
	_, _ = f.WriteString(`{"at":"2026-08-13T09:00:61Z","kind":"future","text":"x"}` + "\n")
	_ = f.Close()

	s, err := Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Turns()) != 1 {
		t.Fatalf("turns = %d, want the known record", len(s.Turns()))
	}
	if s.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", s.Skipped)
	}
}

func TestRecordTextIsClippedOnRuneBoundary(t *testing.T) {
	isolate(t)
	id := "20260813-090070-aaaaaa"
	// Cyrillic: a byte-boundary cut would split a rune and the whole record would
	// fail to decode, losing the turn rather than its tail.
	//
	// The leading "a" matters. maxRecordBytes is even and "я" is two bytes, so an
	// all-Cyrillic string happens to be cut on a rune boundary anyway, and a
	// byte-boundary clip would pass. The odd offset is what makes the cut land
	// inside a character.
	long := "a" + strings.Repeat("я", maxRecordBytes)
	write(t, id, true, Record{Kind: KindUser, Text: long})

	s, err := Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	turns := s.Turns()
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1: the clipped record must still decode", len(turns))
	}
	if !turns[0].Truncated {
		t.Fatal("a clipped record must be marked Truncated")
	}
	if len(turns[0].Text) > maxRecordBytes {
		t.Fatalf("text is %d bytes, want <= %d", len(turns[0].Text), maxRecordBytes)
	}
	if !strings.HasSuffix(turns[0].Text, "я") {
		t.Fatal("clip must land on a rune boundary")
	}
}

func TestPruneKeepsNewestJournals(t *testing.T) {
	dir := isolate(t)
	// prune runs on open, so write maxSessionFiles+2 journals and check the two
	// oldest are gone and the newest survived.
	for i := 0; i < maxSessionFiles+2; i++ {
		id := "20260813-" + pad(i) + "-aaaaaa"
		write(t, id, true, Record{Kind: KindUser, Text: "t"})
	}
	names, err := journalNames(dir)
	if err != nil {
		t.Fatalf("journalNames: %v", err)
	}
	if len(names) > maxSessionFiles {
		t.Fatalf("kept %d journals, want <= %d", len(names), maxSessionFiles)
	}
	if _, err := os.Stat(filepath.Join(dir, "20260813-"+pad(0)+"-aaaaaa"+ext)); !os.IsNotExist(err) {
		t.Fatal("the oldest journal should have been pruned")
	}
	newest := "20260813-" + pad(maxSessionFiles+1) + "-aaaaaa"
	if _, err := os.Stat(filepath.Join(dir, newest+ext)); err != nil {
		t.Fatalf("the newest journal must survive pruning: %v", err)
	}
}

// pad renders i as a 6-digit, sortable pseudo-time component.
func pad(i int) string {
	s := "000000" + itoa(i)
	return s[len(s)-6:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestAgeFormatsRelativeTime(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
		// Each threshold, from both sides. A list of comfortably-inside values
		// leaves every boundary free to move: widening the day cutoff would
		// render yesterday's session as "30h ago" and nothing above would notice.
		{now.Add(-59*time.Second - 999*time.Millisecond), "just now"},
		{now.Add(-time.Minute), "1m ago"},
		{now.Add(-59 * time.Minute), "59m ago"},
		{now.Add(-time.Hour), "1h ago"},
		{now.Add(-23*time.Hour - 59*time.Minute), "23h ago"},
		{now.Add(-24 * time.Hour), "1d ago"},
		{now.Add(-30 * time.Hour), "1d ago"},
	}
	for _, c := range cases {
		got := Age(Session{Meta: Record{At: c.at}}, now)
		if got != c.want {
			t.Errorf("Age(%v) = %q, want %q", c.at, got, c.want)
		}
	}
	if got := Age(Session{}, now); got != "" {
		t.Errorf("Age with no timestamp = %q, want empty", got)
	}
}
