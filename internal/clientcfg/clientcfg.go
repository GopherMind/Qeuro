// Package clientcfg persists the CLI's connection settings. On Windows the API
// token is encrypted with DPAPI in a sidecar file and omitted from config.json;
// other platforms keep the owner-only config-file fallback. Env vars override
// persisted values so the CLI can run against a different backend/token without
// editing config.
package clientcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"qeuro/internal/client"
)

// readStoredToken and probeStoredToken are the seams the lazy-resolution tests
// replace. They point at the platform implementations (DPAPI on Windows, the OS
// keychain elsewhere).
//
// The pair is the whole design in miniature: probeStoredToken answers "is there a
// token" cheaply, readStoredToken produces the value expensively, and roadmap §8
// asks that the startup path use only the first. A test can count calls to the
// second, which is the only way to keep that true as the startup path changes.
var (
	readStoredToken  = loadStoredToken
	probeStoredToken = storedTokenPresent
)

// DefaultBaseURL is the backend address used when nothing else is set. It is
// a var (not a const) so release builds can point at production without code
// changes:
//
//	go build -ldflags "-X qeuro/internal/clientcfg.DefaultBaseURL=https://api.qeuro.dev"
//
// QEURO_API_URL overrides it at runtime.
var DefaultBaseURL = "http://localhost:8080"

// DefaultConsoleURL is the web console address used for browser-based signup.
// The CLI probes common dev ports before falling back to this value. Release
// builds should override it:
//
//	go build -ldflags "-X qeuro/internal/clientcfg.DefaultConsoleURL=https://console.qeuro.dev"
//
// QEURO_CONSOLE_URL overrides it at runtime.
var DefaultConsoleURL = "http://localhost:3000"

// Config is the persisted CLI configuration.
type Config struct {
	BaseURL    string `json:"base_url"`
	ConsoleURL string `json:"console_url,omitempty"`
	// Token is the value config.json carries, and nothing else. Reading a token
	// belongs to Secret(); this field exists because the file format has the key
	// and Save has to write it on platforms without a working secret store.
	//
	// Callers must not read it. Use Secret() for the value and LoggedIn() for
	// presence: on a platform where the token lives in the OS keychain this
	// field is empty even for a logged-in user, so a direct read reports
	// "logged out" on exactly the machines where storage worked.
	Token            string `json:"token,omitempty"`
	OnboardingOpened bool   `json:"onboarding_opened,omitempty"`

	// secret carries the resolved token. It is a pointer to shared state rather
	// than a value because Config is copied freely (it is passed by value
	// everywhere), and resolving the token in one copy must not leave the others
	// about to repeat a D-Bus round trip.
	secret *secretRef

	// Model is a pinned model id; empty means the auto-router decides. Settable
	// from the TOML layers, so a repository can commit a `.qeuro.toml` choosing
	// the model appropriate for its codebase.
	Model string `json:"-"`
	// AutoApprove skips tool-approval prompts. It stays out of config.json
	// deliberately: this is the one setting that removes a human from the loop
	// before a write or a command, so it must be a visible, per-invocation
	// choice (flag or env) or an explicit line in a config file someone can
	// read — never something the CLI silently persisted on their behalf.
	AutoApprove bool `json:"-"`
	// SkillsDir is an extra skills directory.
	SkillsDir string `json:"-"`
	// Budget is a hard ceiling, in credits, on what one session may spend; 0
	// means unlimited. Like AutoApprove it stays out of config.json: the CLI
	// persisting a spend limit the user never wrote down would make a session
	// stop for a reason they cannot find.
	Budget float64 `json:"-"`

	// Local puts the session in offline mode: inference goes to a model on this
	// host and the backend is never contacted (roadmap §8 row "Offline"). Like
	// AutoApprove it is deliberately not persisted — which model answered, and
	// whether anything left the machine, must be a visible per-invocation choice.
	Local bool `json:"-"`
	// LocalURL is the local model server; empty means client.DefaultLocalURL.
	LocalURL string `json:"-"`
	// LocalModel is the model name to ask that server for; empty asks the server
	// what it has.
	LocalModel string `json:"-"`

	// UnsafeParallelWrites lifts the roadmap-v3 §4.1 restriction that makes a
	// parallel team step read-only. It exists as the rollout flag §0.3 requires,
	// and the default (false = restriction in force) is the safe one because the
	// restriction prevents measured work-loss, not a hypothetical race.
	//
	// Not persisted, like AutoApprove and Budget: this re-enables a mode where the
	// tool reports success for an edit it then discards, so it has to be a visible
	// per-invocation choice rather than something the CLI wrote down once.
	UnsafeParallelWrites bool `json:"-"`

	// Origins records where each setting came from, for `qeuro config doctor`.
	Origins []Origin `json:"-"`
	// Warnings are non-fatal config problems (unreadable file, unknown key).
	Warnings []string `json:"-"`
}

