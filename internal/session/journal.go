package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Journal is an append-only session log fsynced on every record.
//
// A nil *Journal is usable and does nothing. That is deliberate: the caller is a
// TUI turn, and "the config directory is unavailable" must degrade to "no
// journal", never to a turn that fails to run.
//
// The file is created on the first record, not on New. Creating it eagerly would
// leave a turn-less journal behind every `qeuro` launch that only looked at
// something and quit, and since the directory is capped, 200 such launches would
// evict the transcripts the cap exists to preserve. It also keeps a mkdir, a
// create and an fsync out of the startup path that the same roadmap section
// budgets at 50 ms.
type Journal struct {
	id   string
	path string
	meta Record

	mu       sync.Mutex
	f        *os.File
	started  bool   // the file has been created and meta written
	bytes    int64  // bytes written, against maxJournalBytes
	full     bool   // cap reached; stop appending
	writeErr string // first write failure, reported once to the caller
}

// New returns the journal for a session id. It touches no disk.
//
// A nil Journal (and no error) means there is no config directory to write to:
// the row is about durability of a session that exists, not about refusing to
// start. An invalid id is an error, because every id comes from NewID and one
// that does not is a bug, not a user's environment.
func New(id string, now time.Time, meta Record) (*Journal, error) {
	if Dir() == "" {
		return nil, nil
	}
	if !validID(id) {
		return nil, fmt.Errorf("invalid session id")
	}
	meta.Kind = KindMeta
	meta.At = now
	meta.Text = ""
	return &Journal{
		id:   id,
		path: filepath.Join(Dir(), id+ext),
		meta: meta,
	}, nil
}

// startLocked creates the journal file and writes the meta record. Callers hold
// j.mu.
func (j *Journal) startLocked() error {
	if j.started {
		return nil
	}
	// Mark started before any I/O: a failing filesystem must not retry mkdir and
	// create on every turn of a long session.
	j.started = true

	dir := Dir()
	if dir == "" {
		return fmt.Errorf("no config directory for the session journal")
	}
	// 0o700: a transcript contains everything the user typed and everything the
	// model answered. On a shared host that is the whole point of the directory
	// being private.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// O_EXCL: an id collision must fail loudly rather than append this session's
	// turns to somebody else's transcript.
	f, err := os.OpenFile(j.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	j.f = f

	if err := j.appendLocked(j.meta); err != nil {
		return err
	}
	// Pruning happens after the new journal exists, so an interruption during
	// pruning cannot leave the current session without a file.
	prune(dir, maxSessionFiles)
	return nil
}

// ID returns the session id, for display and for `qeuro resume <id>`.
func (j *Journal) ID() string {
	if j == nil {
		return ""
	}
	return j.id
}

// Path returns the journal file path, for display.
func (j *Journal) Path() string {
	if j == nil {
		return ""
	}
	return j.path
}

// Append writes one record and fsyncs it.
//
// fsync per record is the row's requirement and it is the right trade here: a
// turn already costs a network round trip, so one flush is invisible, while the
// alternative loses exactly the last turn — the one the user was working on when
// the process died.
func (j *Journal) Append(rec Record) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.startLocked(); err != nil {
		j.recordErrLocked(err)
		return err
	}
	return j.appendLocked(rec)
}

// appendLocked writes and fsyncs one record; callers hold j.mu and have started
// the file.
func (j *Journal) appendLocked(rec Record) error {
	if j.f == nil || j.full {
		return nil
	}

	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	rec.At = rec.At.UTC()
	rec.Text, rec.Truncated = clipText(rec.Text)

	line, err := json.Marshal(rec)
	if err != nil {
		// Records are built in this package from strings; a marshal failure is
		// not reachable through the API, but swallowing it would hide a future
		// field that is not encodable.
		return err
	}
	line = append(line, '\n')

	if j.bytes+int64(len(line)) > maxJournalBytes {
		// Report the drop that fills the journal, and only that one: the guard at
		// the top of this function returns early once j.full is set, so every
		// later record is dropped silently. That split is deliberate — the
		// conversation on screen keeps going, so warning on every turn from here
		// on would bury it, while saying nothing at all means the user finds out
		// by trying to resume and finding the end of the transcript missing.
		j.full = true
		return errJournalFull
	}

	n, err := j.f.Write(line)
	j.bytes += int64(n)
	if err != nil {
		j.recordErrLocked(err)
		return err
	}
	// A short write leaves a truncated line: the record is lost, but the parser
	// skips it, so the rest of the journal stays readable.
	if n != len(line) {
		err := fmt.Errorf("short write: %d of %d bytes", n, len(line))
		j.recordErrLocked(err)
		return err
	}
	if err := syncFile(j.f); err != nil {
		j.recordErrLocked(err)
		return err
	}
	return nil
}

// syncFile is the fsync seam. The per-record flush is this row's whole point,
// and it is the one part of the journal a test cannot otherwise observe: after a
// plain Write the page cache serves the same bytes back, so removing the flush
// changes nothing a reader in the same process can see — only what survives the
// machine losing power. One indirection makes the call assertable; production
// always goes through (*os.File).Sync.
var syncFile = func(f *os.File) error { return f.Sync() }

// recordErrLocked keeps the first write failure; callers hold j.mu.
func (j *Journal) recordErrLocked(err error) {
	if j.writeErr == "" {
		j.writeErr = err.Error()
	}
}

// Err returns the first write failure, if any. The TUI surfaces it once instead
// of on every turn: a full disk produces an error per record, and a status line
// repeating it would bury the conversation.
func (j *Journal) Err() string {
	if j == nil {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.full && j.writeErr == "" {
		return errJournalFull.Error()
	}
	return j.writeErr
}

// Close writes the end marker and closes the file. A journal without an end
// marker is how a reader knows the session crashed rather than exited.
//
// A journal that was never started stays absent: writing an end marker would
// create the very empty file the lazy start avoids.
func (j *Journal) Close(now time.Time) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.started || j.f == nil {
		return nil
	}
	_ = j.appendLocked(Record{Kind: KindEnd, At: now})
	err := j.f.Close()
	j.f = nil
	return err
}

// clipText caps a record's text at maxRecordBytes on a rune boundary.
//
// Cutting at a byte boundary would split a multi-byte character, and the whole
// record then fails to decode — a Cyrillic transcript would lose the turn, not
// the tail.
func clipText(s string) (string, bool) {
	if len(s) <= maxRecordBytes {
		return s, false
	}
	cut := maxRecordBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// prune removes the oldest journals beyond keep. Ids sort chronologically, so
// this is a name sort; a file whose name is not a journal is left alone.
func prune(dir string, keep int) {
	names, err := journalNames(dir)
	if err != nil || len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, name+ext))
	}
}

// journalNames lists the session ids present in dir.
func journalNames(dir string) ([]string, error) {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range dirents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ext) {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ext)
		// A file dropped into the directory by hand is not resumable and must
		// not be offered: its name reaches a path join in Load.
		if !validID(id) {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
