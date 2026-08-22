package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Per-writer working-tree isolation (roadmap-v3 §4.1, C-1).
//
// The row asks for "отдельный `os.Root`-ограниченный worktree на узел", with
// git-worktree named as a later optimization. This implements that as
// copy-on-write rather than a full copy, for a measured reason: the repository
// this runs in has 22937 tracked files, so materializing a tree per writer would
// cost more than the model call it protects. A writer's isolated Runner reads
// through to the base tree and confines every write to a directory of its own, so
// two writers never observe each other's edits — which is the property the row is
// actually after, and the same one a git-worktree would provide.
//
// Why this lives in package tools rather than a new package: every path here has
// to pass the same containment checks as an ordinary tool call (resolve →
// ensureInsideRoot → checkExistingPathComponents → checkCanonicalContainment).
// A separate package would either import unexported internals or reimplement
// them, and a second implementation of path containment is a security defect
// waiting to drift from the first.

// isolationDir is where per-writer overlays live. Under .infinity so it inherits
// the ignore rule the memory and checkpoint stores already rely on, and so a
// crashed run leaves its evidence somewhere a user can find rather than in a
// temp directory the OS may reap.
const isolationDir = ".infinity/worktrees"

// ChangeKind says what a writer did to one path.
type ChangeKind int

const (
	ChangeModified ChangeKind = iota // the path exists in the base and differs
	ChangeCreated                    // the path does not exist in the base
)

func (k ChangeKind) String() string {
	if k == ChangeCreated {
		return "created"
	}
	return "modified"
}

// Change is one path a writer changed in its own tree, with the bytes it ended up
// with. Content is carried rather than re-read on apply so that integration acts
// on exactly what the writer produced, even if something else touched the overlay
// afterwards.
type Change struct {
	Path    string // slash-separated, relative to the tree root
	Kind    ChangeKind
	Content []byte
}

// Isolated returns a Runner that shares this Runner's tree for reading and
// confines every write to its own overlay directory. name identifies the writer
// (its role) and appears in the overlay path, so a half-finished run can be
// inspected by hand.
//
// The returned Runner is a peer, not a child: it has its own checkpoint store and
// its own mutex, because a writer must be able to undo its own work without
// serializing against — or rolling back — another writer's.
//
// Isolating an already-isolated Runner is refused. Nested overlays would need a
// read fall-through chain, and every additional link is another place for the
// containment checks to be applied in the wrong order. Nothing needs it.
func (r *Runner) Isolated(name string) (*Runner, error) {
	if r.base != "" {
		return nil, errors.New("cannot isolate a Runner that is already isolated")
	}
	slug := isolationSlug(name)
	if slug == "" {
		return nil, errors.New("isolated worktree needs a non-empty name")
	}
	dir := filepath.Join(r.root, filepath.FromSlash(isolationDir), slug)
	// A leftover directory from an earlier run of the same role would silently
	// contribute its files to this run's changes, so the overlay always starts
	// empty. This is the one destructive act in this file, and it is bounded to a
	// path this package owns and built from a slug that cannot escape it.
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("cannot clear isolated worktree: %w", err)
	}
	// #nosec G301 -- an overlay of the user's own source tree; the files copied
	// into it are theirs and are meant to stay readable.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create isolated worktree: %w", err)
	}
	iso := &Runner{
		root:        dir,
		base:        r.root,
		checkpoints: newCheckpointStore(dir),
		mem:         r.mem, // memory is shared and already serialized by its own mutex
	}
	return iso, nil
}

// IsIsolated reports whether writes from this Runner are confined to an overlay.
func (r *Runner) IsIsolated() bool { return r.base != "" }

// Root is the directory this Runner writes into: the project tree, or a writer's
// own overlay. Exposed so a caller can tell two writers' trees apart and so an
// overlay can be named in a diagnostic; it is not a way to bypass resolve, which
// remains the only path from a tool argument to a filesystem path.
func (r *Runner) Root() string { return r.root }

// isolationSlug reduces a role name to something safe to use as one path
// component. The name comes from a model-authored plan, so it is untrusted input
// being turned into a filesystem path: everything outside a conservative
// allow-list is dropped rather than escaped, and the result cannot be "", ".",
// ".." or contain a separator.
func isolationSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			// Anything else — separators, dots, spaces, escapes, non-ASCII — is
			// dropped. Dots in particular: allowing them would admit ".." one
			// character at a time.
		}
		if b.Len() >= 48 {
			break
		}
	}
	s := strings.Trim(b.String(), "-_")
	return s
}

