package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session is a journal read back from disk.
type Session struct {
	ID      string
	Path    string
	Meta    Record
	Records []Record

	// Crashed reports that the journal has no end marker: the process died
	// without closing it. This is the state the row's "recovery after a crash"
	// clause is about, and resume tells the user which case they are in — a clean
	// exit and a crash restore differently in the user's head even when the
	// mechanics are identical.
	Crashed bool

	// Skipped counts lines that did not decode. Almost always exactly one, the
	// partial tail of a killed process. Reported rather than hidden: a resume
	// that quietly drops turns is worse than one that says it dropped them.
	Skipped int

	// Truncated reports that the journal held more records than a replay admits.
	Truncated bool
}

// Turns returns the conversation to restore: user and assistant records in
// order. Partial, error, meta and end records are provenance, not turns.
//
// A trailing partial is not replayed as an assistant turn even though it holds
// real model output: the model never finished it, and feeding a cut-off reply
// back as if it were complete teaches the next turn that a truncated answer was
// accepted. It is surfaced to the user instead (see PartialTail).
func (s Session) Turns() []Record {
	var out []Record
	for _, r := range s.Records {
		if r.Kind.replayable() {
			out = append(out, r)
		}
	}
	return out
}

// PartialTail returns the text of a partial reply at the very end of the
// journal, or "". That is the answer the user lost to a crash or a cancel, and
// showing it is the difference between "your work is gone" and "here is where
// you were".
func (s Session) PartialTail() string {
	for i := len(s.Records) - 1; i >= 0; i-- {
		switch s.Records[i].Kind {
		case KindPartial:
			return s.Records[i].Text
		case KindUser, KindAssistant:
			return ""
		}
	}
	return ""
}

// Load reads one journal by id.
func Load(id string) (Session, error) {
	dir := Dir()
	if dir == "" {
		return Session{}, ErrNoSession
	}
	if !validID(id) {
		return Session{}, fmt.Errorf("invalid session id")
	}
	return loadFile(dir, id)
}

// LoadLatest reads the newest journal, optionally excluding one id (the session
// doing the resuming, which is already open and would otherwise always win).
//
// It walks backwards until it finds a journal with at least one turn: a session
// that was started and immediately quit leaves a meta-only journal, and stopping
// at the newest file would make resume report "nothing to restore" while the
// real transcript sits one file below.
func LoadLatest(exclude string) (Session, error) {
	dir := Dir()
	if dir == "" {
		return Session{}, ErrNoSession
	}
	names, err := journalNames(dir)
	if err != nil {
		return Session{}, ErrNoSession
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, id := range names {
		if id == exclude {
			continue
		}
		s, err := loadFile(dir, id)
		if err != nil {
			continue
		}
		if len(s.Turns()) > 0 {
			return s, nil
		}
	}
	return Session{}, ErrNoSession
}

// loadFile parses one journal, tolerating a damaged tail.
func loadFile(dir, id string) (Session, error) {
	path := filepath.Join(dir, id+ext)
	// #nosec G304 -- id passed validID (timestamp/hex/hyphen only) and is joined
	// onto the journal directory; the path cannot escape it.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, ErrNoSession
		}
		return Session{}, err
	}
	defer f.Close()

	s := Session{ID: id, Path: path, Crashed: true}
	sc := bufio.NewScanner(f)
	// A record is capped at maxRecordBytes plus JSON overhead; give the scanner
	// room for the encoded form so a legitimate long line is not reported as
	// corruption. Without this the default 64k limit would reject records the
	// writer was allowed to write.
	sc.Buffer(make([]byte, 0, 64<<10), maxRecordBytes*2)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			s.Skipped++
			continue
		}
		switch rec.Kind {
		case KindMeta:
			s.Meta = rec
			continue
		case KindEnd:
			s.Crashed = false
			continue
		case KindUser, KindAssistant, KindPartial, KindError, KindResume:
		default:
			// An unknown kind is a journal from a newer CLI. Skip the record
			// rather than the file: the known ones still replay.
			s.Skipped++
			continue
		}
		if len(s.Records) >= maxReplayRecords {
			s.Truncated = true
			break
		}
		s.Records = append(s.Records, rec)
	}
	if err := sc.Err(); err != nil {
		// A line longer than the buffer, or a read error: keep what parsed. The
		// journal is the user's only copy, so partial recovery beats none.
		s.Skipped++
	}
	return s, nil
}

// Label returns a short human description of a session for a status line.
func (s Session) Label() string {
	when := s.Meta.At
	if when.IsZero() && len(s.Records) > 0 {
		when = s.Records[0].At
	}
	if when.IsZero() {
		return s.ID
	}
	return s.ID + " · " + when.Local().Format("2006-01-02 15:04")
}

// List returns the known sessions, newest first, for `qeuro resume` with no id.
// Each entry is parsed, because "how many turns" and "did it crash" are the two
// facts that let a user pick one, and neither is in the filename.
func List(limit int) []Session {
	dir := Dir()
	if dir == "" {
		return nil
	}
	names, err := journalNames(dir)
	if err != nil {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	out := make([]Session, 0, len(names))
	for _, id := range names {
		s, err := loadFile(dir, id)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Age formats how long ago a session ran, for the list.
func Age(s Session, now time.Time) string {
	when := s.Meta.At
	if when.IsZero() {
		return ""
	}
	d := now.Sub(when)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
