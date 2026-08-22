package clientcfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Roadmap §8 row "Конфиг": flags override env, env overrides `./.qeuro.toml`,
// which overrides `~/.qeuro/config.toml`. The reason the roadmap pairs this with
// `qeuro config doctor` is that precedence is only useful if it is *visible* —
// "my setting is being ignored and I cannot tell which layer won" is the support
// thread this row exists to prevent. So resolution records the origin of every
// value as it goes, rather than reconstructing it afterwards.

// Layer identifies where a value came from. Ordered lowest to highest
// precedence; the zero value is the built-in default.
type Layer int

const (
	LayerDefault Layer = iota
	LayerUserFile
	LayerProjectFile
	LayerEnv
	LayerFlag
)

func (l Layer) String() string {
	switch l {
	case LayerFlag:
		return "flag"
	case LayerEnv:
		return "env"
	case LayerProjectFile:
		return "project file"
	case LayerUserFile:
		return "user file"
	default:
		return "default"
	}
}

// setting describes one configurable value: the TOML key, the env var that
// overrides it, and whether its value may be printed.
//
// `secret` is not cosmetic. `config doctor` is the command a user runs when
// something is wrong, which is exactly when they are most likely to paste its
// output into an issue or a chat. A token in that output is a leaked credential,
// so doctor prints presence and provenance for secrets and never the value.
type setting struct {
	key    string
	env    string
	kind   valueKind
	secret bool
	desc   string

	// projectSafe allows the setting in `./.qeuro.toml`.
	//
	// That file is not the user's: it arrives with a cloned repository, so it is
	// attacker-controlled input that the CLI reads before the user has looked at
	// anything. Most of this surface is unsafe in those hands:
	//
	//   base_url    — the CLI sends the bearer token in every request, so a repo
	//                 that redirects it exfiltrates the user's credential and
	//                 every prompt to a host of its choosing.
	//   console_url — where signup opens; redirecting it is phishing.
	//   token       — a repo choosing whose account is billed.
	//   auto_approve— removes the human from the loop before a write or a
	//                 command, which `.ai/RULES.md:24` prohibits, and roadmap
	//                 §5.3 allows only inside the isolated runner.
	//   skills_dir  — skills become model instructions, so a repo pointing this
	//                 inside itself is prompt injection with extra steps.
	//
	// What is left is `model`, which is also the row's motivating use case: a
	// repository declaring the model its codebase wants. A restricted key found
	// in a project file is reported, not silently dropped — silence would make
	// the setting look applied.
	projectSafe bool
}

type valueKind int

const (
	kindString valueKind = iota
	kindBool
)

