package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"qeuro/internal/client"
)

// `qeuro cost` answers a question about money, so the parts under test are the
// two places it could quietly answer the wrong question: the window parser, and
// the rendering of strings the server chose.

func TestCostCommandIsRegistered(t *testing.T) {
	var found *command
	for _, cmd := range commands() {
		if cmd.matches("cost") {
			c := cmd
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal(`no "cost" command: roadmap §8 requires "qeuro cost --since 7d"`)
	}
	if found.run == nil || found.usage == "" || found.summary == "" {
		t.Fatal(`"cost" is registered but not dispatchable or not in help`)
	}
	if !strings.Contains(found.usage, "--since") {
		t.Fatalf("usage %q does not mention --since, which is the flag the row asks for", found.usage)
	}
}

func TestParseSinceWindows(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"7d", 7, "the documented form"},
		{"1d", 1, "a single day"},
		{"30", 30, "a bare number is a day count"},
		{"2w", 14, "weeks are seven days each"},
		{"24h", 1, "a day of hours is one day"},
		{"1h", 1, "hours round up: the server buckets by whole UTC days"},
		{"25h", 2, "25 hours spans two day buckets, so it must not report one"},
		{"48h", 2, "exactly two days of hours"},
		{"  7D  ", 7, "case and surrounding whitespace are shell accidents, not requests"},
	}
	for _, c := range cases {
		got, err := parseSince(c.in)
		if err != nil {
			t.Errorf("parseSince(%q) errored: %v (%s)", c.in, err, c.why)
			continue
		}
		if got != c.want {
			t.Errorf("parseSince(%q) = %d, want %d: %s", c.in, got, c.want, c.why)
		}
	}
}

// Every one of these must fail rather than fall back to the default window. A
// summary that silently covers a different period than the one asked for is a
// wrong answer, not a lenient one.
func TestParseSinceRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "   ", "d", "w", "h", "0", "0d", "0h", "-5", "-5d", "7x", "abc", "7 d", "1.5d", "∞"} {
		if got, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) = %d with no error, want a refusal", in, got)
		}
	}
}

func TestParseCostArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantDays int
		wantJSON bool
	}{
		{"no args is a week", nil, 7, false},
		{"separate value", []string{"--since", "30d"}, 30, false},
		{"equals form", []string{"--since=14d"}, 14, false},
		{"json only", []string{"--json"}, 7, true},
		{"both, either order", []string{"--json", "--since", "1d"}, 1, true},
		{"both, reversed", []string{"--since", "1d", "--json"}, 1, true},
		{"last --since wins", []string{"--since", "3d", "--since", "5d"}, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			days, jsonOut, err := parseCostArgs(c.args)
			if err != nil {
				t.Fatalf("parseCostArgs(%v) errored: %v", c.args, err)
			}
			if days != c.wantDays {
				t.Errorf("days = %d, want %d", days, c.wantDays)
			}
			if jsonOut != c.wantJSON {
				t.Errorf("jsonOut = %v, want %v", jsonOut, c.wantJSON)
			}
		})
	}
}

// A mistyped flag must not be ignored: `qeuro cost --sicne 30d` reporting the
// default week looks like an answer and is not one.
func TestParseCostArgsRefusesUnknownArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--sicne", "30d"},
		{"--since"},
		{"--since", "banana"},
		{"--since="},
		{"-j"},
		{"30d"},
		{"--json=true"},
	} {
		if _, _, err := parseCostArgs(args); err == nil {
			t.Errorf("parseCostArgs(%v) accepted arguments it cannot honour", args)
		}
	}
}

func usageFixture() *client.UsageResponse {
	return &client.UsageResponse{
		Days:  7,
		Since: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		Totals: client.UsageTotals{
			Requests: 42, InTokens: 1_500_000, OutTokens: 90_000,
			CostUSD: 1.2345, Credits: 123.4, SavedUSD: 0.5,
		},
		Models: []client.UsageModel{
			{Model: "deepseek/deepseek-v3", Requests: 30, Credits: 100, CostUSD: 1.0},
			{Model: "anthropic/claude-sonnet-4", Requests: 12, Credits: 23.4, CostUSD: 0.2345},
		},
		Series: []client.UsageDay{
			{Day: "2026-08-12", Requests: 20, Credits: 60},
			{Day: "2026-08-13", Requests: 22, Credits: 63.4},
		},
		CreditsBalance: 876.5,
	}
}

func TestRenderCostShowsTheNumbersItWasGiven(t *testing.T) {
	got := renderCost(usageFixture())
	for _, want := range []string{
		"7 days", "2026-08-07", // the window, so the totals can be interpreted
		"42",         // calls
		"123.4",      // credits spent
		"$1.2345",    // cost
		"1.5M",       // input tokens, shortened
		"876.5",      // balance
		"deepseek",   // the top spender
		"2026-08-12", // the day rows
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output is missing %q:\n%s", want, got)
		}
	}
}

