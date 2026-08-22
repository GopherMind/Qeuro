package main

// `qeuro cost` — what this account spent, over a window (roadmap §8, row
// "Стоимость"). The status bar already shows the balance during a session; this
// answers the other half of the question, which is where the credits went.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/styles"
)

const costUsage = "qeuro cost [--since 7d|24h|30] [--json]"

func cmdCost(args []string) {
	days, jsonOut, err := parseCostArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+err.Error())
		fmt.Fprintln(os.Stderr, styles.Muted.Render("usage: "+costUsage))
		os.Exit(2)
	}

	cfg := loadConfigOrExit()
	if !cfg.LoggedIn() {
		fmt.Println("  " + styles.Warn.Render("not logged in. ") + styles.UserTag.Render("qeuro login <token>"))
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	usage, err := client.New(cfg.BaseURL, cfg.Secret()).Usage(ctx, days)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+err.Error())
		os.Exit(1)
	}

	if jsonOut {
		printCostJSON(usage)
		return
	}
	fmt.Println(styles.Indent(renderCost(usage), "  "))
}

// parseCostArgs reads the flags. Unknown flags are an error rather than being
// ignored: a mistyped `--since` silently reporting the default window would be a
// wrong answer to a question about money.
func parseCostArgs(args []string) (days int, jsonOut bool, err error) {
	days = 7
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json":
			jsonOut = true
		case a == "--since":
			if i+1 >= len(args) {
				return 0, false, fmt.Errorf("--since needs a value, e.g. --since 7d")
			}
			i++
			days, err = parseSince(args[i])
			if err != nil {
				return 0, false, err
			}
		case strings.HasPrefix(a, "--since="):
			days, err = parseSince(strings.TrimPrefix(a, "--since="))
			if err != nil {
				return 0, false, err
			}
		default:
			return 0, false, fmt.Errorf("unknown argument %q", a)
		}
	}
	return days, jsonOut, nil
}

// parseSince accepts `7d`, `24h` and a bare day count. Hours are rounded up to a
// whole day because the server buckets by UTC day: accepting `6h` and answering
// with a full day would be a quieter lie than refusing it.
func parseSince(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("--since needs a value, e.g. --since 7d")
	}
	mult := 1
	num := s
	switch {
	case strings.HasSuffix(s, "d"):
		num = strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "w"):
		num, mult = strings.TrimSuffix(s, "w"), 7
	case strings.HasSuffix(s, "h"):
		hours, err := strconv.Atoi(strings.TrimSuffix(s, "h"))
		if err != nil || hours <= 0 {
			return 0, fmt.Errorf("bad --since value %q", s)
		}
		return (hours + 23) / 24, nil
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad --since value %q (try 7d, 24h, 2w)", s)
	}
	return n * mult, nil
}

// costJSON is the shape `--json` emits. It is a declared type marshalled by
// encoding/json rather than a Printf template, because the values include a
// model id chosen elsewhere: fmt's %q is Go quoting, not JSON quoting, and it
// renders a control byte as \x1b — a sequence JSON has no escape for, which
// makes the whole document unparseable for the script that asked for it.
type costJSON struct {
	Days           int             `json:"days"`
	Since          string          `json:"since"`
	Requests       int             `json:"requests"`
	Credits        float64         `json:"credits"`
	CostUSD        float64         `json:"cost_usd"`
	SavedUSD       float64         `json:"saved_usd"`
	InTokens       int64           `json:"in_tokens"`
	OutTokens      int64           `json:"out_tokens"`
	CreditsBalance float64         `json:"credits_balance"`
	Models         []costJSONModel `json:"models"`
	Series         []costJSONDay   `json:"series"`
}

type costJSONModel struct {
	Model    string  `json:"model"`
	Requests int     `json:"requests"`
	Credits  float64 `json:"credits"`
	CostUSD  float64 `json:"cost_usd"`
}

type costJSONDay struct {
	Day      string  `json:"day"`
	Requests int     `json:"requests"`
	Credits  float64 `json:"credits"`
}

// printCostJSON emits the server's numbers for scripts. It prints what the
// server said rather than a re-derived summary, so a script and the framed
// output can never disagree.
//
// Values are NOT put through DisplaySafe here: this output is for a program, and
// mangling a model id would corrupt the data the script is parsing. Escaping is
// the terminal renderer's concern (renderCost), not the machine format's —
// encoding/json already escapes control characters into valid \u sequences.
func printCostJSON(u *client.UsageResponse) {
	if err := writeCostJSON(os.Stdout, u); err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+err.Error())
		os.Exit(1)
	}
}

