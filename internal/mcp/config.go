package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"qeuro/internal/clientcfg"
	"qeuro/internal/tools"
)

// ConfigFileName is the file that declares MCP servers.
const ConfigFileName = "mcp.json"

// Limits on the configuration itself. A config file is trusted more than a
// server, but it is still worth bounding: these numbers exist so a typo (a
// pasted directory listing, a runaway generator) fails with a clear message
// instead of starting two hundred processes.
const (
	maxServers            = 32
	maxAllowedToolsPerCfg = 128
	maxConfigBytes        = 256 << 10
	defaultCallsPerMinute = 30
)

// ServerConfig declares one MCP server.
//
// Every field is deliberately explicit. There is no "auto-discover servers from
// the environment" path and no inheritance from a project file, because both
// would let something other than the user decide which processes run on their
// machine with their tokens.
type ServerConfig struct {
	// Enabled must be set to true for the server to be started. The zero value
	// being "off" means a server pasted into the file for later cannot start
	// running on the next invocation by accident.
	Enabled bool `json:"enabled"`

	// Command and Args are an argv vector, never a shell string
	// (.ai/RULES.md:23). Dir is the working directory; empty means the CLI's.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir,omitempty"`

	// URL selects the streamable HTTP transport instead of stdio. Exactly one of
	// URL and Command may be set: a server is either a process this CLI supervises
	// or an endpoint it calls, and an entry with both would leave which one runs
	// up to the reader.
	URL string `json:"url,omitempty"`

	// AuthFrom names the environment variable holding the bearer token for an
	// HTTP server. A name, not a value, for the same reason EnvFrom is: the file
	// stays safe to commit to a dotfiles repository. There is deliberately no
	// literal-token field — a URL with an embedded credential is refused too.
	AuthFrom string `json:"authFrom,omitempty"`

	// Env sets literal environment values for the child. EnvFrom names variables
	// to copy from the CLI's own environment — names only, so the file never has
	// to contain a credential, which keeps it safe to commit to a dotfiles repo.
	Env     map[string]string `json:"env,omitempty"`
	EnvFrom []string          `json:"envFrom,omitempty"`

	// AllowTools lists the tools the model may call, by their server-side name.
	// An absent or empty list means no tools: a server whose tool set changes
	// after an update must not gain reach silently (.ai/RULES.md:24).
	AllowTools []string `json:"allowTools,omitempty"`

	// Comment is a per-server note, for the same reason as Config.Comment.
	Comment string `json:"//,omitempty"`

	// CallsPerMinute bounds how often the model may invoke this server. Zero
	// takes the default; a negative value means unlimited, which has to be
	// written out to be chosen.
	CallsPerMinute int `json:"callsPerMinute,omitempty"`
}

// Config is the parsed mcp.json.
type Config struct {
	// Comment exists because this is a file people edit by hand and JSON has no
	// comments. Unknown keys are an error — a typo'd "allowTool" must not look
	// applied — so "//" has to be a declared key rather than a tolerated one.
	Comment string `json:"//,omitempty"`

	Servers map[string]ServerConfig `json:"servers"`
}

// ServerNames returns the configured server names in a stable order, so
// discovery, listing and tool ordering do not depend on map iteration.
func (c Config) ServerNames() []string {
	out := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Allowed reports whether the model may call this tool on this server.
//
// The check is exact-match only: no globs, no prefixes. A glob in an allow-list
// is how "read-only access" becomes "also the write tool the server added last
// week", and there is no way for the user to notice.
func (s ServerConfig) Allowed(tool string) bool {
	for _, t := range s.AllowTools {
		if t == tool {
			return true
		}
	}
	return false
}

// Limit returns the effective per-minute call budget: zero means unlimited in
// the client, so a negative configured value maps to zero.
func (s ServerConfig) Limit() int {
	switch {
	case s.CallsPerMinute < 0:
		return 0
	case s.CallsPerMinute == 0:
		return defaultCallsPerMinute
	default:
		return s.CallsPerMinute
	}
}

// ConfigPath returns the only location the CLI reads MCP servers from.
func ConfigPath() string {
	d := clientcfg.ConfigDir()
	if d == "" {
		return ConfigFileName
	}
	return filepath.Join(d, ConfigFileName)
}

// LoadConfig reads ~/.qeuro/mcp.json. A missing file yields an empty config and
// no error — most users will not have one.
//
// There is deliberately no project layer. `./.qeuro.toml` exists and is filtered
// key by key because a repository is untrusted input (see settings.go's
// projectSafe); an MCP server declaration has no safe subset to filter down to —
// it is "run this program with these environment variables" — so a project-local
// mcp.json is not read at all. It is reported when present, because silently
// ignoring a file the user can see is worse than refusing it out loud.
func LoadConfig() (Config, []string, error) {
	var warnings []string
	if wd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(wd, ConfigFileName)); err == nil {
			warnings = append(warnings, fmt.Sprintf(
				"%s in this directory is ignored: MCP servers are only read from %s, "+
					"because a file that arrives with a repository must not choose which programs run with your tokens",
				ConfigFileName, ConfigPath()))
		}
	}

	cfg, warns, err := loadConfigFrom(ConfigPath(), os.LookupEnv)
	return cfg, append(warnings, warns...), err
}