// settingSpecs is the authoritative list. `doctor` iterates it, so a setting
// that is resolved but not listed here cannot exist — which is the point: a
// value the CLI honours but doctor does not report is invisible precedence,
// the failure this row is meant to remove.
var settingSpecs = []setting{
	{key: "base_url", env: "QEURO_API_URL", kind: kindString, desc: "backend proxy address"},
	{key: "console_url", env: "QEURO_CONSOLE_URL", kind: kindString, desc: "web console address"},
	{key: "token", env: "QEURO_TOKEN", kind: kindString, secret: true, desc: "API token"},
	{key: "model", env: "QEURO_MODEL", kind: kindString, projectSafe: true, desc: "pinned model id (empty = auto-router)"},
	{key: "auto_approve", env: "QEURO_AUTO_APPROVE", kind: kindBool, desc: "skip tool approval prompts"},
	// budget is deliberately not projectSafe. It is a ceiling on the user's own
	// money, and a cloned repository raising or removing it is the whole risk:
	// a project file setting `budget = 0` would silently disable a limit the user
	// set for themselves.
	{key: "budget", env: "QEURO_BUDGET", kind: kindString, desc: "hard session credit ceiling (empty = unlimited)"},
	{key: "skills_dir", env: "QEURO_SKILLS_DIR", kind: kindString, desc: "extra skills directory"},
	// Offline mode (roadmap §8 row "Offline"). None of the three is projectSafe,
	// and local_url is the reason: it is where prompts and file contents are sent,
	// so a cloned repository choosing it is the same exfiltration risk as base_url.
	// `local` itself matters too — a repo silently switching a user onto a local
	// model would change which model reviewed their code without saying so.
	{key: "local", env: "QEURO_LOCAL", kind: kindBool, desc: "run inference on a local model, never the backend"},
	{key: "local_url", env: "QEURO_LOCAL_URL", kind: kindString, desc: "local model server address (default http://localhost:11434)"},
	{key: "local_model", env: "QEURO_LOCAL_MODEL", kind: kindString, desc: "model name on the local server (empty = ask the server)"},
	// The rollout flag for roadmap-v3 §4.1. Default off, meaning each writer in a
	// parallel team step gets its own working tree and their changes are integrated
	// afterwards in plan order. Setting it skips isolation and lets the writers
	// share one tree — the pre-isolation behaviour, which is measured to lose work:
	// two workers patching different functions in one file both report success and
	// one edit is silently gone (ledger §40.2).
	//
	// Owner: platform-operator. Expires one release after isolation ships (ledger
	// §41), at which point the flag and this comment are deleted rather than
	// flipped. Rollback criterion: set it if isolation itself breaks a run — an
	// integration conflict on files a single writer owns, or a writer unable to read
	// the project through its overlay — and report that, because either is a defect
	// in the isolation rather than a reason to want shared writing.
	//
	// Not projectSafe, and for the usual reason on this list: a cloned repository
	// must not be able to re-enable concurrent writers on the machine that cloned
	// it. This is the flag that turns silent work-loss back on.
	{key: "unsafe_parallel_writes", env: "QEURO_UNSAFE_PARALLEL_WRITES", kind: kindBool, desc: "allow concurrent agents to write one tree (unsafe; loses edits)"},
}

// Origin is the resolved provenance of one setting, for `config doctor`.
type Origin struct {
	Key    string
	Layer  Layer
	Value  string // redacted when the setting is secret
	Source string // file path, env var name, or "built-in"
	Secret bool
	Set    bool // false when no layer supplied a value
	Desc   string
}

// Resolved is the outcome of layering every source.
type Resolved struct {
	values  map[string]string
	origins map[string]Origin
	// Warnings are non-fatal problems worth telling the user about: an
	// unreadable file, a key nobody recognises. They are warnings rather than
	// errors because refusing to start over an unknown key would make adding a
	// setting in a newer CLI break an older one on the same machine.
	Warnings []string
}

// Value returns the resolved string for a key.
func (r Resolved) Value(key string) string { return r.values[key] }