// resolveRead returns the path a read should use: the overlay when the writer has
// already materialized the file there, the base tree otherwise. Both candidates
// go through the same containment checks an ordinary resolve applies.
func (r *Runner) resolveRead(rel string) (string, error) {
	over, err := r.resolve(rel)
	if err != nil {
		return "", err
	}
	if r.base == "" {
		return over, nil
	}
	if _, statErr := os.Lstat(over); statErr == nil {
		return over, nil
	}
	return r.resolveBase(rel)
}

// resolveBase resolves rel against the base tree with the same checks resolve
// applies to the overlay. It exists so the fall-through cannot become the one
// path in the package that skips containment.
func (r *Runner) resolveBase(rel string) (string, error) {
	if r.base == "" {
		return "", errors.New("no base tree")
	}
	baseRunner := &Runner{root: r.base}
	return baseRunner.resolve(rel)
}

// materialize copies a file from the base tree into the overlay so that a patch
// applies to the writer's own copy. It is the "copy" in copy-on-write and runs
// once per path per writer.
//
// A path that exists in neither tree is not an error here: write_file creates new
// files, and its own "already exists" check is what decides that case.
func (r *Runner) materialize(rel string) error {
	if r.base == "" {
		return nil
	}
	over, err := r.resolve(rel)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(over); statErr == nil {
		return nil // already the writer's own
	}
	src, err := r.resolveBase(rel)
	if err != nil {
		return err
	}
	if err := checkHardLink(src); err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return nil // absent in the base: nothing to copy up
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	// #nosec G304 -- src came from resolveBase, which applies the same
	// containment checks as every other read in this package.
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(over); dir != "" {
		// #nosec G301 -- see Isolated.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// #nosec G306 -- a copy of the user's own source file, kept readable.
	return os.WriteFile(over, data, 0o644)
}

// Changes lists what this writer changed, sorted by path so that integration is
// deterministic rather than dependent on directory order.
//
// A file whose overlay content equals the base is reported as no change at all: a
// worker that read a file, decided nothing was needed and wrote it back unchanged
// must not create a conflict with a worker that genuinely edited it.
func (r *Runner) Changes() ([]Change, error) {
	if r.base == "" {
		return nil, errors.New("Changes is only meaningful for an isolated Runner")
	}
	overlay, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = overlay.Close() }()

	var out []Change
	err = fs.WalkDir(overlay.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := filepath.ToSlash(path)
		if rel == "." {
			return nil
		}
		// A writer's own checkpoint store is its undo history, not a change to
		// integrate. Same for a nested isolation dir, which cannot occur today but
		// would be silently integrated if it ever did.
		if rel == ".infinity" || strings.HasPrefix(rel, ".infinity/") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("isolated worktree contains a symlink: %s", rel)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, readErr := overlay.ReadFile(filepath.FromSlash(path))
		if readErr != nil {
			return readErr
		}
		kind := ChangeCreated
		if base, baseErr := r.baseContent(rel); baseErr == nil {
			if string(base) == string(data) {
				return nil // materialized but not actually changed
			}
			kind = ChangeModified
		}
		out = append(out, Change{Path: rel, Kind: kind, Content: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// baseContent reads rel from the base tree. A non-nil error means "not present in
// the base", which callers treat as "this path is new".
func (r *Runner) baseContent(rel string) ([]byte, error) {
	src, err := r.resolveBase(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	// #nosec G304 -- resolveBase applies the package's containment checks.
	return os.ReadFile(src)
}

// readDirMerged lists a directory as the writer sees it: the overlay's entries
// followed by the base tree's. The caller de-duplicates by name, so overlay
// entries win — which is what a writer that replaced a file should observe.
//
// A directory that exists in only one of the two trees is not an error; a path
// that exists in neither produces the base tree's error, so the message the model
// gets is about the project rather than about an implementation detail it has no
// way to understand.
func (r *Runner) readDirMerged(rel string) ([]os.DirEntry, error) {
	over, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	overEntries, overErr := os.ReadDir(over)
	if r.base == "" {
		return overEntries, overErr
	}
	baseDir, baseErr := r.resolveBase(rel)
	if baseErr != nil {
		if overErr != nil {
			return nil, baseErr
		}
		return overEntries, nil
	}
	baseEntries, baseErr := os.ReadDir(baseDir)
	if overErr != nil && baseErr != nil {
		return nil, baseErr
	}
	// The isolation directory is an implementation detail of this package and must
	// not appear in a listing of the project root.
	filtered := make([]os.DirEntry, 0, len(baseEntries))
	for _, e := range baseEntries {
		if e.IsDir() && e.Name() == ".infinity" {
			continue
		}
		filtered = append(filtered, e)
	}
	return append(overEntries, filtered...), nil
}

// searchRoots returns the directories a code search must walk, and the tree each
// result should be reported relative to. For an isolated writer that is the
// overlay first (so its own edits are found) and then the base.
func (r *Runner) searchRoots(explicit string) []string {
	if explicit != "" || r.base == "" {
		return nil
	}
	return []string{r.root, r.base}
}

// Discard removes this writer's overlay. Used when its work is rejected, and on
// the error paths of integration, so a failed run does not leave a tree of
// half-applied edits behind.
func (r *Runner) Discard() error {
	if r.base == "" {
		return errors.New("Discard is only meaningful for an isolated Runner")
	}
	return os.RemoveAll(r.root)
}

// Contribution is one writer's finished work, ready to be integrated.
type Contribution struct {
	Writer  string // the role, for messages and for the deterministic order
	Changes []Change
}

// Conflict is two writers changing one path. Reported before anything is applied.
type Conflict struct {
	Path    string
	Writers []string
}

func (c Conflict) String() string {
	return c.Path + " (" + strings.Join(c.Writers, ", ") + ")"
}

// Conflicts reports every path more than one writer changed, sorted by path.
//
// This is separate from Integrate on purpose: roadmap-v3 §4.1 wants conflicting
// writers detected *before* execution, from declared ownership, and this function
// is what the same comparison looks like once the work exists. Keeping it callable
// on its own means the pre-flight check and the post-hoc check cannot disagree
// about what "conflict" means.
func Conflicts(contribs []Contribution) []Conflict {
	byPath := map[string][]string{}
	for _, c := range contribs {
		for _, ch := range c.Changes {
			// A writer that reports the same path twice must not look like two
			// writers in conflict with itself.
			if existing := byPath[ch.Path]; len(existing) > 0 && existing[len(existing)-1] == c.Writer {
				continue
			}
			byPath[ch.Path] = append(byPath[ch.Path], c.Writer)
		}
	}
	var out []Conflict
	for path, writers := range byPath {
		if len(writers) > 1 {
			out = append(out, Conflict{Path: path, Writers: writers})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Integrate applies the writers' work to r's tree in a controlled order and
// returns the paths it wrote, in the order it wrote them.
//
// The order is the caller's: contribs is applied as given, and each writer's own
// changes are already sorted by path. That is the whole point of the row's
// "Integration lead применяет патчи в контролируемом порядке, а не «кто первый
// записал»" — the result must not depend on which worker finished first.
//
// It refuses outright when two writers changed the same path. Applying one and
// discarding the other silently is precisely the failure §40.2 measured; a caller
// that wants a resolution has to make that choice itself, with the Conflicts list
// in hand.
//
// Every write goes through this Runner's ordinary write path, so each one is
// checkpointed and can be undone individually — integration does not become a
// hole in the undo history.
func (r *Runner) Integrate(contribs []Contribution) ([]string, error) {
	if r.base != "" {
		return nil, errors.New("Integrate applies to the project tree, not to an overlay")
	}
	if conflicts := Conflicts(contribs); len(conflicts) > 0 {
		msgs := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			msgs = append(msgs, c.String())
		}
		return nil, fmt.Errorf("conflicting writers, nothing applied: %s", strings.Join(msgs, "; "))
	}

	var applied []string
	for _, c := range contribs {
		for _, ch := range c.Changes {
			if err := r.applyChange(ch); err != nil {
				// Partial application is reported with what was already written, so the
				// caller can undo exactly that much rather than guessing.
				return applied, fmt.Errorf("%s: %s: %w", c.Writer, ch.Path, err)
			}
			applied = append(applied, ch.Path)
		}
	}
	return applied, nil
}

// applyChange writes one change into r's tree through the checkpointed path.
func (r *Runner) applyChange(ch Change) error {
	abs, err := r.resolve(ch.Path)
	if err != nil {
		return err
	}
	if err := checkHardLink(abs); err != nil {
		return err
	}
	if dir := filepath.Dir(abs); dir != "" {
		// #nosec G301 -- see Isolated.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tool := ToolPatchFile
	if ch.Kind == ChangeCreated {
		tool = ToolWriteFile
	}
	rec, err := r.checkpointFile(ch.Path, tool, ch.Content, true)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	// #nosec G306 -- integrating the user's own source file; kept readable.
	if err := os.WriteFile(abs, ch.Content, 0o644); err != nil {
		if abandonErr := r.checkpoints.abandon(rec); abandonErr != nil {
			return fmt.Errorf("%v; checkpoint unavailable: %v", err, abandonErr)
		}
		return err
	}
	return nil
}

// TreeHash is the hash of the whole tree, including untracked files. Exposed so a
// caller can prove that a rollback after a parallel run restored the original
// tree, which roadmap-v3 §4.1 names as part of the gate.
func (r *Runner) TreeHash() (string, error) { return workspaceTreeHash(r.root) }
