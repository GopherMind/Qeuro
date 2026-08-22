package main

import "testing"

// The cloud worker executes `qeuro run --headless --jsonl -- "<prompt>"`
// (cloud-worker/execute.go). That command was implemented in
// internal/agentcore but never registered in commands(), so every invocation
// fell through to "unknown command: run" and exited 1 — which made every cloud
// run fail before the agent started. These tests pin the registration so the
// worker's contract cannot silently regress again.
func TestRunCommandIsRegistered(t *testing.T) {
	var found *command
	for _, cmd := range commands() {
		if cmd.matches("run") {
			c := cmd
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal(`no "run" command: cloud-worker invokes "qeuro run --headless --jsonl" and would exit 1`)
	}
	if found.run == nil {
		t.Fatal(`"run" command has a nil run func`)
	}
	if found.usage == "" || found.summary == "" {
		t.Error(`"run" command is missing usage/summary, so it is absent from help output`)
	}
}

// Every command the worker or a user can reach must be dispatchable and appear
// in help. This guards the registry as a whole, not just "run".
func TestCommandRegistryIsWellFormed(t *testing.T) {
	seen := map[string]string{}
	for _, cmd := range commands() {
		if cmd.name == "" {
			t.Error("a command has an empty name")
			continue
		}
		if cmd.run == nil {
			t.Errorf("command %q has a nil run func", cmd.name)
		}
		for _, n := range append([]string{cmd.name}, cmd.aliases...) {
			if prev, dup := seen[n]; dup {
				t.Errorf("name/alias %q is claimed by both %q and %q", n, prev, cmd.name)
			}
			seen[n] = cmd.name
		}
	}
}

func TestParseLoginArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantURL    string
		wantToken  string
		wantSignup bool
		wantErr    bool
	}{
		{name: "no args opens signup", wantSignup: true},
		{name: "token only", args: []string{"qeuro_live_abc"}, wantToken: "qeuro_live_abc"},
		{name: "url and token", args: []string{"--url", "http://127.0.0.1:8090", "qeuro_live_abc"}, wantURL: "http://127.0.0.1:8090", wantToken: "qeuro_live_abc"},
		{name: "duplicate token rejected", args: []string{"token1", "token2"}, wantErr: true},
		{name: "unknown flag rejected", args: []string{"--token", "abc"}, wantErr: true},
		{name: "missing url rejected", args: []string{"--url"}, wantErr: true},
		{name: "flag-looking url rejected", args: []string{"--url", "--bad", "token"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLoginArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLoginArgs(%v) succeeded, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLoginArgs(%v): %v", tt.args, err)
			}
			if got.BaseURL != tt.wantURL || got.Token != tt.wantToken || got.OpenSignup != tt.wantSignup {
				t.Fatalf("parseLoginArgs(%v) = %+v", tt.args, got)
			}
		})
	}
}

func TestParseStarArgs(t *testing.T) {
	if got, err := parseStarArgs([]string{"  octocat  "}); err != nil || got != "octocat" {
		t.Fatalf("parseStarArgs valid = %q, %v", got, err)
	}
	for _, args := range [][]string{{}, {""}, {"octocat", "extra"}} {
		if got, err := parseStarArgs(args); err == nil {
			t.Fatalf("parseStarArgs(%v) = %q, want error", args, got)
		}
	}
}

// --budget is the one flag whose failure mode is expensive: anything this
// function lets through unvalidated becomes an unlimited session that the user
// believes is capped.
func TestParseChatArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // "" means no budget flag at all
	}{
		{"no args means no ceiling", nil, ""},
		{"separate value", []string{"--budget", "20"}, "20"},
		{"equals form", []string{"--budget=20"}, "20"},
		{"fractional", []string{"--budget", "0.5"}, "0.5"},
		{"surrounding space survives to the layer", []string{"--budget", " 20 "}, " 20 "},
		{"last one wins", []string{"--budget", "5", "--budget", "9"}, "9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flags, err := parseChatArgs(c.args)
			if err != nil {
				t.Fatalf("parseChatArgs(%v) errored: %v", c.args, err)
			}
			got, ok := flags["budget"]
			if c.want == "" {
				if ok {
					t.Fatalf("budget = %q, want no flag layer at all", got)
				}
				return
			}
			if !ok {
				t.Fatalf("no budget flag produced for %v", c.args)
			}
			if got != c.want {
				t.Fatalf("budget = %q, want %q", got, c.want)
			}
		})
	}
}

