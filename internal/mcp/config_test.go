package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig puts a mcp.json in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// noEnv is a lookupEnv that reports every variable as unset.
func noEnv(string) (string, bool) { return "", false }

// envWith builds a lookupEnv over a fixed map, so tests never mutate the process
// environment and can run in parallel.
func envWith(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestLoadConfigMissingFileIsNotAnError(t *testing.T) {
	cfg, warnings, err := loadConfigFrom(filepath.Join(t.TempDir(), "absent.json"), noEnv)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if len(cfg.Servers) != 0 || len(warnings) != 0 {
		t.Fatalf("cfg=%+v warnings=%v, want both empty", cfg, warnings)
	}
}

func TestLoadConfigParsesAServer(t *testing.T) {
	p := writeConfig(t, `{
	  "servers": {
	    "github": {
	      "enabled": true,
	      "command": "mcp-github",
	      "args": ["--stdio"],
	      "allowTools": ["search_issues", "get_file"],
	      "envFrom": ["GITHUB_TOKEN"],
	      "callsPerMinute": 5
	    }
	  }
	}`)
	cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{"GITHUB_TOKEN": "x"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	s, ok := cfg.Servers["github"]
	if !ok {
		t.Fatalf("server missing, got %+v", cfg.Servers)
	}
	if s.Command != "mcp-github" || len(s.Args) != 1 {
		t.Fatalf("command/args = %q %v", s.Command, s.Args)
	}
	if !s.Allowed("search_issues") || s.Allowed("delete_repo") {
		t.Fatal("allow-list is not exact-match")
	}
	if s.Limit() != 5 {
		t.Fatalf("Limit = %d, want 5", s.Limit())
	}
}

// The zero value must be "off": an entry pasted in for later must not start
// running on the next invocation.
func TestServerIsOffUnlessEnabled(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"command":"c","allowTools":["t"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Servers) != 0 {
		t.Fatalf("a server with no enabled flag was loaded: %+v", cfg.Servers)
	}
	if len(warnings) != 0 {
		t.Fatalf("disabled is a normal state and should be silent, got %v", warnings)
	}
}

// An empty or absent allowTools must mean no tools, not all tools. This is the
// difference between a server update adding a tool and a server update gaining
// reach.
func TestEmptyAllowToolsMeansNoTools(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c"}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Servers["x"]
	if len(s.AllowTools) != 0 {
		t.Fatalf("AllowTools = %v", s.AllowTools)
	}
	for _, name := range []string{"anything", "read_file", ""} {
		if s.Allowed(name) {
			t.Fatalf("Allowed(%q) = true with an empty allow-list", name)
		}
	}
	if !hasWarning(warnings, "allowTools is empty") {
		t.Fatalf("no warning about the empty allow-list: %v", warnings)
	}
}

func TestAllowToolsRejectsPatterns(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["read_*","ok_tool"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Servers["x"]
	if len(s.AllowTools) != 1 || s.AllowTools[0] != "ok_tool" {
		t.Fatalf("AllowTools = %v, want only ok_tool", s.AllowTools)
	}
	if !hasWarning(warnings, "looks like a pattern") {
		t.Fatalf("a glob was dropped without saying so: %v", warnings)
	}
}

func TestAllowToolsDeduplicates(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["a","a","b"]}}}`)
	cfg, _, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Servers["x"].AllowTools; len(got) != 2 {
		t.Fatalf("AllowTools = %v, want two entries", got)
	}
}

// A server whose name cannot be part of a valid tool name has to be refused: the
// name becomes mcp__<server>__<tool>, and an illegal one would either be
// unusable or, with a "__" in it, ambiguous.
func TestServerNameMustBeAValidIdentifier(t *testing.T) {
	for _, name := range []string{"my server", "a__b", "with/slash", strings.Repeat("x", 129)} {
		p := writeConfig(t, `{"servers":{`+jsonString(name)+`:{"enabled":true,"command":"c","allowTools":["t"]}}}`)
		cfg, warnings, err := loadConfigFrom(p, noEnv)
		if err != nil {
			t.Fatalf("load(%q): %v", name, err)
		}
		if len(cfg.Servers) != 0 {
			t.Fatalf("server %q was accepted", name)
		}
		if !hasWarning(warnings, "name must be") {
			t.Fatalf("no warning for %q: %v", name, warnings)
		}
	}
}

func TestServerWithoutCommandIsRefused(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"   ","allowTools":["t"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Servers) != 0 {
		t.Fatal("a server with no command was accepted")
	}
	if !hasWarning(warnings, "no command and no url") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// ---- url servers (streamable HTTP) ---------------------------------------

func TestURLServerLoads(t *testing.T) {
	p := writeConfig(t, `{"servers":{"remote":{"enabled":true,"url":"https://mcp.example.com/mcp",`+
		`"authFrom":"REMOTE_MCP_TOKEN","allowTools":["search"],"callsPerMinute":10}}}`)
	cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{"REMOTE_MCP_TOKEN": "dummy-token"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, w := range warnings {
		t.Errorf("unexpected warning: %s", w)
	}
	s, ok := cfg.Servers["remote"]
	if !ok {
		t.Fatal("the url server was dropped")
	}
	if !s.IsHTTP() {
		t.Error("IsHTTP is false for an entry with a url")
	}
	if s.AuthFrom != "REMOTE_MCP_TOKEN" || s.Limit() != 10 {
		t.Errorf("authFrom = %q, limit = %d", s.AuthFrom, s.Limit())
	}
}

// An entry with both command and url has no defined meaning, and picking one
// would mean the file does not describe what runs.
func TestServerWithBothCommandAndURLIsRefused(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"npx","url":"https://e.example.com/mcp","allowTools":["t"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Servers) != 0 {
		t.Fatal("an entry with both transports was accepted")
	}
	if !hasWarning(warnings, "both command and url") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// A bad URL is reported when the file is read, not on the first call: the point
// of `qeuro mcp list` is that it answers "is my config valid" without contacting
// anything.
func TestURLIsValidatedAtLoad(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"http://mcp.example.com/mcp", "cleartext"},
		{"ftp://mcp.example.com/mcp", "must use https"},
		{"https://u:p@mcp.example.com/mcp", "must not embed credentials"},
		{"not a url at all", "must use https"},
	} {
		p := writeConfig(t, `{"servers":{"x":{"enabled":true,"url":`+jsonString(tc.url)+`,"allowTools":["t"]}}}`)
		cfg, warnings, err := loadConfigFrom(p, noEnv)
		if err != nil {
			t.Fatalf("load(%q): %v", tc.url, err)
		}
		if len(cfg.Servers) != 0 {
			t.Errorf("url %q was accepted", tc.url)
		}
		if !hasWarning(warnings, tc.want) {
			t.Errorf("url %q warnings = %v, want %q", tc.url, warnings, tc.want)
		}
	}
}

// A provider key named in authFrom would be sent to a remote host, which is worse
// than the envFrom case it mirrors: there the value reaches a local process.
func TestAuthFromRefusesProviderCredentials(t *testing.T) {
	for _, name := range []string{"QEURO_TOKEN", "OPENAI_API_KEY", "STRIPE_SECRET_KEY", "openrouter_key"} {
		p := writeConfig(t, `{"servers":{"x":{"enabled":true,"url":"https://e.example.com/mcp",`+
			`"authFrom":`+jsonString(name)+`,"allowTools":["t"]}}}`)
		cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{name: "dummy-secret"}))
		if err != nil {
			t.Fatalf("load(%q): %v", name, err)
		}
		if got := cfg.Servers["x"].AuthFrom; got != "" {
			t.Errorf("authFrom %q survived validation as %q", name, got)
		}
		if !hasWarning(warnings, "never sent to an MCP server") {
			t.Errorf("authFrom %q warnings = %v", name, warnings)
		}
	}
}

func TestAuthFromWarnsWhenVariableIsAbsent(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"url":"https://e.example.com/mcp",`+
		`"authFrom":"MISSING_TOKEN","allowTools":["t"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Kept, not dropped: the name is correct and the user may set it later; the
	// warning is what turns "every call returns 401" into a diagnosable state.
	if got := cfg.Servers["x"].AuthFrom; got != "MISSING_TOKEN" {
		t.Errorf("authFrom = %q, want it kept", got)
	}
	if !hasWarning(warnings, "unauthenticated") {
		t.Fatalf("warnings = %v", warnings)
	}
	if !hasWarning(warnings, "not set in this environment") {
		t.Errorf("the warning does not say the variable is absent: %v", warnings)
	}
}

// Set but empty is `export TOK=$(command that failed)`. It reaches the server as
// no Authorization header at all, so the 401 would come back advising the user to
// set authFrom — which they did. The two states need different messages.
func TestAuthFromWarnsWhenVariableIsEmpty(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"url":"https://e.example.com/mcp",`+
		`"authFrom":"EMPTY_TOKEN","allowTools":["t"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{"EMPTY_TOKEN": "   "}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Servers["x"].AuthFrom; got != "EMPTY_TOKEN" {
		t.Errorf("authFrom = %q, want it kept", got)
	}
	if !hasWarning(warnings, "set but empty") {
		t.Fatalf("warnings = %v, want the empty value distinguished from an absent one", warnings)
	}
}

// Fields that only a stdio server reads are dropped from a url entry and
// reported. Keeping them would make `qeuro mcp list` show an env var as passed to
// a server that never receives one.
func TestURLServerDropsStdioOnlyFields(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"url":"https://e.example.com/mcp","allowTools":["t"],`+
		`"args":["--stdio"],"dir":"/opt","env":{"A":"1"},"envFrom":["GITHUB_TOKEN"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{"GITHUB_TOKEN": "ghp_dummy"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Servers["x"]
	if len(s.Args) != 0 || s.Dir != "" || len(s.Env) != 0 || len(s.EnvFrom) != 0 {
		t.Errorf("stdio fields survived on a url server: %+v", s)
	}
	for _, want := range []string{"args", "dir", "env", "envFrom"} {
		if !hasWarning(warnings, want+" applies to a stdio server") {
			t.Errorf("no warning naming %q: %v", want, warnings)
		}
	}
}

// authFrom on a stdio entry would silently do nothing, and the user who wrote it
// expects a credential to be sent somewhere.
func TestAuthFromOnStdioServerIsReported(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","authFrom":"SOME_TOKEN","allowTools":["t"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{"SOME_TOKEN": "dummy"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Servers["x"].AuthFrom; got != "" {
		t.Errorf("authFrom = %q on a stdio server, want it cleared", got)
	}
	if !hasWarning(warnings, "authFrom applies to a url server") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// StartTransport is the single dispatch point, and this is what makes it worth
// having: both call sites get the same transport for the same entry.
func TestStartTransportChoosesByEntryShape(t *testing.T) {
	http, err := StartTransport(ServerConfig{
		Enabled:  true,
		URL:      "https://mcp.example.com/mcp",
		AuthFrom: "TOK",
	}, envWith(map[string]string{"TOK": "dummy-bearer"}))
	if err != nil {
		t.Fatalf("StartTransport(url): %v", err)
	}
	defer func() { _ = http.Close() }()
	if _, ok := http.(*httpTransport); !ok {
		t.Errorf("a url entry produced %T", http)
	}

	// A stdio entry with a command that does not exist still returns a transport
	// or a start error — either way, not the HTTP one.
	stdio, err := StartTransport(ServerConfig{
		Enabled: true,
		Command: "definitely-not-on-path-qeuro-test",
	}, noEnv)
	if err == nil {
		defer func() { _ = stdio.Close() }()
		if _, ok := stdio.(*httpTransport); ok {
			t.Error("a command entry produced the HTTP transport")
		}
	}
}

// The bearer token is read from the environment at start time, and the file holds
// only the variable name.
func TestStartTransportReadsBearerFromEnvironment(t *testing.T) {
	tr, err := StartTransport(ServerConfig{
		Enabled:  true,
		URL:      "https://mcp.example.com/mcp",
		AuthFrom: "REMOTE_TOKEN",
	}, envWith(map[string]string{"REMOTE_TOKEN": "dummy-bearer-value"}))
	if err != nil {
		t.Fatalf("StartTransport: %v", err)
	}
	defer func() { _ = tr.Close() }()
	ht, ok := tr.(*httpTransport)
	if !ok {
		t.Fatalf("got %T", tr)
	}
	if ht.bearer != "dummy-bearer-value" {
		t.Errorf("bearer = %q", ht.bearer)
	}
}

// This is the security property of envFrom: naming a provider credential must
// not pass it to a third-party process. roadmap.txt:333 and .ai/AI.md:49 both
// require it, and envFrom is the only hand-written path by which it could happen.
func TestEnvFromRefusesProviderCredentials(t *testing.T) {
	denied := []string{
		"QEURO_TOKEN", "QEURO_OPENROUTER_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		"OPENROUTER_API_KEY", "NVIDIA_API_KEY", "STRIPE_SECRET_KEY",
		"AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "PGPASSWORD", "REDIS_URL",
		// Lower case too: Windows environment variables are case-insensitive, so a
		// lower-case spelling resolves to the same value there.
		"openai_api_key", "qeuro_token",
	}
	env := map[string]string{}
	for _, n := range denied {
		env[n] = "secret-value"
	}
	for _, n := range denied {
		p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["t"],"envFrom":[`+jsonString(n)+`]}}}`)
		cfg, warnings, err := loadConfigFrom(p, envWith(env))
		if err != nil {
			t.Fatalf("load(%q): %v", n, err)
		}
		if got := cfg.Servers["x"].EnvFrom; len(got) != 0 {
			t.Fatalf("envFrom %q survived validation as %v", n, got)
		}
		if !hasWarning(warnings, "never passed to an MCP server") {
			t.Fatalf("no warning for %q: %v", n, warnings)
		}
	}
}

