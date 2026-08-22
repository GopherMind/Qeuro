package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"qeuro/internal/mcp"
	"qeuro/internal/styles"
	"qeuro/internal/tools"
)

// cmdMCP implements `qeuro mcp list|tools|call` (roadmap §4.8).
//
// The subcommands exist because the alternative is debugging MCP through the
// chat loop, where a server that fails to start looks like a model that chose not
// to use a tool. `list` answers "did my config load"; `tools` answers "what does
// the server actually offer, and what did my allow-list admit"; `call` answers
// "does the tool work at all", without spending a model request to find out.
func cmdMCP(args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	switch sub {
	case "list":
		os.Exit(mcpList(os.Stdout, os.Stderr))
	case "tools":
		os.Exit(mcpTools(os.Stdout, os.Stderr, args))
	case "call":
		os.Exit(mcpCall(os.Stdout, os.Stderr, args))
	case "serve":
		os.Exit(mcp.Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	default:
		fmt.Fprintln(os.Stderr, "  "+styles.Err.Render("unknown subcommand: ")+styles.Base.Render(sub))
		fmt.Fprintln(os.Stderr, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render(mcpUsage))
		os.Exit(2)
	}
}

const mcpUsage = "qeuro mcp list | tools <server> | call <server> <tool> '<json>' | serve"

// mcpList reports what mcp.json declares, without starting anything.
//
// Nothing is connected here on purpose: "which servers are configured" must be
// answerable when a server is broken, and starting a process to answer it would
// make the broken case the one where the command is least useful.
func mcpList(out, errOut io.Writer) int {
	cfg, warnings, err := mcp.LoadConfig()
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("mcp: ")+styles.Muted.Render(err.Error()))
		return 1
	}

	names := cfg.ServerNames()
	if len(names) == 0 {
		fmt.Fprintln(out, "  "+styles.Muted.Render("no enabled MCP servers in ")+styles.Base.Render(mcp.ConfigPath()))
		fmt.Fprintln(out, "  "+styles.Subtle.Render("see mcp.json.example in the repository for a starting point"))
	}

	var b strings.Builder
	for i, name := range names {
		s := cfg.Servers[name]
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(styles.FieldRow(name, styles.Base.Render(commandLine(s)), 58) + "\n")
		b.WriteString("  " + styles.Subtle.Render("transport     ") + styles.Base.Render(transportLabel(s)) + "\n")
		b.WriteString("  " + styles.Subtle.Render("allowed tools ") + styles.Base.Render(joinOr(s.AllowTools, "(none)")) + "\n")
		b.WriteString("  " + styles.Subtle.Render("rate limit    ") + styles.Base.Render(limitText(s.Limit())) + "\n")
		if len(s.EnvFrom) > 0 {
			// Names only. The values are credentials, and this output is pasted into
			// issues.
			b.WriteString("  " + styles.Subtle.Render("env from      ") + styles.Base.Render(strings.Join(s.EnvFrom, ", ")) + "\n")
		}
		if len(s.Env) > 0 {
			b.WriteString("  " + styles.Subtle.Render("env keys      ") + styles.Base.Render(strings.Join(sortedKeys(s.Env), ", ")) + "\n")
		}
	}
	if b.Len() > 0 {
		fmt.Fprintln(out, styles.Indent(styles.Frame("MCP servers", b.String(), 74), "  "))
	}
	fmt.Fprintln(out, "  "+styles.Subtle.Render("config ")+styles.Base.Render(mcp.ConfigPath()))
	fmt.Fprintln(out, "  "+styles.Subtle.Render("every MCP tool call asks for approval; no server receives provider keys"))

	for _, w := range warnings {
		fmt.Fprintln(errOut, "  "+styles.Warn.Render("warning: ")+styles.Muted.Render(w))
	}
	if len(warnings) > 0 {
		// Non-zero so a script can gate on "my MCP config is clean", the same
		// contract `qeuro config doctor` has.
		return 1
	}
	return 0
}

