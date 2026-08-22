package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"qeuro/internal/agentcore"
	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
	"qeuro/internal/hooks"
	"qeuro/internal/styles"
	"qeuro/internal/tui"
)

const version = "0.1.0"

const chatUsage = "qeuro chat [--local] [--local-url <url>] [--local-model <model>] [--budget <credits>]"

// command is one CLI subcommand. The registry below drives dispatch, the
// generated help text and "did you mean" suggestions, so adding a command is
// a single entry in commands() — help and dispatch can no longer drift apart.
type command struct {
	name    string
	aliases []string
	usage   string
	summary string
	run     func(args []string)
}

func commands() []command {
	return []command{
		{
			name:    "chat",
			aliases: []string{"auto"},
			usage:   chatUsage,
			summary: "open the TUI (same as running with no arguments)",
			run:     cmdChat,
		},
		{
			name:    "run",
			usage:   "qeuro run --headless --jsonl [--model <model>] [--parallel N] \"<prompt>\"",
			summary: "headless agent mode (entry point for runners); --parallel isolates each writer",
			run:     func(args []string) { os.Exit(agentcore.RunHeadless(args)) },
		},
		{
			name:    "login",
			usage:   "qeuro login [--url <url>] <token>",
			summary: "save the access token",
			run:     cmdLogin,
		},
		{
			name:    "whoami",
			usage:   "qeuro whoami",
			summary: "plan and remaining credits",
			run:     func([]string) { cmdWhoami() },
		},
		{
			name:    "cost",
			usage:   costUsage,
			summary: "what this account spent, by model and by day",
			run:     cmdCost,
		},
		{
			name:    "star",
			usage:   "qeuro star <github-login>",
			summary: "+credits for starring the repo",
			run:     cmdStar,
		},
		{
			name:    "logout",
			usage:   "qeuro logout",
			summary: "forget the token (and revoke it server-side)",
			run:     func([]string) { cmdLogout() },
		},
		{
			name:    "resume",
			usage:   "qeuro resume [id] | qeuro resume list",
			summary: "continue a previous session (newest by default)",
			run:     cmdResume,
		},
		{
			name:    "config",
			usage:   "qeuro config doctor",
			summary: "show every setting and which layer set it",
			run:     cmdConfig,
		},
		{
			name:    "mcp",
			usage:   "qeuro mcp list | tools <server> | call <server> <tool> '<json>' | serve",
			summary: "inspect configured MCP servers, or run this CLI as one",
			run:     cmdMCP,
		},
		{
			name:    "fix",
			usage:   fixUsage,
			summary: "fix the last failed shell command",
			run:     cmdFix,
		},
		{
			name:    "completion",
			usage:   completionUsage,
			summary: "print a shell completion script",
			run:     cmdCompletion,
		},
		{
			name:    "version",
			aliases: []string{"-v", "--version"},
			usage:   "qeuro version",
			summary: "client version",
			run:     func([]string) { fmt.Println(styles.Logo(version)) },
		},
		{
			name:    "help",
			aliases: []string{"-h", "--help"},
			usage:   "qeuro help",
			summary: "show this help",
			run:     func([]string) { printHelp() },
		},
	}
}

func main() {
	enableVirtualTerminal()

	args := os.Args[1:]
	// `qeuro --local` is the roadmap's primary spelling. Keep the normal command
	// registry unambiguous by desugaring only a leading offline flag into `chat`;
	// `qeuro chat --local` remains available for scripts and completions.
	if len(args) > 0 && args[0] == "--local" {
		cmdChat(args)
		return
	}
	if len(args) == 0 {
		// A signed-out interactive launch starts with an explicit setup decision,
		// before hooks or the TUI can bury it in other output. The choice itself is
		// local-only; a browser opens only after the user selects an action.
		maybePromptFirstRun(os.Stdin, os.Stdout)
		// Выполняем pre-run hook перед запуском TUI
		if ok, err := hooks.RunPreRunHook(context.Background()); !ok {
			if err != nil {
				fmt.Fprintln(os.Stderr, styles.Err.Render("pre-run hook failed:"), err)
			}
			os.Exit(1)
		}
		runTUI()
		return
	}

	name := strings.ToLower(strings.TrimSpace(args[0]))
	for _, cmd := range commands() {
		if cmd.matches(name) {
			cmd.run(args[1:])
			return
		}
	}

	fmt.Println("  " + styles.Err.Render("unknown command: ") + styles.Base.Render(args[0]))
	if suggestion := suggestCommand(name); suggestion != "" {
		fmt.Println("  " + styles.Muted.Render("did you mean: ") + styles.UserTag.Render("qeuro "+suggestion))
	}
	fmt.Println("  " + styles.Muted.Render("see: ") + styles.UserTag.Render("qeuro help"))
	os.Exit(1)
}