// Each of these must stop the command rather than start an unlimited session.
// "nan" and "inf" are the interesting ones: both parse as float64, neither is
// caught by a `<= 0` check, and a NaN ceiling compares as never exhausted.
func TestParseChatArgsRefusesUnusableCeilings(t *testing.T) {
	for _, args := range [][]string{
		{"--budget"},
		{"--budget", "banana"},
		{"--budget="},
		{"--budget", "0"},
		{"--budget", "-5"},
		{"--budget", "nan"},
		{"--budget", "NaN"},
		{"--budget", "inf"},
		{"--budget", "+Inf"},
		{"--budget", "-inf"},
		{"--budget", ""},
		{"--budgt", "5"},
		{"20"},
		{"--json"},
	} {
		if flags, err := parseChatArgs(args); err == nil {
			t.Errorf("parseChatArgs(%v) accepted it and produced %v", args, flags)
		}
	}
}

// Offline mode (roadmap §8 row "Offline"). The flag layer is what decides
// whether inference leaves this machine, so the interesting assertions are about
// what must NOT be accepted silently: an endpoint without --local (which would
// go to the backend while reading as local), and an endpoint that is not a plain
// http(s) URL.
func TestParseChatArgsLocalMode(t *testing.T) {
	ok := []struct {
		name string
		args []string
		want map[string]string
	}{
		{"bare switch", []string{"--local"}, map[string]string{"local": "true"}},
		{"endpoint", []string{"--local", "--local-url", "http://127.0.0.1:1234"},
			map[string]string{"local": "true", "local_url": "http://127.0.0.1:1234"}},
		{"equals form", []string{"--local", "--local-url=http://127.0.0.1:1234"},
			map[string]string{"local": "true", "local_url": "http://127.0.0.1:1234"}},
		{"model name", []string{"--local", "--local-model", "qwen2.5-coder:7b"},
			map[string]string{"local": "true", "local_model": "qwen2.5-coder:7b"}},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			flags, err := parseChatArgs(c.args)
			if err != nil {
				t.Fatalf("parseChatArgs(%v): %v", c.args, err)
			}
			for k, want := range c.want {
				if flags[k] != want {
					t.Errorf("flags[%q] = %q, want %q", k, flags[k], want)
				}
			}
			if len(flags) != len(c.want) {
				t.Errorf("flags = %v, want exactly %v", flags, c.want)
			}
		})
	}

	for _, args := range [][]string{
		// An endpoint or model without --local would send the prompt to the backend
		// while the command line says otherwise.
		{"--local-url", "http://127.0.0.1:1234"},
		{"--local-model", "qwen2.5-coder:7b"},
		// Missing or empty values must not fall through to the default endpoint.
		{"--local", "--local-url"},
		{"--local", "--local-url="},
		{"--local", "--local-url", ""},
		{"--local", "--local-model"},
		// Not an http(s) origin: a file:// "endpoint" is a local read, and
		// credentials in a URL end up in a request line.
		{"--local", "--local-url", "file:///etc/passwd"},
		{"--local", "--local-url", "localhost:11434"},
		{"--local", "--local-url", "http://user:pass@localhost:11434"},
		// Near-misses of the switch itself.
		{"--locl"},
		{"--local=true"},
		// A credit ceiling on a session that bills no credits can never stop
		// anything, so accepting it would display a hard limit that is decorative.
		{"--local", "--budget", "9"},
		{"--budget", "9", "--local"},
	} {
		if flags, err := parseChatArgs(args); err == nil {
			t.Errorf("parseChatArgs(%v) accepted it and produced %v", args, flags)
		}
	}
}