// mcpTools starts one server and lists everything it advertises, marking which
// tools the allow-list admits.
//
// It shows tools that are NOT allowed as well, because the question this answers
// is usually "why can the model not use X" and the answer is normally either a
// misspelled name or a tool that is genuinely absent. Hiding the ones that were
// filtered would leave only the second explanation visible.
func mcpTools(out, errOut io.Writer, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(errOut, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render("qeuro mcp tools <server>"))
		return 2
	}
	server := args[0]

	cfg, warnings, err := mcp.LoadConfig()
	for _, w := range warnings {
		fmt.Fprintln(errOut, "  "+styles.Warn.Render("warning: ")+styles.Muted.Render(w))
	}
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("mcp: ")+styles.Muted.Render(err.Error()))
		return 1
	}
	s, ok := cfg.Servers[server]
	if !ok {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("no enabled server named ")+styles.Base.Render(displaySafeArg(server)))
		fmt.Fprintln(errOut, "  "+styles.Muted.Render("configured: ")+styles.Base.Render(joinOr(cfg.ServerNames(), "(none)")))
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	transport, err := mcp.StartTransport(s, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("cannot start: ")+styles.Muted.Render(err.Error()))
		return 1
	}
	client, err := mcp.Connect(ctx, server, transport, s.Limit())
	if err != nil {
		_ = transport.Close()
		fmt.Fprintln(errOut, "  "+styles.Err.Render("cannot connect: ")+styles.Muted.Render(err.Error()))
		return 1
	}
	defer func() { _ = client.Close() }()

	advertised, err := client.ListTools(ctx)
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("tools/list failed: ")+styles.Muted.Render(err.Error()))
		return 1
	}

	var b strings.Builder
	if info := client.Info(); info.Name != "" {
		// Label kept inside FieldRow's 13-column gutter, or it runs into the value.
		b.WriteString(styles.FieldRow("reports as", styles.Base.Render(clip(oneLine(info.Name+" "+info.Version), 44)), 58) + "\n")
		b.WriteString("  " + styles.Subtle.Render("its own name; tools use the local one") + "\n\n")
	}
	for _, t := range advertised {
		mark := styles.Subtle.Render("·")
		note := styles.Subtle.Render("not in allowTools")
		if s.Allowed(t.Name) {
			mark = styles.OK.Render("✓")
			note = styles.Muted.Render("callable as " + tools.MCPName(server, t.Name))
		}
		b.WriteString("  " + mark + " " + styles.Base.Render(oneLine(t.Name)) + "\n")
		b.WriteString("    " + note + "\n")
		if d := firstLine(t.Description); d != "" {
			// One line, truncated: this is a third-party string, and the terminal is
			// not the place to render a page of it.
			b.WriteString("    " + styles.Subtle.Render(clip(d, 96)) + "\n")
		}
	}
	if len(advertised) == 0 {
		b.WriteString("  " + styles.Muted.Render("the server advertises no tools") + "\n")
	}
	fmt.Fprintln(out, styles.Indent(styles.Frame("Tools on "+oneLine(server), b.String(), 74), "  "))

	if diag := client.Diagnostics(); diag != "" {
		fmt.Fprintln(errOut, "  "+styles.Subtle.Render("server log: ")+styles.Muted.Render(oneLine(diag)))
	}
	return 0
}

