// Package memory is the CLI's local, file-based project memory. It lives in a
// .infinity/ folder at the project root and gives the agent a durable, curated
// knowledge base that survives across sessions — so after a week of work the
// agent recalls the stack, architecture and conventions without the user
// re-explaining them.
//
// Two ideas keep it useful without bloating the context window:
//
//   - Curation over volume. Entries are single, concise lines (hard length
//     cap). Duplicates and near-duplicates are rejected. Each category file is
//     capped; the oldest entries fall off. The agent is told to write only
//     durable, important facts in few precise words.
//   - Bounded recall. The digest injected into every request has a fixed
//     character budget.
//
// Layout:
//
//	.infinity/
//	  memory/
//	    stack.md          tech stack, versions, key dependencies
//	    architecture.md   how the system fits together
//	    frontend.md       frontend structure & decisions
//	    backend.md        backend structure & decisions
//	    conventions.md    coding conventions, patterns, gotchas
//	    changes.md        log of important changes (rolling)
//
// The conversation transcript is NOT here: it belongs to internal/session, which
// journals it durably to ~/.qeuro/sessions/<id>.jsonl (roadmap §8). This package
// held a second, rendered copy in .infinity/sessions, which drifted from the
// first and could not be replayed.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Tunables that keep memory concise and the injected digest small. These are
// the levers that stop the knowledge base from bloating the context window.
const (
	// maxEntryLen caps a single remembered line, forcing concision.
	maxEntryLen = 240
	// maxEntriesKnowledge caps durable knowledge files (oldest fall off).
	maxEntriesKnowledge = 50
	// maxEntriesChanges caps the rolling change log.
	maxEntriesChanges = 40
	// digestBudget bounds the total characters injected into a request.
	digestBudget = 3500
	// digestPerCategory bounds how many lines of one category enter the digest.
	digestPerCategory = 12
)

// Canonical categories, in the order they appear in the injected digest.
var canonical = []string{
	"stack", "architecture", "frontend", "backend", "conventions", "changes",
}

// aliases maps loose names the model might use onto canonical categories, so
// "front"/"back"/"deps" all land in the right file.
var aliases = map[string]string{
	"front": "frontend", "fe": "frontend", "ui": "frontend", "client": "frontend",
	"back": "backend", "be": "backend", "server": "backend", "api": "backend",
	"deps": "stack", "dependencies": "stack", "tech": "stack", "tooling": "stack",
	"arch": "architecture", "design": "architecture", "structure": "architecture",
	"convention": "conventions", "style": "conventions",
	"patterns": "conventions", "gotchas": "conventions",
	"change": "changes", "changelog": "changes", "history": "changes", "log": "changes",
}

// isKnown reports whether c (already normalized) is a canonical category.
func isKnown(c string) bool {
	for _, k := range canonical {
		if k == c {
			return true
		}
	}
	return false
}

// Store is the local memory rooted at a project directory. It performs no disk
// I/O until something is actually written, so constructing one is free.
//
// A Store is shared between the UI goroutine (Digest/Recall during a solo turn)
// and a team run's worker goroutine (Remember/Recall via the tool runner), so
// every operation that touches the category files is serialized by mu (C4: data
// race).
type Store struct {
	root   string // project root
	dir    string // <root>/.infinity
	memDir string // <root>/.infinity/memory

	mu sync.Mutex // guards the category files
}

// New returns a Store rooted at the project directory. No files are created.
func New(root string) *Store {
	dir := filepath.Join(root, ".infinity")
	return &Store{
		root:   root,
		dir:    dir,
		memDir: filepath.Join(dir, "memory"),
	}
}

// Categories returns the canonical category names (for help/UX).
func Categories() []string { return append([]string(nil), canonical...) }

