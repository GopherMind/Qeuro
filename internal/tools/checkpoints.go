package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	checkpointVersion    = 1
	maxCheckpointDepth   = 256
	checkpointDir        = ".infinity/checkpoints"
	checkpointObjectsDir = "objects"
	checkpointRecordsDir = "records"
	checkpointHead       = "HEAD"
)

// checkpointRecord is immutable once it reaches records/. Its ID covers every
// field except ID itself, so a damaged or edited record is never restored.
type checkpointRecord struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Parent       string `json:"parent,omitempty"`
	RootHash     string `json:"root_hash"`
	Path         string `json:"path"`
	Existed      bool   `json:"existed"`
	Object       string `json:"object,omitempty"`
	Mode         uint32 `json:"mode,omitempty"`
	BaselineTree string `json:"baseline_tree"`
	ExpectedTree string `json:"expected_tree"`
	Tool         string `json:"tool"`
	CreatedAt    string `json:"created_at"`
}

type checkpointStore struct {
	root string
}

func newCheckpointStore(root string) *checkpointStore {
	return &checkpointStore{root: root}
}

// checkpoint commits a complete pre-image record before a workspace write. The
// caller supplies the intended post-image, allowing the immutable record to
// include both tree states without ever rewriting checkpoint metadata.
func (s *checkpointStore) checkpoint(rel, tool string, next []byte, nextExists bool) (checkpointRecord, error) {
	rel = filepath.ToSlash(rel)
	if !safeCheckpointPath(rel) {
		return checkpointRecord{}, errors.New("invalid checkpoint path")
	}
	if err := s.ensureDirs(); err != nil {
		return checkpointRecord{}, err
	}
	baseline, err := workspaceTreeHash(s.root)
	if err != nil {
		return checkpointRecord{}, err
	}
	workspace, err := os.OpenRoot(s.root)
	if err != nil {
		return checkpointRecord{}, err
	}
	defer workspace.Close()
	target := filepath.FromSlash(rel)
	data, err := workspace.ReadFile(target)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return checkpointRecord{}, err
	}
	object := ""
	mode := uint32(0)
	if existed {
		object = hashBytes(data)
		info, err := workspace.Stat(target)
		if err != nil {
			return checkpointRecord{}, err
		}
		mode = uint32(info.Mode().Perm())
		if err := s.writeObject(object, data); err != nil {
			return checkpointRecord{}, err
		}
	}
	expected, err := workspaceTreeHashWithOverlay(s.root, rel, next, nextExists)
	if err != nil {
		return checkpointRecord{}, err
	}
	parent, err := s.head()
	if err != nil {
		return checkpointRecord{}, err
	}
	rec := checkpointRecord{
		Version: checkpointVersion, Parent: parent, RootHash: hashBytes([]byte(s.root)),
		Path: rel, Existed: existed, Object: object, Mode: mode,
		BaselineTree: baseline, ExpectedTree: expected, Tool: tool,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	rec.ID = checkpointID(rec)
	if err := s.writeRecord(rec); err != nil {
		return checkpointRecord{}, err
	}
	// HEAD is updated only after object and immutable record are durable. If the
	// subsequent source write fails, abandon restores the prior pointer.
	if err := s.setHead(rec.ID); err != nil {
		return checkpointRecord{}, err
	}
	return rec, nil
}

// abandon removes a committed-but-unapplied checkpoint from the active lineage.
// Its record/blob remain immutable orphan artifacts, safe for later collection.
func (s *checkpointStore) abandon(rec checkpointRecord) error {
	current, err := workspaceTreeHash(s.root)
	if err != nil {
		return err
	}
	if current != rec.BaselineTree {
		return errors.New("workspace changed while checkpoint was being abandoned")
	}
	id, err := s.head()
	if err != nil {
		return err
	}
	if id != rec.ID {
		return errors.New("checkpoint head changed")
	}
	return s.setHead(rec.Parent)
}