func (c command) matches(name string) bool {
	if name == c.name {
		return true
	}
	for _, alias := range c.aliases {
		if name == alias {
			return true
		}
	}
	return false
}

// suggestCommand returns the closest command for a likely typo: first by
// unique prefix ("who" → whoami), then by edit distance ≤ 2 ("lgoin" → login).
func suggestCommand(input string) string {
	if input == "" || strings.HasPrefix(input, "-") {
		return ""
	}
	best, bestDist := "", 3
	for _, cmd := range commands() {
		if strings.HasPrefix(cmd.name, input) {
			return cmd.name
		}
		if d := editDistance(input, cmd.name); d < bestDist {
			best, bestDist = cmd.name, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance; inputs are command-sized, so the
// O(len·len) two-row DP is more than fast enough.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// printHelp renders the command list from the registry, so it always matches
// what main() actually dispatches.
func printHelp() {
	fmt.Println(styles.Logo(version))
	fmt.Println()
	lines := []string{
		styles.Pill("INTERACTIVE", styles.Accent2) + " " + styles.Muted.Render("Run with no arguments to open the TUI."),
		styles.Subtle.Render("slash commands  ") + styles.UserTag.Render("/help"),
		"",
	}
	for _, cmd := range commands() {
		lines = append(lines, styles.UserTag.Render(cmd.usage)+strings.Repeat(" ", usagePad(cmd.usage))+styles.Muted.Render(cmd.summary))
	}
	fmt.Println(styles.Indent(styles.Frame("Commands", strings.Join(lines, "\n"), 76), "  "))
}

// usagePad aligns summaries in the help output regardless of usage length.
func usagePad(usage string) int {
	const col = 34
	if n := len([]rune(usage)); n < col {
		return col - n
	}
	return 2
}

func loadConfigOrExit() clientcfg.Config {
	cfg, err := clientcfg.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("config unreadable: ")+err.Error())
		fmt.Fprintln(os.Stderr, styles.Muted.Render("run qeuro login <token> to repair ")+styles.Base.Render(clientcfg.FilePath()))
		os.Exit(1)
	}
	return cfg
}

func loadConfigForLogin() clientcfg.Config {
	cfg, err := clientcfg.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Warn.Render("config unreadable; using defaults: ")+styles.Muted.Render(err.Error()))
	}
	return cfg
}

type loginArgs struct {
	BaseURL    string
	Token      string
	OpenSignup bool
}

func parseLoginArgs(args []string) (loginArgs, error) {
	if len(args) == 0 {
		return loginArgs{OpenSignup: true}, nil
	}
	var parsed loginArgs
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--url":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
				return loginArgs{}, fmt.Errorf("--url requires a value")
			}
			if parsed.BaseURL != "" {
				return loginArgs{}, fmt.Errorf("--url specified more than once")
			}
			parsed.BaseURL = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(arg, "-"):
			return loginArgs{}, fmt.Errorf("unknown flag %q", arg)
		case arg == "":
			return loginArgs{}, fmt.Errorf("empty token")
		default:
			if parsed.Token != "" {
				return loginArgs{}, fmt.Errorf("unexpected argument %q", arg)
			}
			parsed.Token = arg
		}
	}
	if parsed.Token == "" {
		return loginArgs{}, fmt.Errorf("provide a token: qeuro login <token>")
	}
	return parsed, nil
}

func parseStarArgs(args []string) (string, error) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("provide a GitHub login: qeuro star <github-login>")
	}
	return strings.TrimSpace(args[0]), nil
}

// cmdStar claims the GitHub star bonus for the given GitHub username.
func cmdStar(args []string) {
	username, err := parseStarArgs(args)
	if err != nil {
		fmt.Println("  " + styles.Err.Render(err.Error()))
		fmt.Println("  " + styles.Muted.Render("star the repository first, then run this command."))
		os.Exit(2)
	}
	cfg := loadConfigOrExit()
	if !cfg.LoggedIn() {
		fmt.Println("  " + styles.Warn.Render("not logged in. ") + styles.UserTag.Render("qeuro login <token>"))
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := client.New(cfg.BaseURL, cfg.Secret()).StarBonus(ctx, username)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+err.Error())
		os.Exit(1)
	}
	if res.Granted {
		fmt.Println("  " + styles.Pill("GRANTED", styles.Green) + " " +
			styles.Base.Render(fmt.Sprintf("+%.0f credits", res.Credits)) +
			styles.Subtle.Render(" · balance ") + styles.Strong.Render(fmt.Sprintf("%.1f", res.CreditsBalance)))
	} else {
		fmt.Println("  " + styles.Chip("STAR", styles.Amber) + " " + styles.Muted.Render(res.Message) +
			styles.Subtle.Render(" (balance ") + styles.Base.Render(fmt.Sprintf("%.1f", res.CreditsBalance)) + styles.Subtle.Render(")"))
	}
}