// Bool returns a resolved boolean. Anything a person would reasonably write is
// accepted; QEURO_AUTO_APPROVE historically meant "1", so that keeps working.
func (r Resolved) Bool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(r.values[key])) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Origins returns every setting's provenance, key-sorted so doctor output is
// stable and diffable between runs.
func (r Resolved) Origins() []Origin {
	out := make([]Origin, 0, len(r.origins))
	for _, o := range r.origins {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ProjectFileName is the per-repository config file the CLI looks for in the
// working directory.
const ProjectFileName = ".qeuro.toml"

// UserFileName is the per-user config file under the qeuro config directory.
const UserFileName = "config.toml"

// resolveOptions carries the highest-precedence layer plus the lookup roots.
// Env and the filesystem are parameters rather than globals so the tests can
// drive real precedence without mutating the process, and so `doctor` can
// report on a specific directory.
type resolveOptions struct {
	flags   map[string]string
	getenv  func(string) string
	workDir string
	homeDir string
}

// resolve layers all four sources. Later assignments win, so the calls run in
// ascending precedence and the last writer of a key is also the recorded origin
// — the provenance cannot drift from the value because they are set together.
func resolve(opts resolveOptions) Resolved {
	res := Resolved{values: map[string]string{}, origins: map[string]Origin{}}

	known := map[string]setting{}
	for _, s := range settingSpecs {
		known[s.key] = s
		res.values[s.key] = ""
		res.origins[s.key] = Origin{Key: s.key, Layer: LayerDefault, Source: "built-in", Secret: s.secret, Desc: s.desc}
	}

	apply := func(key, value string, layer Layer, source string) {
		s, ok := known[key]
		if !ok {
			return
		}
		res.values[key] = value
		shown := value
		if s.secret {
			shown = redact(value)
		} else {
			// Every layer funnels through here, and every consumer of a value
			// prints it: doctor renders the table, the TUI shows a notice. The TOML
			// reader already refuses control characters, but env and flags never
			// touch it, so the display path is sanitised once at the point all
			// three layers meet rather than in each renderer.
			shown = sanitizeForDisplay(value, false)
		}
		res.origins[key] = Origin{
			Key: key, Layer: layer, Value: shown, Source: source,
			Secret: s.secret, Set: true, Desc: s.desc,
		}
	}

	// Files, lowest precedence first.
	for _, src := range []struct {
		path  string
		layer Layer
	}{
		{filepath.Join(opts.homeDir, UserFileName), LayerUserFile},
		{filepath.Join(opts.workDir, ProjectFileName), LayerProjectFile},
	} {
		if src.path == "" || opts.homeDir == "" && src.layer == LayerUserFile {
			continue
		}
		parsed, warns, err := readConfigFile(src.path, known)
		res.Warnings = append(res.Warnings, warns...)
		if err != nil {
			res.Warnings = append(res.Warnings, err.Error())
			continue
		}
		for key, tv := range parsed {
			if src.layer == LayerProjectFile && !known[key].projectSafe {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s:%d: %q is ignored in a project file — it arrives with the repository, "+
						"and this setting can redirect credentials or bypass approval; set it in %s or the environment instead",
					src.path, tv.line, key, UserFileName))
				continue
			}
			apply(key, tv.raw, src.layer, fmt.Sprintf("%s:%d", src.path, tv.line))
		}
	}

	// Env overrides files.
	getenv := opts.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, s := range settingSpecs {
		if v := getenv(s.env); v != "" {
			apply(s.key, v, LayerEnv, s.env)
		}
	}

	// Flags override everything.
	for key, v := range opts.flags {
		if v == "" {
			continue
		}
		apply(key, v, LayerFlag, "command line")
	}

	return res
}

// readConfigFile parses one config file. A missing file is not a problem — most
// users will have neither. Unknown keys are reported as warnings naming the
// line, because the most common cause is a typo and the user needs to know the
// value is not in effect.
func readConfigFile(path string, known map[string]setting) (map[string]tomlValue, []string, error) {
	// #nosec G304 -- path is built from the user's own config dir or working
	// directory by resolve(); it is not attacker-supplied.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("config %s is unreadable: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	parsed, err := parseFlatTOML(f, path)
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	for key, tv := range parsed {
		s, ok := known[key]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s:%d: unknown setting %q (ignored); run `qeuro config doctor` for the list", path, tv.line, key))
			delete(parsed, key)
			continue
		}
		if s.kind == kindBool {
			if _, err := strconv.ParseBool(strings.ToLower(tv.raw)); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s:%d: %q expects true or false, got %q (ignored)", path, tv.line, key, tv.raw))
				delete(parsed, key)
			}
		}
	}
	return parsed, warnings, nil
}

// sanitizeForDisplay replaces control characters with a visible escape so a
// value cannot address the terminal it is printed to.
//
// The value itself is left alone — an env var with a stray \r still has to work,
// because refusing it would break a shell script over a cosmetic problem. Only
// what is shown is rewritten, which is why this sits beside redact(): both are
// display concerns, and both would be bugs if applied to res.values.
func sanitizeForDisplay(v string, keepNewlines bool) string {
	if indexControl(v) < 0 {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 8)
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\t', keepNewlines && c == '\n':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// redact shows enough of a secret to recognise which one it is without
// disclosing it. Short values collapse entirely rather than exposing most of a
// weak secret.
func redact(v string) string {
	if v == "" {
		return ""
	}
	const keep = 4
	if len(v) <= keep*2 {
		return strings.Repeat("*", 8)
	}
	return v[:keep] + strings.Repeat("*", 8) + v[len(v)-2:]
}