func (s *checkpointStore) undo(resolve func(string) (string, error), ensure func(string) error) (string, bool) {
	id, err := s.head()
	if err != nil {
		return "undo unavailable: " + err.Error(), false
	}
	if id == "" {
		return "nothing to undo", false
	}
	rec, err := s.record(id)
	if err != nil {
		return "undo unavailable: " + err.Error(), false
	}
	if rec.RootHash != hashBytes([]byte(s.root)) {
		return "undo refused: checkpoint belongs to another workspace", false
	}
	current, err := workspaceTreeHash(s.root)
	if err != nil {
		return "undo unavailable: " + err.Error(), false
	}
	// A crash between moving HEAD and the atomic source write leaves the workspace
	// at the baseline. It is safe to discard that never-applied checkpoint; any
	// other state is drift and must not be overwritten.
	if current == rec.BaselineTree {
		if err := s.setHead(rec.Parent); err != nil {
			return "undo unavailable: " + err.Error(), false
		}
		return "undo: discarded unapplied checkpoint for " + rec.Path, true
	}
	if current != rec.ExpectedTree {
		return "undo refused: workspace changed since checkpoint", false
	}
	abs, err := resolve(filepath.FromSlash(rec.Path))
	if err != nil || ensure(abs) != nil {
		return "undo refused: checkpoint target is outside the workspace", false
	}
	if err := checkHardLink(abs); err != nil && !os.IsNotExist(err) {
		return "undo refused: " + err.Error(), false
	}
	workspace, err := os.OpenRoot(s.root)
	if err != nil {
		return "undo unavailable: " + err.Error(), false
	}
	defer workspace.Close()
	target := filepath.FromSlash(rec.Path)
	if rec.Existed {
		data, err := s.object(rec.Object)
		if err != nil {
			return "undo unavailable: " + err.Error(), false
		}
		if err := atomicRootWrite(workspace, target, data, fs.FileMode(rec.Mode)); err != nil {
			return "undo error: " + err.Error(), false
		}
	} else if err := workspace.Remove(target); err != nil && !os.IsNotExist(err) {
		return "undo error: " + err.Error(), false
	}
	got, err := workspaceTreeHash(s.root)
	if err != nil || got != rec.BaselineTree {
		return "undo error: restored tree did not match checkpoint baseline", false
	}
	if err := s.setHead(rec.Parent); err != nil {
		return "undo error: " + err.Error(), false
	}
	if rec.Existed {
		return "undo: file " + rec.Path + " restored", true
	}
	return "undo: file " + rec.Path + " deleted (was created)", true
}

func (s *checkpointStore) depth() int {
	id, err := s.head()
	if err != nil || id == "" {
		return 0
	}
	depth, seen := 0, map[string]bool{}
	for id != "" && depth < maxCheckpointDepth {
		if seen[id] {
			return 0
		}
		seen[id] = true
		rec, err := s.record(id)
		if err != nil || rec.RootHash != hashBytes([]byte(s.root)) {
			return 0
		}
		depth++
		id = rec.Parent
	}
	if id != "" {
		return 0
	}
	return depth
}

func (s *checkpointStore) ensureDirs() error {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	base := filepath.FromSlash(checkpointDir)
	if err := root.MkdirAll(filepath.Join(base, checkpointObjectsDir), 0o700); err != nil {
		return err
	}
	return root.MkdirAll(filepath.Join(base, checkpointRecordsDir), 0o700)
}

func (s *checkpointStore) openCheckpointRoot() (*os.Root, error) {
	workspace, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	checkpoint, openErr := workspace.OpenRoot(filepath.FromSlash(checkpointDir))
	closeErr := workspace.Close()
	if openErr != nil {
		return nil, openErr
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, checkpoint.Close())
	}
	return checkpoint, nil
}

func (s *checkpointStore) head() (string, error) {
	root, err := s.openCheckpointRoot()
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer root.Close()
	data, err := root.ReadFile(checkpointHead)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", nil
	}
	if !validDigest(id) {
		return "", errors.New("invalid checkpoint head")
	}
	return id, nil
}

func (s *checkpointStore) setHead(id string) error {
	root, err := s.openCheckpointRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if id == "" {
		if err := root.Remove(checkpointHead); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if !validDigest(id) {
		return errors.New("invalid checkpoint parent")
	}
	return atomicRootWrite(root, checkpointHead, []byte(id+"\n"), 0o600)
}

func (s *checkpointStore) writeObject(id string, data []byte) error {
	if !validDigest(id) || hashBytes(data) != id {
		return errors.New("invalid checkpoint object")
	}
	root, err := s.openCheckpointRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	path := filepath.Join(checkpointObjectsDir, id)
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		_, err := s.object(id)
		return err
	}
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *checkpointStore) object(id string) ([]byte, error) {
	if !validDigest(id) {
		return nil, errors.New("invalid checkpoint object")
	}
	root, err := s.openCheckpointRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(filepath.Join(checkpointObjectsDir, id))
	if err != nil {
		return nil, err
	}
	if hashBytes(data) != id {
		return nil, errors.New("checkpoint object digest mismatch")
	}
	return data, nil
}