// cmdLogin parses `login [--url <url>] <token>` and saves the config.
func cmdLogin(args []string) {
	parsed, err := parseLoginArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("argument error: ")+err.Error())
		fmt.Fprintln(os.Stderr, styles.Muted.Render("example: ")+styles.UserTag.Render("qeuro login --url <url> <token>"))
		os.Exit(2)
	}
	if parsed.OpenSignup {
		cfg := loadConfigForLogin()
		openSignup(cfg, true)
		return
	}

	cfg := loadConfigForLogin()
	if parsed.BaseURL != "" {
		cfg.BaseURL = parsed.BaseURL
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = clientcfg.DefaultBaseURL
	}
	cfg.SetToken(parsed.Token)
	cfg.OnboardingOpened = true

	// Verify the token against the backend BEFORE persisting it. Saving an
	// unverified token made `qeuro login <bad-token>` succeed with exit 0 and
	// leave a broken token on disk (M3). We now persist only on success and
	// return a non-zero exit code on failure so scripts can detect it.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	me, err := client.New(cfg.BaseURL, cfg.Secret()).Me(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  "+styles.Err.Render("login failed: ")+styles.Muted.Render(err.Error()))
		fmt.Fprintln(os.Stderr, "  "+styles.Muted.Render("backend: ")+styles.Base.Render(cfg.BaseURL))
		os.Exit(3)
	}

	if err := clientcfg.Save(cfg); err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("could not save config: ")+err.Error())
		os.Exit(1)
	}
	if warning := clientcfg.TokenStorageWarning(); warning != "" {
		fmt.Fprintln(os.Stderr, "  "+styles.Warn.Render("storage warning: ")+styles.Muted.Render(warning))
	}
	body := styles.Pill("SIGNED IN", styles.Green) + " " + styles.Muted.Render(cfg.BaseURL) + "\n\n" +
		styles.FieldRow("plan", styles.Strong.Render(me.Tier), 40) + "\n" +
		styles.FieldRow("balance", styles.Strong.Render(fmt.Sprintf("%.0f credits", me.CreditsBalance)), 40)
	fmt.Println(styles.Indent(styles.Frame("Account", body, 54), "  "))
}

func cmdLogout() {
	cfg := loadConfigOrExit()
	var revokeErr error
	if cfg.LoggedIn() {
		if cfg.BaseURL == "" {
			cfg.BaseURL = clientcfg.DefaultBaseURL
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		revokeErr = client.New(cfg.BaseURL, cfg.Secret()).RevokeToken(ctx)
		cancel()
	}
	cfg.SetToken("")
	if err := clientcfg.Save(cfg); err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("could not save config: ")+err.Error())
		os.Exit(1)
	}
	fmt.Println("  " + styles.Pill("SIGNED OUT", styles.Green) + " " + styles.Muted.Render("token removed"))
	if revokeErr != nil {
		fmt.Println("  " + styles.Warn.Render("server revoke failed: ") + styles.Muted.Render(revokeErr.Error()))
	}
}

func cmdWhoami() {
	cfg := loadConfigOrExit()
	if !cfg.LoggedIn() {
		fmt.Println("  " + styles.Warn.Render("not logged in. ") + styles.UserTag.Render("qeuro login <token>"))
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	me, err := client.New(cfg.BaseURL, cfg.Secret()).Me(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+err.Error())
		os.Exit(1)
	}
	creditPct := 0
	if me.CreditsTotal > 0 {
		creditPct = int(me.CreditsBalance * 100 / me.CreditsTotal)
	}
	col := styles.Green
	if creditPct <= 10 {
		col = styles.Red
	} else if creditPct <= 30 {
		col = styles.Amber
	}
	body := styles.Chip("ACCOUNT", styles.Sky) + " " + styles.Strong.Render(me.Tier) + "\n\n" +
		styles.ProgressBar(creditPct, 28, col) + styles.Base.Render(fmt.Sprintf("  %.1f / %.0f", me.CreditsBalance, me.CreditsTotal)) + "\n\n" +
		styles.FieldRow("saved", styles.Base.Render(fmt.Sprintf("$%.4f", me.SavedUSDMonth)), 42) + "\n" +
		styles.FieldRow("backend", styles.Muted.Render(cfg.BaseURL), 42)
	fmt.Println(styles.Indent(styles.Frame("Account", body, 62), "  "))
}

func runTUI() {
	if err := tui.Run(version); err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("TUI error: ")+err.Error())
		os.Exit(1)
	}
}

