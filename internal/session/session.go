// Package session is the CLI's durable session journal (roadmap §8, row
// "Сессии").
//
// The row asks for three things at once: an addressable session id, a journal at
// ~/.qeuro/sessions/<id>.jsonl fsynced on every event, and recovery that
// continues from the same place after a crash. They are one feature. An id that
// cannot be resumed is a label; and a journal flushed only when the OS feels
// like it is empty in exactly the case it exists for — the process dying without
// a chance to clean up.
//
// This replaces the transcript internal/memory used to keep in
// .infinity/sessions. Two transcripts of one conversation drift, and that one
// could not be replayed: it stored the *rendered* reply (ANSI escapes, hard
// wrapping) and collapsed every run of whitespace, so restoring it fed the model
// terminal control codes instead of the text it had produced. Project memory
// keeps curated knowledge; this package keeps the conversation. One owner each.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"qeuro/internal/clientcfg"
)

const (
	// DirName is the journal directory, beside config.json and mcp.json.
	DirName = "sessions"

	// ext is the journal extension. One JSON object per line: a record is
	// complete the moment its newline is on disk, which is what makes a
	// half-written tail recoverable instead of fatal.
	ext = ".jsonl"

	// idTimeLayout is the sortable prefix of a session id, so "the newest
	// session" is a sort of names rather than a stat of every file.
	idTimeLayout = "20060102-150405"

	// maxRecordBytes caps one journal record. A pasted file can reach the
	// textarea's 16k limit and a tool result is larger still; the cap keeps a
	// single turn from writing an unbounded line that no reader can budget for.
	maxRecordBytes = 64 << 10

	// maxJournalBytes caps one session file. Reached, the journal stops growing
	// and says so rather than filling the user's disk over a long agent run.
	maxJournalBytes = 8 << 20

	// maxReplayRecords bounds a replay. A journal is attacker-influenced only in
	// the weak sense (the user's own transcript), but resume feeds it back to the
	// model, so the cost of one resume has to be bounded by construction.
	maxReplayRecords = 2000

	// maxSessionFiles bounds how many journals are kept. Older ones are removed
	// on open: an agent CLI opens a session per run, and nothing else would ever
	// delete them.
	maxSessionFiles = 200
)

// Kind classifies a journal record. Only user and assistant records replay into
// a conversation; the rest are provenance, kept because a transcript that drops
// what interrupted it cannot explain a truncated reply.
type Kind string

const (
	KindMeta      Kind = "meta"      // session header: version, cwd, model
	KindUser      Kind = "user"      // a user turn, verbatim as sent to the model
	KindAssistant Kind = "assistant" // a completed assistant reply, unrendered
	KindPartial   Kind = "partial"   // text streamed before a cancel/crash
	KindError     Kind = "error"     // a turn that ended in an error
	KindResume    Kind = "resume"    // this session replayed another one
	KindEnd       Kind = "end"       // clean shutdown marker
)

// replayable reports whether a record becomes a conversation message on resume.
func (k Kind) replayable() bool { return k == KindUser || k == KindAssistant }

// Record is one journal line.
//
// Field names are short because every record is re-encoded on every event and a
// journal is read by humans debugging a resume. Time is RFC3339 in UTC: a local
// timestamp in a file that outlives a timezone change is unreadable.
type Record struct {
	At   time.Time `json:"at"`
	Kind Kind      `json:"kind"`
	Text string    `json:"text,omitempty"`

	// Meta fields, set only on KindMeta.
	Version string `json:"version,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Model   string `json:"model,omitempty"`

	// Truncated marks a record whose Text was cut to maxRecordBytes, so a
	// replayed conversation cannot silently claim to be complete.
	Truncated bool `json:"truncated,omitempty"`
}

// ErrNoSession reports that no journal matched a resume request.
var ErrNoSession = errors.New("no session journal found")

// errJournalFull is returned by the Append that first exceeds maxJournalBytes,
// so the caller can tell the user the transcript stopped growing. Later appends
// return nil: the condition is permanent for this session, and repeating the
// warning every turn would bury the conversation it is about.
var errJournalFull = errors.New("session journal reached its size limit; later turns are not recorded")

// NewID returns a session id: a sortable timestamp plus 6 random hex chars.
//
// The random suffix is not decoration. Two `qeuro` processes started in the same
// second — a shell loop, a CI matrix, two panes — would otherwise open the same
// file and interleave their turns into one unreadable transcript. It is not a
// secret, so math/rand would do; crypto/rand is used because a collision here
// silently corrupts a user's history and the cost is 3 bytes once per run.
func NewID(now time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read on a healthy system does not fail; if it does, a
		// time-only id is still better than refusing to open a session.
		return now.UTC().Format(idTimeLayout)
	}
	return now.UTC().Format(idTimeLayout) + "-" + hex.EncodeToString(b[:])
}

// Dir returns the journal directory, or "" when the OS reports no config dir —
// in which case journalling is simply absent, never silently redirected
// somewhere world-readable.
func Dir() string {
	d := clientcfg.ConfigDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, DirName)
}

// validID reports whether id is safe to join onto Dir().
//
// `qeuro resume <id>` takes the id from the command line, so this is the one
// place untrusted text reaches a filesystem path (.ai/RULES.md:22). The check is
// an allow-list of what NewID produces — timestamp, hyphens, lowercase hex — not
// a deny-list of separators: on Windows the path grammar includes drive letters
// and ADS colons, and a deny-list of "/" and ".." misses both.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c == '-':
		default:
			return false
		}
	}
	// A name of only hyphens, or one that is a reserved relative path, would
	// pass the character check.
	if strings.Trim(id, "-") == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		// Reserved DOS device names still resolve inside any directory, so
		// "sessions/con.jsonl" opens the console, not a file.
		switch base := id; base {
		case "con", "prn", "aux", "nul":
			return false
		}
		if len(id) == 4 && (strings.HasPrefix(id, "com") || strings.HasPrefix(id, "lpt")) &&
			id[3] >= '1' && id[3] <= '9' {
			return false
		}
	}
	return true
}
