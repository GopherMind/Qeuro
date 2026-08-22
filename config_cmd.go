package main

import (
	"fmt"
	"os"
	"strings"

	"qeuro/internal/clientcfg"
	"qeuro/internal/styles"
)

// cmdConfig implements `qeuro config doctor` (roadmap §8, row "Конфиг").
//
// The row pairs layered precedence with this command for a reason: precedence a
// user cannot inspect is worse than no precedence, because a value that is being
// overridden looks identical to a value that was never read. Doctor prints every
// setting, the layer that won, and the exact file and line — so "why is it still
// talking to localhost" is one command, not a support thread.
func cmdConfig(args []string) {
	sub := "doctor"
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "doctor":
		cmdConfigDoctor()
	default:
		fmt.Fprintln(os.Stderr, "  "+styles.Err.Render("unknown subcommand: ")+styles.Base.Render(sub))
		fmt.Fprintln(os.Stderr, "  "+styles.Muted.Render("usage: ")+styles.UserTag.Render("qeuro config doctor"))
		os.Exit(2)
	}
}

func cmdConfigDoctor() {
	cfg, err := clientcfg.Load()

	var b strings.Builder
	b.WriteString(styles.Pill("PRECEDENCE", styles.Accent2) + " " +
		styles.Muted.Render("flag → env → ./"+clientcfg.ProjectFileName+" → user file → built-in") + "\n\n")

	// OriginsForDisplay, not cfg.Origins: doctor is the one caller that must force
	// the secret-store read Load skips, because a token row reading "(not set)"
	// for a signed-in user is exactly the confusion this command exists to remove.
	origins := cfg.OriginsForDisplay()

	for _, o := range origins {
		value := o.Value
		switch {
		case o.Secret && value != "":
			// Doctor output is pasted into issues. Presence and provenance are
			// what diagnose a problem; the value itself never is.
			value = value + styles.Subtle.Render(" (redacted)")
		case value == "":
			value = styles.Subtle.Render("(not set)")
		}
		b.WriteString(styles.FieldRow(o.Key, value, 58) + "\n")
		b.WriteString("  " + styles.Subtle.Render(o.Layer.String()) + "\n")
	}

	fmt.Println(styles.Indent(styles.Frame("Config", b.String(), 74), "  "))

	// Paths and their line numbers go outside the frame: Frame() truncates to a
	// fixed width, and a config path is routinely longer than that. Truncating
	// the one string the user needs in order to open the file would defeat the
	// command.
	fmt.Println()
	for _, o := range origins {
		if o.Layer == clientcfg.LayerUserFile || o.Layer == clientcfg.LayerProjectFile {
			fmt.Println("  " + styles.Subtle.Render(o.Key+" ← ") + styles.Base.Render(o.Source))
		}
	}
	fmt.Println("  " + styles.Subtle.Render("state file   ") + fileNote(clientcfg.FilePath()))
	fmt.Println("  " + styles.Subtle.Render("project file ") + fileNote(clientcfg.ProjectFilePath()))
	fmt.Println("  " + styles.Subtle.Render("user file    ") + fileNote(clientcfg.UserFilePath()))

	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "  "+styles.Warn.Render("warning: ")+styles.Muted.Render(w))
	}
	if warning := clientcfg.TokenStorageWarning(); warning != "" {
		fmt.Fprintln(os.Stderr, "  "+styles.Warn.Render("token storage: ")+styles.Muted.Render(warning))
	}
	if err != nil {
		// Exit non-zero on a broken config: doctor is what a script runs to
		// check whether the CLI is usable, and a green exit on an unreadable
		// config would make it useless for that.
		fmt.Fprintln(os.Stderr, "  "+styles.Err.Render("config error: ")+styles.Muted.Render(err.Error()))
		os.Exit(1)
	}
	if len(cfg.Warnings) > 0 {
		os.Exit(1)
	}
}

// fileNote reports whether a config file exists, since "absent" and "present but
// overridden" are the two cases a user confuses.
func fileNote(path string) string {
	if _, err := os.Stat(path); err == nil {
		return styles.Base.Render(path)
	}
	return styles.Muted.Render(path) + styles.Subtle.Render(" (absent)")
}
