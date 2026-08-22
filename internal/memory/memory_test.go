package memory

import (
	"strings"
	"testing"
)

func TestRememberAndRecall(t *testing.T) {
	s := New(t.TempDir())

	if got := s.Remember("stack", "Go 1.22 backend, Bubble Tea TUI"); !strings.Contains(got, "stack") {
		t.Fatalf("remember status: %q", got)
	}
	out := s.Recall("stack")
	if !strings.Contains(out, "Go 1.22") {
		t.Fatalf("recall missing entry: %q", out)
	}
}

func TestCategoryAliases(t *testing.T) {
	s := New(t.TempDir())
	s.Remember("front", "React + Vite")
	s.Remember("back", "net/http + pgx")
	if !strings.Contains(s.Recall("frontend"), "React") {
		t.Fatal("front alias should map to frontend")
	}
	if !strings.Contains(s.Recall("backend"), "pgx") {
		t.Fatal("back alias should map to backend")
	}
}

func TestDedup(t *testing.T) {
	s := New(t.TempDir())
	s.Remember("stack", "uses Postgres")
	got := s.Remember("stack", "uses Postgres") // exact dup
	if !strings.Contains(got, "already known") {
		t.Fatalf("expected dedup, got %q", got)
	}
	// Near-duplicate (superset) should also be rejected.
	got = s.Remember("stack", "uses Postgres")
	if !strings.Contains(got, "already known") {
		t.Fatalf("expected near-dedup, got %q", got)
	}
	entries, _ := s.readEntries("stack")
	if len(entries) != 1 {
		t.Fatalf("want 1 entry after dedup, got %d", len(entries))
	}
}

func TestEntryLengthCap(t *testing.T) {
	s := New(t.TempDir())
	long := strings.Repeat("x", maxEntryLen+200)
	s.Remember("conventions", long)
	entries, _ := s.readEntries("conventions")
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if len([]rune(entries[0])) > maxEntryLen+1 { // +1 for the ellipsis
		t.Fatalf("entry not capped: %d runes", len([]rune(entries[0])))
	}
}

func TestEntryCountCapEvictsOldest(t *testing.T) {
	s := New(t.TempDir())
	for i := 0; i < maxEntriesChanges+10; i++ {
		s.Remember("changes", "change number "+itoa(i))
	}
	entries, _ := s.readEntries("changes")
	if len(entries) != maxEntriesChanges {
		t.Fatalf("want %d entries, got %d", maxEntriesChanges, len(entries))
	}
	// Oldest ("change number 0") must have been evicted; newest kept.
	if strings.Contains(strings.Join(entries, "\n"), "change number 0 ") {
		t.Fatal("oldest entry should have been evicted")
	}
	if !strings.Contains(entries[len(entries)-1], itoa(maxEntriesChanges+9)) {
		t.Fatal("newest entry should be retained")
	}
}

func TestDigestBudget(t *testing.T) {
	s := New(t.TempDir())
	for _, cat := range canonical {
		for i := 0; i < 30; i++ {
			s.Remember(cat, cat+" fact "+itoa(i)+" "+strings.Repeat("y", 100))
		}
	}
	d := s.Digest()
	if len(d) > digestBudget {
		t.Fatalf("digest exceeds budget: %d > %d", len(d), digestBudget)
	}
	if d == "" {
		t.Fatal("digest should not be empty")
	}
}

func TestEmptyRecall(t *testing.T) {
	s := New(t.TempDir())
	if got := s.Recall(""); !strings.Contains(got, "empty") {
		t.Fatalf("empty recall should report emptiness, got %q", got)
	}
	if s.HasContent() {
		t.Fatal("fresh store should have no content")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