// secretRef is the resolved-token cell shared by every copy of a Config.
//
// Roadmap §8 asks for lazy keyring initialisation, and the reason is measurable:
// on Linux a keychain read is a D-Bus round trip to a service that may not be
// running, so it is a wait rather than a lookup, and it happens before the prompt
// is drawn. It is also usually wasted — the keychain is the *lowest*-precedence
// source of a token, so any of env, flags, config.toml or config.json answering
// makes the read pure cost.
type secretRef struct {
	mu sync.Mutex
	// value is the token once known; ok records that it was resolved, so a
	// legitimately empty token is not re-resolved on every call.
	value string
	ok    bool
	// present is what LoggedIn answers. It is set eagerly from what the layers
	// supplied plus a cheap existence probe, and is deliberately separate from
	// value: presence gates a notice and a status line, and paying for a secret
	// to draw a status line is what this row exists to remove.
	present bool
	// resolve produces the token. nil means "nothing above the store supplied
	// one, ask the platform store".
	resolve func() (string, error)
}

// Secret returns the API token, consulting the OS secret store on the first call
// if no higher-precedence layer supplied one. Callers that only need to know
// whether the user is signed in must use LoggedIn instead.
func (c Config) Secret() string {
	if c.secret == nil {
		return c.Token
	}
	c.secret.mu.Lock()
	defer c.secret.mu.Unlock()
	if c.secret.ok {
		return c.secret.value
	}
	if c.secret.resolve != nil {
		// A store failure is not an error here: the documented fallback is
		// config.json, and Load already recorded a warning about it. Returning ""
		// makes the caller's request unauthenticated, which the backend answers
		// with 401 — a diagnosable outcome, unlike a startup that refuses to run.
		v, _ := c.secret.resolve()
		c.secret.value = v
	}
	c.secret.ok = true
	return c.secret.value
}

// LocalEndpoint returns the local model server the session will use, applying
// the default when no layer set one.
func (c Config) LocalEndpoint() string {
	if c.LocalURL != "" {
		return c.LocalURL
	}
	return client.DefaultLocalURL
}

// LoggedIn reports whether a token is present, without resolving it.
func (c Config) LoggedIn() bool {
	if c.secret == nil {
		return c.Token != ""
	}
	c.secret.mu.Lock()
	defer c.secret.mu.Unlock()
	return c.secret.present
}

// SetToken replaces the token in this Config, for login and logout. It sets both
// the value and the presence signal, so the caller does not have to know that
// they are tracked separately.
func (c *Config) SetToken(token string) {
	c.Token = token
	if c.secret == nil {
		c.secret = &secretRef{}
	}
	c.secret.mu.Lock()
	c.secret.value = token
	c.secret.ok = true
	c.secret.present = token != ""
	c.secret.resolve = nil
	c.secret.mu.Unlock()
}

// Provider returns the inference source this configuration selects: a local
// model server in offline mode, otherwise the backend.
//
// Every entry point goes through here rather than calling client.New directly,
// because the guarantee offline mode makes is negative — that nothing reaches the
// network — and a single construction point is what makes it checkable. There is
// deliberately no fallback from local to backend: the user asked for a closed
// contour, and quietly reaching the internet when the local server is down would
// send the prompt exactly where they said it must not go.
func (c Config) Provider() client.Provider {
	if c.Local {
		return client.NewLocalProvider(c.LocalURL, c.LocalModel, client.DialectAuto)
	}
	return client.New(c.BaseURL, c.Secret())
}

// LazyProvider is Provider for the startup path: the backend client resolves its
// token on first use rather than now (roadmap §8 "Startup"). In offline mode
// there is no token to resolve, so both paths are identical.
func (c Config) LazyProvider() client.Provider {
	if c.Local {
		return client.NewLocalProvider(c.LocalURL, c.LocalModel, client.DialectAuto)
	}
	return client.NewLazy(c.BaseURL, c.Secret)
}

// TokenStorageWarning returns a user-facing warning when the current platform
// cannot store tokens in an OS-backed secret store.
func TokenStorageWarning() string {
	return tokenStorageWarning()
}

// dir returns the qeuro config directory, creating nothing.
func dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "qeuro"), nil
}

// path returns the config file path.
func path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

func tokenPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "token.dat"), nil
}

// Load reads the layered configuration. Precedence, lowest to highest
// (roadmap §8): built-in defaults, `~/.qeuro/config.toml`, `./.qeuro.toml`,
// environment, then flags supplied by the caller. A missing file at any layer is
// not an error — it yields the layer below.
//
// config.json is *not* one of those layers. It is the CLI's own state file (what
// `qeuro login` wrote), so it sits between the defaults and the TOML files: a
// file the user hand-edited must beat a file the CLI wrote for them, or editing
// it would appear to do nothing.
func Load() (Config, error) { return LoadWithFlags(nil) }