// mcpCall invokes one tool directly.
//
// The allow-list is enforced here too. It would be easy to argue that a human
// typing the command is the approval, but the allow-list is also what the user
// declared this server may do, and a command that bypasses it means the file no
// longer describes the reachable surface.
func mcpCall(out, errOut io.Writer, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(errOut, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render(`qeuro mcp call <server> <tool> '{"arg":"value"}'`))
		return 2
	}
	server, tool := args[0], args[1]
	payload := "{}"
	if len(args) > 2 {
		payload = args[2]
	}
	if !json.Valid([]byte(payload)) {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("arguments are not valid JSON"))
		return 2
	}

	cfg, warnings, err := mcp.LoadConfig()
	for _, w := range warnings {
		fmt.Fprintln(errOut, "  "+styles.Warn.Render("warning: ")+styles.Muted.Render(w))
	}
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("mcp: ")+styles.Muted.Render(err.Error()))
		return 1
	}
	s, ok := cfg.Servers[server]
	if !ok {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("no enabled server named ")+styles.Base.Render(displaySafeArg(server)))
		return 1
	}
	if !s.Allowed(tool) {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("not in allowTools: ")+styles.Base.Render(displaySafeArg(tool)))
		fmt.Fprintln(errOut, "  "+styles.Muted.Render("allowed: ")+styles.Base.Render(joinOr(s.AllowTools, "(none)")))
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	transport, err := mcp.StartTransport(s, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("cannot start: ")+styles.Muted.Render(err.Error()))
		return 1
	}
	client, err := mcp.Connect(ctx, server, transport, s.Limit())
	if err != nil {
		_ = transport.Close()
		fmt.Fprintln(errOut, "  "+styles.Err.Render("cannot connect: ")+styles.Muted.Render(err.Error()))
		return 1
	}
	defer func() { _ = client.Close() }()

	res, err := client.CallTool(ctx, tool, json.RawMessage(payload))
	if err != nil {
		fmt.Fprintln(errOut, "  "+styles.Err.Render("call failed: ")+styles.Muted.Render(err.Error()))
		return 1
	}

	// The result is printed inside the same fence the model would receive. The
	// point is not to protect the terminal — it is that what a human inspects and
	// what the model is handed should be the same artefact, so a payload that tries
	// to pass itself off as an instruction is visible here.
	text, truncated := res.Text()
	fmt.Fprintln(out, tools.FenceUntrusted(tools.MCPSource(server), tools.TrustTierMCP, text))
	if truncated {
		fmt.Fprintln(errOut, "  "+styles.Warn.Render("note: ")+styles.Muted.Render("the result was truncated by the client"))
	}
	if res.IsError {
		// An execution error is a result, not a protocol failure — but a shell caller
		// needs a non-zero status to notice it.
		fmt.Fprintln(errOut, "  "+styles.Warn.Render("the tool reported an execution error"))
		return 1
	}
	return 0
}

// ---- small helpers -------------------------------------------------------

// commandLine renders what will be contacted: an argv vector for a stdio server,
// the endpoint for an HTTP one. An HTTP entry has no Command, so without this the
// row would be blank — which reads as a broken config rather than a working
// server of the other kind.
func commandLine(s mcp.ServerConfig) string {
	if s.IsHTTP() {
		return clip(oneLine(s.URL), 96)
	}
	parts := append([]string{s.Command}, s.Args...)
	return clip(oneLine(strings.Join(parts, " ")), 96)
}

// transportLabel names the transport for the list output. The distinction is
// worth a line of its own: a stdio server runs on this machine and an HTTP one
// sends the prompt's tool arguments over the network.
func transportLabel(s mcp.ServerConfig) string {
	if !s.IsHTTP() {
		return "stdio (local process)"
	}
	if s.AuthFrom != "" {
		return "streamable http, bearer from " + s.AuthFrom
	}
	return "streamable http, no token"
}

func limitText(limit int) string {
	if limit <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d calls/minute", limit)
}

func joinOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	return strings.Join(items, ", ")
}

// sortedKeys gives map iteration a stable order. Go randomises it, and both
// callers put the result somewhere a human reads: an env listing that reshuffles
// between runs, or a generated completion script that produces a different diff
// on every regeneration.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// oneLine strips control characters from third-party text before it is printed.
// Server names, tool names and descriptions all reach the terminal through here.
func oneLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// displaySafeArg sanitises a value that came from the command line. It is the
// user's own input, but it is echoed into an error message and argv is not
// filtered by the config reader.
func displaySafeArg(s string) string { return clip(oneLine(s), 64) }