// writeCostJSON is split out from printCostJSON so a test can parse the document
// this command actually emits. The exit lives at the edge, in the caller.
func writeCostJSON(w io.Writer, u *client.UsageResponse) error {
	out := costJSON{
		Days:           u.Days,
		Since:          u.Since.UTC().Format(time.RFC3339),
		Requests:       u.Totals.Requests,
		Credits:        u.Totals.Credits,
		CostUSD:        u.Totals.CostUSD,
		SavedUSD:       u.Totals.SavedUSD,
		InTokens:       u.Totals.InTokens,
		OutTokens:      u.Totals.OutTokens,
		CreditsBalance: u.CreditsBalance,
		// Empty rather than nil so the document always has arrays: a script
		// iterating `.models` should not have to special-case null.
		Models: make([]costJSONModel, 0, len(u.Models)),
		Series: make([]costJSONDay, 0, len(u.Series)),
	}
	for _, m := range u.Models {
		out.Models = append(out.Models, costJSONModel{
			Model: m.Model, Requests: m.Requests, Credits: m.Credits, CostUSD: m.CostUSD,
		})
	}
	for _, d := range u.Series {
		out.Series = append(out.Series, costJSONDay{Day: d.Day, Requests: d.Requests, Credits: d.Credits})
	}
	return json.NewEncoder(w).Encode(out)
}

func renderCost(u *client.UsageResponse) string {
	var b strings.Builder
	window := fmt.Sprintf("%d day", u.Days)
	if u.Days != 1 {
		window += "s"
	}
	b.WriteString(styles.Chip("SPEND", styles.Sky) + " " +
		styles.Strong.Render(window) + " " +
		styles.Muted.Render("since "+u.Since.UTC().Format("2006-01-02")) + "\n\n")

	if u.Totals.Requests == 0 {
		b.WriteString(styles.Muted.Render("no billed calls in this window") + "\n")
		b.WriteString(styles.FieldRow("balance", styles.Base.Render(fmt.Sprintf("%.1f credits", u.CreditsBalance)), 56))
		return styles.Frame("Cost", b.String(), 64)
	}

	b.WriteString(styles.FieldRow("calls", styles.Base.Render(strconv.Itoa(u.Totals.Requests)), 56) + "\n")
	b.WriteString(styles.FieldRow("credits", styles.Base.Render(fmt.Sprintf("%.1f", u.Totals.Credits)), 56) + "\n")
	b.WriteString(styles.FieldRow("cost", styles.Base.Render(fmt.Sprintf("$%.4f", u.Totals.CostUSD)), 56) + "\n")
	b.WriteString(styles.FieldRow("tokens", styles.Base.Render(fmt.Sprintf("%s in · %s out",
		humanCount(u.Totals.InTokens), humanCount(u.Totals.OutTokens))), 56) + "\n")
	if u.Totals.SavedUSD > 0 {
		b.WriteString(styles.FieldRow("saved", styles.Base.Render(fmt.Sprintf("$%.4f", u.Totals.SavedUSD)), 56) + "\n")
	}
	b.WriteString(styles.FieldRow("balance", styles.Base.Render(fmt.Sprintf("%.1f credits", u.CreditsBalance)), 56) + "\n")

	if len(u.Models) > 0 {
		b.WriteString("\n" + styles.Muted.Render("by model") + "\n")
		// Bars are relative to the top spender, so the shape of the list shows
		// where the money went without the reader doing arithmetic.
		top := u.Models[0].Credits
		for _, m := range u.Models {
			pct := 0
			if top > 0 {
				pct = int(m.Credits * 100 / top)
			}
			// The model id is a stored string that reaches the terminal, so it goes
			// through the same one-line escape as any other server-supplied text
			// (.ai/SECURITY.md:33). A row that could emit a CSI sequence would let
			// stored data repaint the table it appears in.
			name := clientcfg.DisplaySafe(truncateName(m.Model, 28))
			b.WriteString(styles.ProgressBar(pct, 16, styles.Sky) + " " +
				styles.Base.Render(fmt.Sprintf("%-28s %6.1f cr  %d×",
					name, m.Credits, m.Requests)) + "\n")
		}
	}

	if len(u.Series) > 1 {
		b.WriteString("\n" + styles.Muted.Render("by day") + "\n")
		for _, d := range u.Series {
			day := clientcfg.DisplaySafe(truncateName(d.Day, 10))
			b.WriteString(styles.Base.Render(fmt.Sprintf("  %-10s  %6.1f cr  %d×", day, d.Credits, d.Requests)) + "\n")
		}
	}
	return styles.Frame("Cost", strings.TrimRight(b.String(), "\n"), 64)
}

// humanCount shortens token counts, which routinely run to seven figures and
// would otherwise push the row past the frame.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// truncateName keeps a long model id from breaking the row. It counts runes, not
// bytes: a byte-slice cut mid-rune would emit a replacement character into the
// middle of the table. The name comes from the backend catalogue and is
// display-only here.
func truncateName(s string, width int) string {
	r := []rune(s)
	if len(r) <= width || width < 1 {
		return s
	}
	return string(r[:width-1]) + "…"
}