// LoadWithFlags is Load with the top precedence layer supplied by the caller,
// keyed by setting name (see settingSpecs). Commands that accept a flag for a
// setting pass it here rather than assigning over the result, so the flag lands
// in the same resolution that records provenance and doctor can report it.
func LoadWithFlags(flags map[string]string) (Config, error) {
	cfg := Config{BaseURL: DefaultBaseURL, ConsoleURL: DefaultConsoleURL}

	// parseErr records a corrupt config file. We still apply defaults and env
	// overrides below (so a bad file never blocks QEURO_TOKEN/QEURO_API_URL) but
	// return the error so the caller can warn instead of silently logging the
	// user out (L10).
	var parseErr error
	if p, err := path(); err == nil {
		// #nosec G304 -- p is this process's own config path under the user's
		// home directory, derived by path(); it is not caller-supplied.
		if data, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				cfg = Config{BaseURL: DefaultBaseURL, ConsoleURL: DefaultConsoleURL}
				parseErr = fmt.Errorf("clientcfg: parse %s: %w", p, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
	}

	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.ConsoleURL == "" {
		cfg.ConsoleURL = DefaultConsoleURL
	}
	// The OS secret store is deliberately *not* read here. It is the lowest
	// precedence source of a token, so reading it before resolve() below meant
	// paying for it and then usually discarding the result — and on Linux that
	// payment is a D-Bus round trip on the path to the prompt (roadmap §8
	// "Startup"). storedTokenPresent is the cheap probe that keeps LoggedIn
	// answerable without it.
	cfg.secret = &secretRef{present: cfg.Token != ""}
	if cfg.Token == "" {
		cfg.secret.resolve = func() (string, error) { return readStoredToken() }
		if probeStoredToken() {
			cfg.secret.present = true
		}
	} else {
		// config.json carried it, so there is nothing to resolve.
		cfg.secret.value, cfg.secret.ok = cfg.Token, true
	}

	// Everything above is config.json plus the OS keychain — the CLI's own
	// state. The TOML files, env and flags layer on top of it.
	res := resolve(resolveOptions{
		flags:   flags,
		workDir: workingDir(),
		homeDir: configDir(),
	})
	cfg.Origins, cfg.Warnings = res.Origins(), res.Warnings

	// A layer that supplied nothing must not blank out what config.json holds,
	// so each assignment is guarded by whether the setting was actually set.
	// Doing this per key rather than by "is the resolved string empty" keeps an
	// explicit `base_url = ""` distinguishable from an absent one.
	for _, o := range cfg.Origins {
		if !o.Set {
			continue
		}
		switch o.Key {
		case "base_url":
			cfg.BaseURL = res.Value("base_url")
		case "console_url":
			cfg.ConsoleURL = res.Value("console_url")
		case "token":
			// A layer above the store answered, so the store is never consulted:
			// this is where most of the saved time comes from, since QEURO_TOKEN in
			// a shell profile is the common case.
			cfg.SetToken(res.Value("token"))
		case "model":
			cfg.Model = res.Value("model")
		case "auto_approve":
			cfg.AutoApprove = res.Bool("auto_approve")
		case "skills_dir":
			cfg.SkillsDir = res.Value("skills_dir")
		case "local":
			cfg.Local = res.Bool("local")
		case "unsafe_parallel_writes":
			cfg.UnsafeParallelWrites = res.Bool("unsafe_parallel_writes")
		case "local_url":
			// An unusable address is a warning plus the default, not a silent
			// substitution and not a hard stop: offline mode exists for machines with
			// no fallback, so a typo in an env var must not leave the user with no
			// working CLI, and it must not quietly point somewhere else either.
			v := strings.TrimRight(strings.TrimSpace(res.Value("local_url")), "/")
			if err := client.ValidateLocalURL(v); err != nil {
				cfg.Warnings = append(cfg.Warnings, err.Error()+" — using the default local address")
				break
			}
			cfg.LocalURL = v
		case "local_model":
			cfg.LocalModel = strings.TrimSpace(res.Value("local_model"))
		case "budget":
			// An unparseable or negative ceiling is a warning, not silence and not a
			// hard failure. Silence would leave the user believing a limit is in
			// force; refusing to start would make a typo in a config file lock them
			// out of their own CLI.
			v := strings.TrimSpace(res.Value("budget"))
			if v == "" {
				break
			}
			n, err := strconv.ParseFloat(v, 64)
			switch {
			case err != nil:
				cfg.Warnings = append(cfg.Warnings,
					fmt.Sprintf("budget: %q is not a number — no spend ceiling is in force", v))
			// "nan" and "inf" parse successfully, and neither is caught by n < 0:
			// NaN fails every comparison, so it would read as no ceiling, and +Inf
			// is a ceiling that can never be reached. Both are the silent-limit
			// failure this warning exists to prevent, so both are named.
			case math.IsNaN(n), math.IsInf(n, 0):
				cfg.Warnings = append(cfg.Warnings,
					fmt.Sprintf("budget: %s is not a usable amount — no spend ceiling is in force", v))
			case n < 0:
				cfg.Warnings = append(cfg.Warnings,
					fmt.Sprintf("budget: %s is negative — no spend ceiling is in force", v))
			default:
				cfg.Budget = n
			}
		}
	}

	// The stored/default values are the effective ones for anything no layer
	// set, so doctor reports what the CLI will actually use rather than an empty
	// cell that looks like a missing setting.
	cfg.Origins = withEffectiveDefaults(cfg.Origins, cfg)
	return cfg, parseErr
}

// OriginsForDisplay returns the provenance table with the token row filled in,
// forcing the one secret-store read that Load avoids.
//
// `qeuro config doctor` is the only caller, and it is the one place the read is
// correct: the command exists because "overridden" and "never read" look
// identical, so a row reading "(not set)" for a user who is in fact signed in
// would recreate exactly the confusion it removes. Doctor is interactive and
// already stats three files, so the cost is invisible there — unlike on the
// startup path.
func (c Config) OriginsForDisplay() []Origin {
	origins := make([]Origin, len(c.Origins))
	copy(origins, c.Origins)
	token := c.Secret()
	for i := range origins {
		if origins[i].Key != "token" || origins[i].Set || token == "" {
			continue
		}
		origins[i].Value = redact(token)
		origins[i].Source = FilePath()
		origins[i].Set = true
	}
	return origins
}

// withEffectiveDefaults fills the display value of unset settings from the
// resolved config, marking the source as config.json when that file supplied it.
//
// The token is absent from the map on purpose: filling it here would resolve the
// secret on every Load, which is what roadmap §8 asks us to stop doing. Doctor
// gets it from OriginsForDisplay instead.
func withEffectiveDefaults(origins []Origin, cfg Config) []Origin {
	stored := map[string]string{
		"base_url":    cfg.BaseURL,
		"console_url": cfg.ConsoleURL,
		// The effective endpoint, not the raw field: doctor's job is to report what
		// the CLI will actually use, and an empty cell here would read as "offline
		// mode has nowhere to connect".
		"local_url": cfg.LocalEndpoint(),
	}
	for i, o := range origins {
		if o.Set {
			continue
		}
		v, ok := stored[o.Key]
		if !ok || v == "" {
			continue
		}
		// config.json reaches Origins without passing through resolve()'s apply(),
		// so it needs the same display treatment: a hand-edited state file is
		// another way a control character could reach the terminal.
		origins[i].Value = sanitizeForDisplay(v, false)
		if o.Secret {
			origins[i].Value = redact(v)
		}
		// base_url and console_url have a built-in default; only call it
		// config.json when the value differs from that default, otherwise a
		// fresh install would claim a file it never wrote.
		switch {
		case o.Key == "base_url" && v == DefaultBaseURL,
			o.Key == "console_url" && v == DefaultConsoleURL,
			// local_url is never in config.json — it is flag/env/TOML only — so its
			// effective value is always the built-in default when no layer set it.
			o.Key == "local_url":
			origins[i].Source = "built-in"
		default:
			origins[i].Source = FilePath()
			origins[i].Set = true
		}
	}
	return origins
}

// workingDir returns the directory searched for ProjectFileName.
func workingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// configDir returns the qeuro config directory, or "" when the OS does not
// report one — in which case the user-file layer is simply absent.
func configDir() string {
	d, err := dir()
	if err != nil {
		return ""
	}
	return d
}

// ConfigDir returns the qeuro config directory, or "" when the OS does not
// report one. Exported so other packages can place their own files beside
// config.json (mcp.json, sessions) without re-deriving the location.
func ConfigDir() string { return configDir() }

// UserFilePath returns the location of the user TOML config, for display.
func UserFilePath() string {
	d := configDir()
	if d == "" {
		return UserFileName
	}
	return filepath.Join(d, UserFileName)
}

// ProjectFilePath returns the location of the project TOML config, for display.
func ProjectFilePath() string {
	wd := workingDir()
	if wd == "" {
		return ProjectFileName
	}
	return filepath.Join(wd, ProjectFileName)
}

// Save writes the config file with owner-only permissions, creating the
// directory if needed.
func Save(cfg Config) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	if err := saveStoredToken(cfg.Token); err != nil {
		return err
	}
	p := filepath.Join(d, "config.json")
	if omitTokenFromConfig() {
		cfg.Token = ""
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// FilePath returns the location of the config file (for display in messages).
func FilePath() string {
	if p, err := path(); err == nil {
		return p
	}
	return "config.json"
}