// loadConfigFrom is LoadConfig with the path and environment injected, so tests
// exercise the real parser and the real validation.
func loadConfigFrom(path string, lookupEnv func(string) (string, bool)) (Config, []string, error) {
	var warnings []string

	// #nosec G304 -- path is the CLI's own config location, built by ConfigPath()
	// from the user's config dir; in tests it is a t.TempDir() path.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil, nil
		}
		return Config{}, nil, fmt.Errorf("mcp: %s is unreadable: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return Config{}, nil, fmt.Errorf("mcp: %s: %w", path, err)
	}
	if info.Size() > maxConfigBytes {
		return Config{}, nil, fmt.Errorf("mcp: %s is %d bytes, larger than the %d-byte limit",
			path, info.Size(), maxConfigBytes)
	}

	var raw Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // a typo'd key must not look applied
	if err := dec.Decode(&raw); err != nil {
		return Config{}, nil, fmt.Errorf("mcp: %s is not valid JSON: %w", path, err)
	}
	if len(raw.Servers) > maxServers {
		return Config{}, nil, fmt.Errorf("mcp: %s declares %d servers, more than the %d allowed",
			path, len(raw.Servers), maxServers)
	}

	out := Config{Servers: map[string]ServerConfig{}}
	names := raw.ServerNames()
	for _, name := range names {
		s := raw.Servers[name]
		clean, warns := validateServer(path, name, s, lookupEnv)
		warnings = append(warnings, warns...)
		if clean == nil {
			continue
		}
		out.Servers[name] = *clean
	}
	return out, warnings, nil
}

// validateServer checks one entry. It returns nil when the entry cannot be used
// at all; anything recoverable is dropped from the entry with a warning, so one
// bad tool name does not cost the user the whole server.
func validateServer(path, name string, s ServerConfig, lookupEnv func(string) (string, bool)) (*ServerConfig, []string) {
	var warnings []string
	where := path + ": server " + quoteName(name)

	// The server name becomes part of every tool name the model sees, so it has
	// to satisfy the specification's identifier rules before namespacing can
	// produce a valid name.
	if !tools.ValidMCPIdent(name) {
		return nil, append(warnings, fmt.Sprintf(
			"%s: name must be 1–128 characters of A–Z a–z 0–9 _ - . and must not contain a double underscore; ignored",
			where))
	}
	hasCommand := strings.TrimSpace(s.Command) != ""
	hasURL := strings.TrimSpace(s.URL) != ""
	switch {
	case hasCommand && hasURL:
		return nil, append(warnings, fmt.Sprintf(
			"%s: has both command and url, so which transport to use is undefined; ignored", where))
	case !hasCommand && !hasURL:
		return nil, append(warnings, fmt.Sprintf("%s: no command and no url; ignored", where))
	case hasURL:
		if _, err := parseServerURL(s.URL); err != nil {
			return nil, append(warnings, fmt.Sprintf("%s: %v; ignored", where, err))
		}
		// The stdio-only fields are dropped rather than tolerated: leaving args or
		// env on an HTTP entry would look applied in `qeuro mcp list` while nothing
		// reads them.
		for _, unused := range stdioOnlyFields(s) {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s applies to a stdio server and is ignored for a url server", where, unused))
		}
	}
	if !s.Enabled {
		// Not a warning: "off" is a normal state for an entry kept around.
		return nil, nil
	}

	clean := s
	clean.AllowTools = nil
	seen := map[string]bool{}
	for _, tool := range s.AllowTools {
		switch {
		case strings.ContainsAny(tool, "*?["):
			// A glob is already rejected by the validity check below, but it gets its
			// own message first: a user who wrote one has a specific expectation, and
			// "not a valid tool name" would not tell them patterns are unsupported by
			// design rather than unimplemented.
			warnings = append(warnings, fmt.Sprintf("%s: allowTools entry %q looks like a pattern, and patterns are not supported — list each tool; ignored", where, sanitizeOneLine(tool)))
		case !tools.ValidMCPIdent(tool):
			warnings = append(warnings, fmt.Sprintf("%s: allowTools entry %q is not a valid tool name; ignored", where, sanitizeOneLine(tool)))
		case seen[tool]:
		case len(clean.AllowTools) >= maxAllowedToolsPerCfg:
			warnings = append(warnings, fmt.Sprintf("%s: more than %d allowed tools; the rest are ignored", where, maxAllowedToolsPerCfg))
		default:
			seen[tool] = true
			clean.AllowTools = append(clean.AllowTools, tool)
		}
	}
	if len(clean.AllowTools) == 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: allowTools is empty, so no tool on it can be called; run `qeuro mcp tools %s` to see what it offers", where, name))
	}

	if hasURL {
		// An HTTP server gets no environment at all: there is no child process to
		// give one to, and carrying the fields forward would make `qeuro mcp list`
		// show credentials as passed when nothing passes them.
		clean.Args, clean.Dir, clean.Env, clean.EnvFrom = nil, "", nil, nil
		authFrom, authWarns := validateAuthFrom(where, s.AuthFrom, lookupEnv)
		clean.AuthFrom = authFrom
		warnings = append(warnings, authWarns...)
		return &clean, warnings
	}

	// authFrom on a stdio server would silently do nothing, and a user who wrote
	// it expects a token to be sent somewhere.
	if strings.TrimSpace(s.AuthFrom) != "" {
		warnings = append(warnings, fmt.Sprintf(
			"%s: authFrom applies to a url server; a stdio server receives credentials through envFrom", where))
		clean.AuthFrom = ""
	}

	envFrom, envFromWarns := validateEnvFrom(where, s.EnvFrom, lookupEnv)
	clean.EnvFrom = envFrom
	warnings = append(warnings, envFromWarns...)

	env, envWarns := validateEnv(where, s.Env)
	clean.Env = env
	warnings = append(warnings, envWarns...)

	return &clean, warnings
}