// An empty window is a normal state, not an error, and it must still report the
// balance: "nothing spent" and "cannot tell you" are different answers.
func TestRenderCostOnAnEmptyWindow(t *testing.T) {
	got := renderCost(&client.UsageResponse{
		Days:           1,
		Since:          time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		CreditsBalance: 100,
		Models:         []client.UsageModel{},
		Series:         []client.UsageDay{},
	})
	if !strings.Contains(got, "no billed calls") {
		t.Errorf("an empty window should say so:\n%s", got)
	}
	if !strings.Contains(got, "100") {
		t.Errorf("the balance should still be shown:\n%s", got)
	}
	if !strings.Contains(got, "1 day") || strings.Contains(got, "1 days") {
		t.Errorf("the window label should be singular for one day:\n%s", got)
	}
}

// The model id and the day string are stored values chosen elsewhere. If one
// carried a CSI sequence it would repaint the table it appears in, so both go
// through the one-line escape (.ai/SECURITY.md:33).
func TestRenderCostSanitisesServerSuppliedStrings(t *testing.T) {
	got := renderCost(&client.UsageResponse{
		Days:   2,
		Since:  time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		Totals: client.UsageTotals{Requests: 1, Credits: 1},
		Models: []client.UsageModel{
			{Model: "evil\x1b[2Kmodel", Requests: 1, Credits: 1},
		},
		Series: []client.UsageDay{
			{Day: "2026\x1b[2K", Requests: 1, Credits: 0.5},
			{Day: "2026-08-13", Requests: 1, Credits: 0.5},
		},
	})
	if strings.Contains(got, "\x1b[2K") {
		t.Fatalf("an escape sequence from the server reached the terminal:\n%q", got)
	}
	if !strings.Contains(got, `\x1b`) {
		t.Fatalf("the escape should be shown in visible form:\n%s", got)
	}
}

// `--json` feeds a program, so the one property that matters is that the
// document parses. This is the regression guard for a real defect: the first
// version built the JSON with fmt and %q, which renders a control byte as \x1b —
// Go quoting, not JSON quoting. JSON has no \x escape, so a single model id with
// an escape sequence in it made the whole document unparseable for the script
// that asked for it.
func TestCostJSONParsesWithHostileServerStrings(t *testing.T) {
	u := usageFixture()
	u.Models[0].Model = "evil\x1b[2Kmodel"
	u.Models[1].Model = `quote"backslash\and	tab`
	u.Series[0].Day = "2026\x1b[2K"

	var buf bytes.Buffer
	if err := writeCostJSON(&buf, u); err != nil {
		t.Fatalf("writeCostJSON errored: %v", err)
	}

	var got struct {
		Days     int     `json:"days"`
		Requests int     `json:"requests"`
		Credits  float64 `json:"credits"`
		InTokens int64   `json:"in_tokens"`
		Models   []struct {
			Model   string  `json:"model"`
			Credits float64 `json:"credits"`
		} `json:"models"`
		Series []struct {
			Day string `json:"day"`
		} `json:"series"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("the emitted document does not parse as JSON: %v\n%s", err, buf.String())
	}

	// The values must survive intact, not be mangled: a script parsing this is
	// matching on the model id, and DisplaySafe-ing it here would corrupt the data.
	if got.Models[0].Model != "evil\x1b[2Kmodel" {
		t.Errorf("model id was altered on the way through: %q", got.Models[0].Model)
	}
	if got.Models[1].Model != u.Models[1].Model {
		t.Errorf("quotes and backslashes did not round-trip: %q", got.Models[1].Model)
	}
	if got.Series[0].Day != "2026\x1b[2K" {
		t.Errorf("day string was altered: %q", got.Series[0].Day)
	}
	if got.Days != 7 || got.Requests != 42 || got.Credits != 123.4 || got.InTokens != 1_500_000 {
		t.Errorf("totals did not round-trip: %+v", got)
	}
}

// A script iterating `.models` should not have to special-case null, so the
// arrays are always present even when the window is empty.
func TestCostJSONEmitsArraysNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCostJSON(&buf, &client.UsageResponse{Days: 1, Since: time.Now().UTC()}); err != nil {
		t.Fatalf("writeCostJSON errored: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("empty window did not produce parseable JSON: %v", err)
	}
	for _, key := range []string{"models", "series"} {
		if string(got[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, got[key])
		}
	}
	// Every field the framed output shows must be present for the machine reader
	// too, or `--json` is a lesser answer to the same question.
	for _, key := range []string{"days", "since", "requests", "credits", "cost_usd", "saved_usd", "in_tokens", "out_tokens", "credits_balance"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the document is missing %q", key)
		}
	}
}

func TestTruncateNameCountsRunes(t *testing.T) {
	// A byte-wise cut through this string would emit a replacement character in
	// the middle of the table.
	got := truncateName("модель-с-очень-длинным-именем-которое-не-влезает", 10)
	if n := len([]rune(got)); n != 10 {
		t.Fatalf("truncated to %d runes, want 10: %q", n, got)
	}
	if strings.Contains(got, "\uFFFD") {
		t.Fatalf("the cut fell mid-rune: %q", got)
	}
	if short := truncateName("gpt-4", 28); short != "gpt-4" {
		t.Fatalf("a short name was altered: %q", short)
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[int64]string{
		0: "0", 999: "999", 1_000: "1.0k", 1_500: "1.5k",
		999_999: "1000.0k", 1_000_000: "1.0M", 2_500_000: "2.5M",
	}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
}