// GITHUB_TOKEN is the most common legitimate use of envFrom. A blanket rule on
// "TOKEN" would block it and push users to hard-code the value in the file,
// which is worse than what it prevents.
func TestEnvFromAllowsThirdPartyTokens(t *testing.T) {
	for _, n := range []string{"GITHUB_TOKEN", "GITLAB_TOKEN", "SENTRY_AUTH_TOKEN", "HOME"} {
		p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["t"],"envFrom":[`+jsonString(n)+`]}}}`)
		cfg, _, err := loadConfigFrom(p, envWith(map[string]string{n: "v"}))
		if err != nil {
			t.Fatalf("load(%q): %v", n, err)
		}
		if got := cfg.Servers["x"].EnvFrom; len(got) != 1 || got[0] != n {
			t.Fatalf("envFrom %q was dropped: %v", n, got)
		}
	}
}

func TestEnvFromWarnsWhenVariableIsAbsent(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["t"],"envFrom":["GITHUB_TOKEN"]}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Kept, because the server may treat it as optional.
	if len(cfg.Servers["x"].EnvFrom) != 1 {
		t.Fatal("an absent variable should still be requested, not dropped")
	}
	if !hasWarning(warnings, "not set in this environment") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestEnvFromRejectsMalformedNames(t *testing.T) {
	for _, n := range []string{"1BAD", "HAS-DASH", "HAS=EQUALS", "HAS SPACE", ""} {
		p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["t"],"envFrom":[`+jsonString(n)+`]}}}`)
		cfg, warnings, err := loadConfigFrom(p, noEnv)
		if err != nil {
			t.Fatalf("load(%q): %v", n, err)
		}
		if len(cfg.Servers["x"].EnvFrom) != 0 {
			t.Fatalf("envFrom %q was accepted", n)
		}
		if !hasWarning(warnings, "not an environment variable name") {
			t.Fatalf("no warning for %q: %v", n, warnings)
		}
	}
}