// stdioOnlyFields names the fields set on an entry that only a stdio server
// reads, so an HTTP entry carrying them is reported rather than silently pruned.
func stdioOnlyFields(s ServerConfig) []string {
	var out []string
	if len(s.Args) > 0 {
		out = append(out, "args")
	}
	if strings.TrimSpace(s.Dir) != "" {
		out = append(out, "dir")
	}
	if len(s.Env) > 0 {
		out = append(out, "env")
	}
	if len(s.EnvFrom) > 0 {
		out = append(out, "envFrom")
	}
	return out
}

// validateAuthFrom checks the environment variable name a bearer token is read
// from.
//
// The same deny-list as envFrom applies, and for a stronger reason: an HTTP
// server is reached over the network, so a provider key named here would be sent
// to a remote host rather than merely handed to a local process.
func validateAuthFrom(where, name string, lookupEnv func(string) (string, bool)) (string, []string) {
	name = strings.TrimSpace(name)
	if name == "" {
		// Not a warning: a public or loopback server may need no token at all.
		return "", nil
	}
	switch {
	case !validEnvName(name):
		return "", []string{fmt.Sprintf(
			"%s: authFrom %q is not an environment variable name; no token will be sent", where, sanitizeOneLine(name))}
	case deniedEnvName(name):
		return "", []string{fmt.Sprintf(
			"%s: authFrom %q is a provider or infrastructure credential and is never sent to an MCP server; ignored", where, name)}
	}
	v, ok := lookupEnv(name)
	switch {
	case !ok:
		return name, []string{fmt.Sprintf(
			"%s: authFrom names %q, which is not set in this environment, so requests will be unauthenticated", where, name)}
	case strings.TrimSpace(v) == "":
		// Set but empty is the `export TOK=$(command that failed)` case, and it is
		// worth its own message: the request goes out with no Authorization header,
		// and the server's 401 would otherwise be reported with the advice to set
		// authFrom — which the user already did.
		return name, []string{fmt.Sprintf(
			"%s: authFrom names %q, which is set but empty, so requests will be unauthenticated", where, name)}
	}
	return name, nil
}

// IsHTTP reports whether this entry uses the streamable HTTP transport.
func (s ServerConfig) IsHTTP() bool { return strings.TrimSpace(s.URL) != "" }

// validateEnvFrom filters the names to copy from the CLI's environment.
func validateEnvFrom(where string, names []string, lookupEnv func(string) (string, bool)) ([]string, []string) {
	var (
		out      []string
		warnings []string
	)
	for _, n := range names {
		switch {
		case !validEnvName(n):
			warnings = append(warnings, fmt.Sprintf(
				"%s: envFrom entry %q is not an environment variable name (letters, digits and _, not starting with a digit); ignored", where, n))
		case deniedEnvName(n):
			// The user is allowed to configure their own machine, but this
			// particular door stays shut: .ai/AI.md:49 and roadmap.txt:333 both
			// require that provider secrets never enter an MCP server process, and
			// an MCP server is third-party code with network access. Naming the
			// variable in envFrom is the one way that could happen by hand.
			warnings = append(warnings, fmt.Sprintf(
				"%s: envFrom entry %q is a provider or infrastructure credential and is never passed to an MCP server; ignored", where, n))
		default:
			if _, ok := lookupEnv(n); !ok {
				// Not fatal: the server may treat it as optional. But an absent
				// variable is the likeliest cause of "the server starts and every
				// call fails with 401", and that is worth saying up front.
				warnings = append(warnings, fmt.Sprintf("%s: envFrom names %q, which is not set in this environment", where, n))
			}
			out = append(out, n)
		}
	}
	return out, warnings
}

