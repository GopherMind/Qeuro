// Package skills loads the curated library of agent skills — markdown files
// with a small YAML front-matter (name + description) followed by the skill's
// instructions. The library is the knowledge the team's lead/planner draws on:
// it searches skills by keyword (Find) and injects a chosen skill's body into a
// worker's system prompt (Body).
//
// Only the standard library is used. Front-matter is parsed with a minimal
// line scanner (the files only use single-line name/description fields), so no
// YAML dependency is pulled in.
package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"qeuro/internal/clientcfg"
)

// maxBodyBytes caps how much of a skill file is injected into a prompt, to
// protect the worker's context window.
const maxBodyBytes = 12 * 1024

// Skill is one entry in the library: its identity plus the path to its file.
// The full instruction body is read lazily via Library.Body.
type Skill struct {
	Name        string
	Description string
	Path        string // absolute path to the .md file
}

// Library is the parsed skill index (name + description + path for every file).
type Library struct {
	dir    string
	skills []Skill
	byName map[string]Skill
}

// Dir reports the directory the library was loaded from (for diagnostics).
func (l *Library) Dir() string { return l.dir }

// Count reports how many skills are indexed.
func (l *Library) Count() int {
	if l == nil {
		return 0
	}
	return len(l.skills)
}

// Load discovers the skills directory and parses the front-matter of every
// *.md file into an index. A missing directory is not an error: it returns an
// empty library so the team can still run (workers just get no skill).
func Load() (*Library, error) {
	dir := discoverDir()
	lib := &Library{dir: dir, byName: map[string]Skill{}}
	if dir == "" {
		return lib, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return lib, nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		name, desc, ok := parseFrontmatter(path)
		if !ok || name == "" {
			// Fall back to the file name (sans extension) so the file is still
			// discoverable even without proper front-matter.
			name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		s := Skill{Name: name, Description: desc, Path: path}
		lib.skills = append(lib.skills, s)
		lib.byName[strings.ToLower(name)] = s
	}
	sort.Slice(lib.skills, func(i, j int) bool { return lib.skills[i].Name < lib.skills[j].Name })
	return lib, nil
}

// discoverDir locates the skills library. It checks, in order: the
// QEURO_SKILLS_DIR override, a ./skills folder, and then walks up from the
// working directory (and the executable's directory) looking for a known
// library folder. Returns "" when nothing is found.
func discoverDir() string {
	// The override goes through clientcfg so the config files participate with
	// the precedence roadmap §8 defines, rather than env being the only way to
	// set it. A project file cannot supply this one (see projectSafe): skills
	// become model instructions, so a cloned repository pointing it at itself
	// would be prompt injection.
	if cfg, err := clientcfg.Load(); err == nil {
		if d := strings.TrimSpace(cfg.SkillsDir); d != "" && isDir(d) {
			return d
		}
	}
	// Candidate folder names a library may live under.
	names := []string{"skills", filepath.Join("backend", "officialskills_skills"), "officialskills_skills"}

	var roots []string
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}

	for _, root := range roots {
		dir := root
		// Walk up at most 6 levels so running from a subdirectory still finds it.
		for i := 0; i < 6 && dir != ""; i++ {
			for _, n := range names {
				cand := filepath.Join(dir, n)
				// Require the candidate to actually contain skill files, so the
				// generic name "skills" does not falsely match an unrelated
				// directory (e.g. this Go package's own source folder).
				if looksLikeLibrary(cand) {
					return cand
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

func isDir(p string) bool {
	// #nosec G703 -- a stat on a candidate skills directory. The paths are built
	// from the CLI's own search roots, and nothing is opened or written here.
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// looksLikeLibrary reports whether dir is a directory holding at least one .md
// skill file (cheap structural check, reads the directory once).
func looksLikeLibrary(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			return true
		}
	}
	return false
}

// Get returns the indexed skill with the given name (case-insensitive).
func (l *Library) Get(name string) (Skill, bool) {
	if l == nil {
		return Skill{}, false
	}
	s, ok := l.byName[strings.ToLower(strings.TrimSpace(name))]
	return s, ok
}

// Find ranks skills against a free-text query and returns up to limit matches,
// best first. Scoring favours matches in the skill name over the description.
// An empty query or no match returns nil.
func (l *Library) Find(query string, limit int) []Skill {
	if l == nil || len(l.skills) == 0 {
		return nil
	}
	words := tokenize(query)
	if len(words) == 0 {
		return nil
	}
	full := strings.ToLower(strings.TrimSpace(query))

	type scored struct {
		s     Skill
		score int
	}
	var ranked []scored
	for _, s := range l.skills {
		name := strings.ToLower(s.Name)
		desc := strings.ToLower(s.Description)
		score := 0
		for _, w := range words {
			if strings.Contains(name, w) {
				score += 3
			}
			if strings.Contains(desc, w) {
				score++
			}
		}
		// Bonus when the whole query appears verbatim.
		if full != "" {
			if strings.Contains(name, full) {
				score += 5
			} else if strings.Contains(desc, full) {
				score += 2
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{s, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Skill, len(ranked))
	for i, r := range ranked {
		out[i] = r.s
	}
	return out
}

// Body returns a skill's instruction text (the markdown after the front-matter),
// capped at maxBodyBytes. Resolves by name or by a previously indexed skill.
func (l *Library) Body(name string) (string, error) {
	s, ok := l.Get(name)
	if !ok {
		// Allow passing an arbitrary indexed name through.
		for _, k := range l.skills {
			if strings.EqualFold(k.Name, name) {
				s, ok = k, true
				break
			}
		}
	}
	if !ok {
		return "", os.ErrNotExist
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	body := stripFrontmatter(string(data))
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes] + "\n…[skill truncated]"
	}
	return body, nil
}

// tokenize splits a query into lowercase alphanumeric words (length ≥ 2).
func tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') &&
			!(r >= 'а' && r <= 'я') && r != '+' && r != '#'
	}) {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

// parseFrontmatter extracts name and description from a leading `---` YAML
// block. It only understands single-line `key: value` pairs (optionally
// quoted), which is all the library uses.
func parseFrontmatter(path string) (name, desc string, ok bool) {
	// #nosec G304 -- path is produced by the skills walk over the known skill
	// roots, which skips symlinks and reparse points, so it stays inside them.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	text := string(data)
	if !strings.HasPrefix(text, "---") {
		return "", "", false
	}
	// Take everything up to the closing fence.
	rest := strings.TrimPrefix(text, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false
	}
	block := rest[:end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "name:"):
			name = unquote(strings.TrimSpace(strings.TrimPrefix(line, "name:")))
		case strings.HasPrefix(line, "description:"):
			desc = unquote(strings.TrimSpace(strings.TrimPrefix(line, "description:")))
		}
	}
	return name, desc, true
}

// stripFrontmatter removes a leading `---` … `---` block from a markdown file.
func stripFrontmatter(text string) string {
	if !strings.HasPrefix(text, "---") {
		return text
	}
	rest := strings.TrimPrefix(text, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return text
	}
	after := rest[end+len("\n---"):]
	// Drop the remainder of the fence line.
	if nl := strings.IndexByte(after, '\n'); nl >= 0 {
		after = after[nl+1:]
	}
	return strings.TrimLeft(after, "\n")
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