func (s *checkpointStore) writeRecord(rec checkpointRecord) error {
	if err := validateRecord(rec); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	root, err := s.openCheckpointRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	path := filepath.Join(checkpointRecordsDir, rec.ID+".json")
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		got, err := s.record(rec.ID)
		if err != nil || got != rec {
			return errors.New("checkpoint record collision")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *checkpointStore) record(id string) (checkpointRecord, error) {
	var rec checkpointRecord
	if !validDigest(id) {
		return rec, errors.New("invalid checkpoint id")
	}
	root, err := s.openCheckpointRoot()
	if err != nil {
		return rec, err
	}
	defer root.Close()
	data, err := root.ReadFile(filepath.Join(checkpointRecordsDir, id+".json"))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	if err := validateRecord(rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func validateRecord(rec checkpointRecord) error {
	if rec.Version != checkpointVersion || !validDigest(rec.ID) || rec.ID != checkpointID(rec) ||
		!validDigest(rec.RootHash) || !validDigest(rec.BaselineTree) || !validDigest(rec.ExpectedTree) ||
		!safeCheckpointPath(rec.Path) || rec.Tool == "" || len(rec.Tool) > 64 || rec.CreatedAt == "" ||
		(rec.Existed && rec.Mode == 0) {
		return errors.New("invalid checkpoint record")
	}
	if rec.Parent != "" && !validDigest(rec.Parent) {
		return errors.New("invalid checkpoint parent")
	}
	if rec.Existed != (rec.Object != "") {
		return errors.New("invalid checkpoint object state")
	}
	if rec.Object != "" && !validDigest(rec.Object) {
		return errors.New("invalid checkpoint object")
	}
	if _, err := time.Parse(time.RFC3339Nano, rec.CreatedAt); err != nil {
		return errors.New("invalid checkpoint timestamp")
	}
	return nil
}

func checkpointID(rec checkpointRecord) string {
	rec.ID = ""
	data, _ := json.Marshal(rec)
	return hashBytes(data)
}

func validDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func safeCheckpointPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return path != "" && path == clean && path != "." && !strings.HasPrefix(path, "../") &&
		!filepath.IsAbs(filepath.FromSlash(path)) && !strings.HasPrefix(path, ".infinity/")
}

func atomicRootWrite(root *os.Root, path string, data []byte, mode fs.FileMode) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(path), ".checkpoint-"+hex.EncodeToString(nonce[:]))
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(temp, path)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func workspaceTreeHash(root string) (string, error) {
	return workspaceTreeHashWithOverlay(root, "", nil, false)
}

// isEngineStatePath reports whether a workspace-relative path holds state this
// package maintains rather than content the user authored. Such paths are outside
// the tree hash, so their presence or absence never looks like workspace drift and
// never defeats a rollback comparison.
func isEngineStatePath(rel string) bool {
	for _, dir := range [...]string{checkpointDir, isolationDir} {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

func workspaceTreeHashWithOverlay(root, overlayPath string, overlay []byte, overlayExists bool) (string, error) {
	workspace, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer workspace.Close()
	entries := make([]treeEntry, 0)
	foundOverlay := false
	err = fs.WalkDir(workspace.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		// Engine state, not the user's files. Checkpoints were always excluded for
		// this reason; per-writer overlays (roadmap-v3 §4.1, isolate.go) are excluded
		// on the same ground — creating one is not a change to the tree, and a
		// rollback that restored the original hash would otherwise appear to have
		// failed merely because a worker's scratch directory existed.
		if isEngineStatePath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("workspace contains symlink")
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if rel == overlayPath {
			foundOverlay = true
			if overlayExists {
				entries = append(entries, treeEntry{path: rel, data: overlay})
			}
			return nil
		}
		data, err := workspace.ReadFile(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		entries = append(entries, treeEntry{path: rel, data: data})
		return nil
	})
	if err != nil {
		return "", err
	}
	if overlayPath != "" && overlayExists && !foundOverlay {
		entries = append(entries, treeEntry{path: overlayPath, data: overlay})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, entry := range entries {
		writeHashField(h, []byte(entry.path))
		writeHashField(h, entry.data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type treeEntry struct {
	path string
	data []byte
}

func writeHashField(w io.Writer, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(data)
}