// normalize maps a free-form category to a canonical, filesystem-safe name. An
// unknown category is sanitized and kept as-is (so the model can introduce a
// new bucket), but anything empty defaults to "notes".
func normalize(category string) string {
	c := strings.ToLower(strings.TrimSpace(category))
	if c == "" {
		return "notes"
	}
	if mapped, ok := aliases[c]; ok {
		return mapped
	}
	if isKnown(c) {
		return c
	}
	// Sanitize an unknown bucket name to a safe single token.
	var b strings.Builder
	for _, r := range c {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "notes"
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// cleanEntry collapses a note to a single concise line and caps its length.
func cleanEntry(note string) string {
	s := strings.TrimSpace(note)
	if s == "" {
		return ""
	}
	// Collapse to one line; squeeze internal whitespace.
	s = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	// Drop a leading bullet if the model added one.
	s = strings.TrimLeft(s, "-*• ")
	if len(s) > maxEntryLen {
		s = strings.TrimSpace(s[:maxEntryLen]) + "…"
	}
	return s
}

// Remember stores one concise fact in a category, deduplicating and capping the
// file. It returns a short status describing what happened.
func (s *Store) Remember(category, note string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	cat := normalize(category)
	entry := cleanEntry(note)
	if entry == "" {
		return "memory: empty entry skipped"
	}

	entries, _ := s.readEntries(cat)

	// Reject exact duplicates, or a new entry that is wholly contained in an
	// existing one at word boundaries (so "uses Postgres" won't be re-added,
	// but sequential facts like "step 1" vs "step 10" stay distinct).
	le := strings.ToLower(entry)
	for _, e := range entries {
		el := strings.ToLower(e)
		if el == le || containsAtBoundary(el, le) || containsAtBoundary(le, el) {
			return "memory: already known — skipped (" + cat + ")"
		}
	}

	entries = append(entries, entry)

	cap := maxEntriesKnowledge
	if cat == "changes" {
		cap = maxEntriesChanges
	}
	dropped := 0
	if len(entries) > cap {
		dropped = len(entries) - cap
		entries = entries[dropped:] // keep most recent
	}

	if err := s.writeEntries(cat, entries); err != nil {
		return "memory: write error — " + err.Error()
	}

	status := fmt.Sprintf("memory: saved to %s (%d entries)", cat, len(entries))
	if dropped > 0 {
		status += fmt.Sprintf(", evicted old entries: %d", dropped)
	}
	return status
}

// Recall returns the content of one category, or the full curated digest when
// category is empty. Intended as a tool result the model reads on demand.
func (s *Store) Recall(category string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(category) == "" {
		d := s.digestLocked()
		if d == "" {
			return "memory is empty — nothing recorded yet"
		}
		return d
	}
	cat := normalize(category)
	entries, err := s.readEntries(cat)
	if err != nil || len(entries) == 0 {
		return "memory: category «" + cat + "» is still empty"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", cat)
	for _, e := range entries {
		b.WriteString("- " + e + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Digest builds the bounded knowledge summary injected into each request. Only
// the canonical knowledge categories are included; the total is hard-capped at
// digestBudget characters so memory can never dominate the context window.
func (s *Store) Digest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digestLocked()
}

// digestLocked is the body of Digest; callers must hold s.mu.
func (s *Store) digestLocked() string {
	var b strings.Builder
	total := 0

	add := func(line string) bool {
		if total+len(line)+1 > digestBudget {
			return false
		}
		b.WriteString(line + "\n")
		total += len(line) + 1
		return true
	}

	for _, cat := range canonical {
		entries, _ := s.readEntries(cat)
		if len(entries) == 0 {
			continue
		}
		// Show the most recent N lines of each category.
		if len(entries) > digestPerCategory {
			entries = entries[len(entries)-digestPerCategory:]
		}
		if !add("## " + cat) {
			break
		}
		full := true
		for _, e := range entries {
			if !add("- " + e) {
				full = false
				break
			}
		}
		if !full {
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// HasContent reports whether any knowledge has been recorded yet.
func (s *Store) HasContent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cat := range canonical {
		if entries, _ := s.readEntries(cat); len(entries) > 0 {
			return true
		}
	}
	return false
}

// containsAtBoundary reports whether sub appears in s as a run of whole words
// (bounded by non-alphanumeric characters or the string ends). This treats
// "uses postgres" as contained in "the app uses postgres heavily" but keeps
// "change number 1" distinct from "change number 10".
func containsAtBoundary(s, sub string) bool {
	if sub == "" || len(sub) > len(s) {
		return false
	}
	from := 0
	for {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			return false
		}
		i += from
		leftOK := i == 0 || !isWordByte(s[i-1])
		end := i + len(sub)
		rightOK := end == len(s) || !isWordByte(s[end])
		if leftOK && rightOK {
			return true
		}
		from = i + 1
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// --- file helpers ---------------------------------------------------------

// readEntries returns the bullet entries of a category file (without the "- "
// prefix or the header). A missing file yields an empty slice and no error.
func (s *Store) readEntries(cat string) ([]string, error) {
	data, err := os.ReadFile(s.path(cat))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimLeft(line, "-*• ")
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// writeEntries rewrites a category file with a header and the given entries.
func (s *Store) writeEntries(cat string, entries []string) error {
	if err := os.MkdirAll(s.memDir, 0o750); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title(cat))
	for _, e := range entries {
		b.WriteString("- " + e + "\n")
	}
	return os.WriteFile(s.path(cat), []byte(b.String()), 0o600)
}

func (s *Store) path(cat string) string {
	return filepath.Join(s.memDir, cat+".md")
}

// title makes a category name presentable in a file header.
func title(cat string) string {
	if cat == "" {
		return "Notes"
	}
	return strings.ToUpper(cat[:1]) + cat[1:]
}

// List returns the categories that currently have content, sorted canonically
// first then alphabetically (for a future /memory screen).
func (s *Store) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := map[string]bool{}
	var known, extra []string
	entries, _ := os.ReadDir(s.memDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		cat := strings.TrimSuffix(e.Name(), ".md")
		seen[cat] = true
	}
	for _, c := range canonical {
		if seen[c] {
			known = append(known, c)
		}
	}
	for c := range seen {
		if !isKnown(c) {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	return append(known, extra...)
}