// validateEnv filters literal environment entries.
func validateEnv(where string, env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	var warnings []string
	out := map[string]string{}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case !validEnvName(k):
			warnings = append(warnings, fmt.Sprintf("%s: env key %q is not an environment variable name; ignored", where, k))
		case deniedEnvName(k):
			warnings = append(warnings, fmt.Sprintf(
				"%s: env key %q is a provider or infrastructure credential and is never passed to an MCP server; ignored", where, k))
		case strings.ContainsRune(env[k], 0):
			warnings = append(warnings, fmt.Sprintf("%s: env value for %q contains a NUL byte; ignored", where, k))
		default:
			out[k] = env[k]
		}
	}
	if len(out) == 0 {
		return nil, warnings
	}
	return out, warnings
}

// validEnvName accepts what a POSIX shell and Windows both accept as a variable
// name. The restriction matters because the name is written into the child's
// environment block, and "=" or NUL there is a malformed environment
// (.ai/RULES.md:22).
func validEnvName(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// deniedEnvNames are never passed to an MCP server process.
//
// Prefixes cover this product's own variables and the two cloud vendors whose
// names are stable; the exact list covers other model providers and the data
// stores. The check is deliberately not a substring match on TOKEN or SECRET:
// GITHUB_TOKEN is the single most common legitimate reason to use envFrom, and a
// rule that blocked it would push users to hard-code the value in the file
// instead — a worse outcome than the one being prevented.
var deniedEnvPrefixes = []string{"QEURO_", "STRIPE_", "AWS_", "PG"}

var deniedEnvExact = []string{
	"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "OPENROUTER_KEY",
	"GOOGLE_API_KEY", "GEMINI_API_KEY", "DEEPSEEK_API_KEY", "NVIDIA_API_KEY",
	"GROQ_API_KEY", "MISTRAL_API_KEY", "COHERE_API_KEY", "XAI_API_KEY",
	"DATABASE_URL", "POSTGRES_PASSWORD", "POSTGRES_URL", "REDIS_URL",
}

// deniedEnvName matches case-insensitively. Windows environment variables are
// case-insensitive, so a lower-case spelling of a denied name would otherwise
// resolve to the same value there.
func deniedEnvName(name string) bool {
	up := strings.ToUpper(name)
	for _, p := range deniedEnvPrefixes {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	for _, e := range deniedEnvExact {
		if up == e {
			return true
		}
	}
	return false
}

// StartTransport starts the transport one server entry describes.
//
// Both call sites (the manager and `qeuro mcp`) go through here rather than each
// choosing a transport themselves. A second dispatch site is how one of them ends
// up not knowing about a transport, and the symptom would be an HTTP server that
// works in `qeuro mcp tools` and is absent in chat.
func StartTransport(s ServerConfig, lookupEnv func(string) (string, bool)) (Transport, error) {
	if s.IsHTTP() {
		bearer := ""
		if s.AuthFrom != "" {
			// Absent is not an error here: validateAuthFrom already warned, and a
			// server may accept unauthenticated requests.
			bearer, _ = lookupEnv(s.AuthFrom)
		}
		return StartHTTP(HTTPConfig{URL: s.URL, Bearer: bearer})
	}
	return StartStdio(StdioConfigFor(s, lookupEnv))
}

// StdioConfigFor builds the transport configuration for one server, resolving
// EnvFrom against the CLI's environment. The result is built from scratch by
// BaseEnv, so a variable that is neither on the allow-list nor named here cannot
// reach the child.
func StdioConfigFor(s ServerConfig, lookupEnv func(string) (string, bool)) StdioConfig {
	extra := map[string]string{}
	for k, v := range s.Env {
		extra[k] = v
	}
	for _, n := range s.EnvFrom {
		if v, ok := lookupEnv(n); ok {
			extra[n] = v
		}
	}
	return StdioConfig{
		Command: s.Command,
		Args:    s.Args,
		Dir:     s.Dir,
		Env:     BaseEnv(extra),
	}
}

// quoteName quotes a server name for a warning message. The name comes from the
// user's own file, but it is still interpolated into text that reaches a
// terminal, so it passes through the same one-line sanitiser as server output.
func quoteName(s string) string { return "\"" + sanitizeOneLine(s) + "\"" }