// cmdChat opens the TUI, optionally with a hard session spend ceiling
// (roadmap §8, row "Стоимость"). The value goes in as a flag layer so it wins
// over env and files and shows up in `qeuro config doctor` with the right
// provenance.
func cmdChat(args []string) {
	flags, err := parseChatArgs(args)
	if err != nil {
		chatUsageError(err.Error())
	}
	if err := tui.RunWithFlags(version, flags); err != nil {
		fmt.Fprintln(os.Stderr, styles.Err.Render("TUI error: ")+err.Error())
		os.Exit(1)
	}
}

// parseChatArgs turns the command line into a config flag layer. It is separate
// from cmdChat because cmdChat's other half starts a terminal program, and the
// decision worth testing is which values become a ceiling and which are refused.
func parseChatArgs(args []string) (map[string]string, error) {
	flags := map[string]string{}
	// value binds a `--flag value` / `--flag=value` pair to a setting key. It
	// refuses an empty value rather than recording it, because resolve() skips
	// empty flag values: `--local-url=` would otherwise look accepted and then
	// silently fall through to the default endpoint.
	value := func(i *int, name, key string) error {
		a := args[*i]
		if eq := name + "="; strings.HasPrefix(a, eq) {
			v := strings.TrimPrefix(a, eq)
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s needs a value", name)
			}
			flags[key] = v
			return nil
		}
		if *i+1 >= len(args) || strings.TrimSpace(args[*i+1]) == "" {
			return fmt.Errorf("%s needs a value", name)
		}
		*i = *i + 1
		flags[key] = args[*i]
		return nil
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--budget" || strings.HasPrefix(a, "--budget="):
			if err := value(&i, "--budget", "budget"); err != nil {
				return nil, fmt.Errorf("--budget needs a value, e.g. --budget 20")
			}
		// Offline mode (roadmap §8 row "Offline"). `--local` is a switch, so it
		// takes no value; the other two carry the endpoint and the model name.
		case a == "--local":
			flags["local"] = "true"
		case a == "--local-url" || strings.HasPrefix(a, "--local-url="):
			if err := value(&i, "--local-url", "local_url"); err != nil {
				return nil, err
			}
		case a == "--local-model" || strings.HasPrefix(a, "--local-model="):
			if err := value(&i, "--local-model", "local_model"); err != nil {
				return nil, err
			}
		default:
			// An unknown argument is refused rather than ignored: `qeuro chat
			// --budgt 5` silently starting an unlimited session would be the one
			// failure this flag exists to prevent.
			return nil, fmt.Errorf("unknown argument %s", strconv.Quote(a))
		}
	}

	// An endpoint typed on this command line is an explicit request, so an
	// unusable one stops here. clientcfg only warns for the env/file layers,
	// because a bad file must not lock the user out of their own CLI — but a flag
	// silently falling back to localhost would send the prompt somewhere the user
	// did not ask for.
	if v, ok := flags["local_url"]; ok {
		if err := client.ValidateLocalURL(v); err != nil {
			return nil, err
		}
	}
	// Naming an endpoint or a model without --local reads as "use it", and the
	// gap between that and what would happen (the backend, silently) is exactly
	// the kind of invisible precedence this row's sibling rows exist to remove.
	if _, ok := flags["local"]; !ok {
		for _, k := range []string{"local_url", "local_model"} {
			if _, set := flags[k]; set {
				return nil, fmt.Errorf("--%s only applies with --local", strings.ReplaceAll(k, "_", "-"))
			}
		}
	}
	// A ceiling that does not parse must stop here, not become "unlimited" three
	// layers down. clientcfg warns and continues (a bad file must not lock the
	// user out), but a flag typed on this command line is an explicit request.
	if v, ok := flags["budget"]; ok {
		// NaN and ±Inf parse successfully and are not caught by `<= 0`: NaN fails
		// every comparison, so a NaN ceiling would compare as never exhausted —
		// an unlimited session wearing a limit's clothes.
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
			return nil, fmt.Errorf("--budget must be a positive number of credits, got %s", strconv.Quote(v))
		}
	}
	if _, local := flags["local"]; local {
		if _, budget := flags["budget"]; budget {
			// A local server emits no billing receipt, so spent can never advance and
			// this would be an unlimited session displaying a hard limit it cannot
			// enforce. Refuse that dishonest combination instead of accepting a flag
			// whose defining property is a stop.
			return nil, fmt.Errorf("--budget cannot be used with --local: local servers do not report Qeuro credits")
		}
	}
	return flags, nil
}

func chatUsageError(msg string) {
	fmt.Fprintln(os.Stderr, styles.Err.Render("error: ")+msg)
	fmt.Fprintln(os.Stderr, styles.Muted.Render("usage: "+chatUsage))
	os.Exit(2)
}