func TestLiteralEnvIsFilteredToo(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTools":["t"],
	  "env":{"QEURO_TOKEN":"stolen","MY_SETTING":"fine"}}}}`)
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env := cfg.Servers["x"].Env
	if _, bad := env["QEURO_TOKEN"]; bad {
		t.Fatal("a literal provider credential reached the server config")
	}
	if env["MY_SETTING"] != "fine" {
		t.Fatalf("env = %v", env)
	}
	if !hasWarning(warnings, "never passed to an MCP server") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// An unknown key is almost always a typo, and a typo that looks applied is the
// failure mode worth preventing: "allowTool" instead of "allowTools" would
// otherwise read as an allow-list that silently allows nothing.
func TestUnknownKeyIsAnError(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"c","allowTool":["t"]}}}`)
	if _, _, err := loadConfigFrom(p, noEnv); err == nil {
		t.Fatal("an unknown key was accepted")
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	p := writeConfig(t, `{"servers": {`)
	_, _, err := loadConfigFrom(p, noEnv)
	if err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error %q does not say what is wrong", err)
	}
}

func TestOversizedConfigIsRefused(t *testing.T) {
	p := writeConfig(t, `{"servers":{"x":{"enabled":true,"command":"`+strings.Repeat("c", maxConfigBytes)+`"}}}`)
	_, _, err := loadConfigFrom(p, noEnv)
	if err == nil {
		t.Fatal("an oversized config was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error %q does not mention the limit", err)
	}
}

func TestTooManyServersIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"servers":{`)
	for i := 0; i <= maxServers; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(jsonString("s"+itoa(i)) + `:{"enabled":true,"command":"c"}`)
	}
	b.WriteString(`}}`)
	p := writeConfig(t, b.String())
	if _, _, err := loadConfigFrom(p, noEnv); err == nil {
		t.Fatalf("more than %d servers were accepted", maxServers)
	}
}

func TestLimitDefaultsAndUnlimited(t *testing.T) {
	if got := (ServerConfig{}).Limit(); got != defaultCallsPerMinute {
		t.Fatalf("default Limit = %d, want %d", got, defaultCallsPerMinute)
	}
	if got := (ServerConfig{CallsPerMinute: -1}).Limit(); got != 0 {
		t.Fatalf("negative Limit = %d, want 0 (unlimited)", got)
	}
	if got := (ServerConfig{CallsPerMinute: 7}).Limit(); got != 7 {
		t.Fatalf("Limit = %d, want 7", got)
	}
}

func TestServerNamesAreSorted(t *testing.T) {
	cfg := Config{Servers: map[string]ServerConfig{"z": {}, "a": {}, "m": {}}}
	got := cfg.ServerNames()
	if len(got) != 3 || got[0] != "a" || got[2] != "z" {
		t.Fatalf("ServerNames = %v", got)
	}
}

// The child environment is built from scratch, so what StdioConfigFor produces is
// the complete list of what the server will see.
func TestStdioConfigForCarriesOnlyWhatWasAsked(t *testing.T) {
	s := ServerConfig{
		Command: "c",
		Env:     map[string]string{"LITERAL": "1"},
		EnvFrom: []string{"COPIED"},
	}
	cfg := StdioConfigFor(s, envWith(map[string]string{
		"COPIED":        "from-env",
		"QEURO_TOKEN":   "secret",
		"UNRELATED_VAR": "no",
	}))
	var sawLiteral, sawCopied bool
	for _, e := range cfg.Env {
		switch {
		case e == "LITERAL=1":
			sawLiteral = true
		case e == "COPIED=from-env":
			sawCopied = true
		case strings.HasPrefix(e, "QEURO_TOKEN="), strings.HasPrefix(e, "UNRELATED_VAR="):
			t.Fatalf("child environment contains %q", strings.SplitN(e, "=", 2)[0])
		}
	}
	if !sawLiteral || !sawCopied {
		t.Fatalf("env = %v, want LITERAL and COPIED present", cfg.Env)
	}
}

// The shipped example is the first thing a user copies. If it does not parse, or
// if a key in it was renamed in the loader, they get a validation error on a file
// we gave them — so the example is loaded here rather than trusted to stay
// correct.
func TestShippedExampleLoads(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "mcp.json.example"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	p := writeConfig(t, string(body))
	cfg, warnings, err := loadConfigFrom(p, noEnv)
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	// Every server in the example must ship disabled: copying it must not start
	// processes on the next invocation.
	if len(cfg.Servers) != 0 {
		t.Fatalf("the example enables servers: %v", cfg.ServerNames())
	}
	for _, w := range warnings {
		t.Fatalf("the example produces a warning: %s", w)
	}
	// A token-shaped literal in the example would fail the repository secret scan,
	// and the fingerprints in .gitleaksignore are line-bound, so it could not be
	// waived either. envFrom names variables; it never holds a value.
	if strings.Contains(string(body), "ghp_") || strings.Contains(string(body), "qeuro_live_") {
		t.Fatal("the example contains a token-shaped literal; use envFrom instead")
	}
	// Both transports are documented, because the example is where a user learns
	// that a remote server is configured with url rather than command.
	if !strings.Contains(string(body), `"url"`) {
		t.Fatal("the example documents no url server, so the HTTP transport is undiscoverable")
	}
}

// The example must be usable, not merely parseable: every entry ships disabled,
// so TestShippedExampleLoads validates almost nothing about them. Flipping the
// switch is what a user does first, and an entry that only validates while off is
// a broken example that no test would catch.
func TestShippedExampleWorksWhenEnabled(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "mcp.json.example"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	enabled := strings.ReplaceAll(string(body), `"enabled": false`, `"enabled": true`)
	if enabled == string(body) {
		t.Fatal("no entry was switched on; the example's formatting changed")
	}
	p := writeConfig(t, enabled)
	cfg, warnings, err := loadConfigFrom(p, envWith(map[string]string{
		"GITHUB_TOKEN": "dummy", "TRACKER_MCP_TOKEN": "dummy",
	}))
	if err != nil {
		t.Fatalf("the enabled example does not load: %v", err)
	}
	for _, w := range warnings {
		t.Fatalf("the enabled example produces a warning: %s", w)
	}
	if len(cfg.Servers) < 3 {
		t.Fatalf("only %d servers loaded: %v", len(cfg.Servers), cfg.ServerNames())
	}

	// And the url entry is an HTTP entry with its token wired up, rather than an
	// entry that happened to parse.
	var http int
	for _, name := range cfg.ServerNames() {
		s := cfg.Servers[name]
		if !s.IsHTTP() {
			continue
		}
		http++
		if s.AuthFrom == "" {
			t.Errorf("server %q has a url but no authFrom survived validation", name)
		}
		if s.Command != "" {
			t.Errorf("server %q has both a url and a command", name)
		}
	}
	if http == 0 {
		t.Fatal("no url entry loaded as an HTTP server")
	}
}

// jsonString quotes a Go string as a JSON string literal for the fixtures above.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
