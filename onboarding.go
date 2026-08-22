package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"qeuro/internal/clientcfg"
	"qeuro/internal/styles"
)

// probeTimeout bounds one console probe. It is short because the probe is a
// convenience — guessing which dev port the console is on — and a convenience
// that blocks the user is not one.
const probeTimeout = 500 * time.Millisecond

// onboardingDial is the seam the startup tests replace in order to count
// connection attempts.
//
// It sits at the dialer rather than at discoverConsoleURL on purpose: the property
// roadmap §8 asks for is «без сетевых вызовов на старте», and a test that stubbed
// the function would only prove that *this* function was not called. Counting
// dials means any future probe on the startup path fails the test however it was
// written.
var onboardingDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: probeTimeout}).DialContext(ctx, network, addr)
}

// openBrowserFn is the seam for the browser launch, so tests can assert that
// registration was opened without a window appearing on the machine running them.
var openBrowserFn = openBrowser

type onboardingChoice uint8

const (
	onboardingNone onboardingChoice = iota
	onboardingRegister
	onboardingProvider
)

// maybePromptFirstRun stops a signed-out interactive launch before the TUI and
// asks which usable inference path the user wants: a Qeuro account or a BYOK
// provider. It deliberately ignores the legacy OnboardingOpened bit. That bit
// meant a dismissed browser tab silenced setup forever; the actual invariant is
// that every signed-out launch offers a way to become usable.
//
// Reading config and rendering the choice make no network call. Opening a URL is
// an explicit consequence of selecting an action, never a startup probe. Local
// mode is already a connected provider path and therefore skips this cloud setup.
func maybePromptFirstRun(in io.Reader, out io.Writer) {
	cfg, err := clientcfg.Load()
	if err != nil || cfg.LoggedIn() || cfg.Local {
		return
	}

	body := styles.Chip("SETUP", styles.Sky) + " " + styles.Muted.Render("choose how Qeuro should run") + "\n\n" +
		styles.UserTag.Render("1") + "  " + styles.Base.Render("Create a Qeuro account") + "\n" +
		styles.Subtle.Render("   managed models, credits and cloud features") + "\n\n" +
		styles.UserTag.Render("2") + "  " + styles.Base.Render("Connect an AI provider (BYOK)") + "\n" +
		styles.Subtle.Render("   use your own provider key through the console")
	fmt.Fprintln(out, styles.Indent(styles.Frame("Welcome to Qeuro", body, 64), "  "))

	choice, ok := readOnboardingChoice(in, out)
	if !ok {
		fmt.Fprintln(out, "  "+styles.Muted.Render("registration: ")+styles.Base.Render(signupURL(cfg, false)))
		fmt.Fprintln(out, "  "+styles.Muted.Render("provider setup: ")+styles.Base.Render(providerSetupURL(cfg)))
		return
	}
	if choice == onboardingProvider {
		openProviderSetup(cfg, out)
		return
	}
	openSignupTo(cfg, false, out)
}

// readOnboardingChoice owns only terminal parsing. Keeping it separate from the
// browser action makes EOF/non-interactive launches deterministic and makes an
// invalid answer re-prompt instead of accidentally choosing an account path.
func readOnboardingChoice(in io.Reader, out io.Writer) (onboardingChoice, bool) {
	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "  "+styles.Base.Render("Choose 1 or 2: "))
		raw, err := reader.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(raw))
		switch answer {
		case "1", "register", "signup", "account":
			return onboardingRegister, true
		case "2", "provider", "byok":
			return onboardingProvider, true
		}
		if err != nil {
			fmt.Fprintln(out)
			return onboardingNone, false
		}
		fmt.Fprintln(out, "  "+styles.Warn.Render("enter 1 to register or 2 to connect a provider"))
	}
}

// openSignup points the user's browser at registration.
//
// manual distinguishes the two callers, and it decides more than the wording. On
// the manual path (`qeuro login` with no token) the console URL is probed: that
// command is an explicit request which already blocks on launching a browser, so
// the probe is inside latency the user asked for, and it is where guessing a dev
// port actually helps — someone running the console locally is the person who
// types it. On the first-run path there is no probe, because that one is in front
// of the prompt.
func openSignup(cfg clientcfg.Config, manual bool) {
	openSignupTo(cfg, manual, os.Stdout)
}

func openSignupTo(cfg clientcfg.Config, manual bool, out io.Writer) {
	registrationURL := signupURL(cfg, manual)
	if err := openBrowserFn(registrationURL); err != nil {
		fmt.Fprintln(out, "  "+styles.Warn.Render("could not open browser: ")+styles.Muted.Render(err.Error()))
		fmt.Fprintln(out, "  "+styles.Muted.Render("open this link: ")+styles.Base.Render(registrationURL))
	} else {
		fmt.Fprintln(out, "  "+styles.OK.Render("opened registration: ")+styles.Base.Render(registrationURL))
	}
	fmt.Fprintln(out, "  "+styles.Muted.Render("after registration, copy the CLI token and return here:"))
	fmt.Fprintln(out, "  "+styles.UserTag.Render("qeuro login <CLI_TOKEN>"))
	if !manual {
		fmt.Fprintln(out, "  "+styles.Muted.Render("the CLI will stay offline until the token is saved."))
	}
}

func signupURL(cfg clientcfg.Config, manual bool) string {
	consoleURL := configuredConsoleURL(cfg)
	if manual {
		consoleURL = discoverConsoleURL(consoleURL)
	}
	return actionURL(consoleURL, "/register", url.Values{"from": {"cli"}})
}

func providerSetupURL(cfg clientcfg.Config) string {
	return actionURL(configuredConsoleURL(cfg), "/providers", url.Values{"from": {"cli"}})
}

func configuredConsoleURL(cfg clientcfg.Config) string {
	consoleURL := strings.TrimRight(strings.TrimSpace(cfg.ConsoleURL), "/")
	if consoleURL == "" {
		return strings.TrimRight(clientcfg.DefaultConsoleURL, "/")
	}
	return consoleURL
}

func actionURL(consoleURL, page string, query url.Values) string {
	return strings.TrimRight(consoleURL, "/") + page + "?" + query.Encode()
}

func openProviderSetup(cfg clientcfg.Config, out io.Writer) {
	setupURL := providerSetupURL(cfg)
	if err := openBrowserFn(setupURL); err != nil {
		fmt.Fprintln(out, "  "+styles.Warn.Render("could not open browser: ")+styles.Muted.Render(err.Error()))
		fmt.Fprintln(out, "  "+styles.Muted.Render("open this link: ")+styles.Base.Render(setupURL))
	} else {
		fmt.Fprintln(out, "  "+styles.OK.Render("opened provider setup: ")+styles.Base.Render(setupURL))
	}
	fmt.Fprintln(out, "  "+styles.Muted.Render("sign in or create the account requested by the console, then add your provider key."))
	fmt.Fprintln(out, "  "+styles.Muted.Render("afterwards copy the CLI token and run: ")+styles.UserTag.Render("qeuro login <CLI_TOKEN>"))
}

// discoverConsoleURL finds a reachable console among the configured origin and
// the two common dev ports. Only `qeuro login` calls it — see openSignup.
func discoverConsoleURL(preferred string) string {
	preferred = strings.TrimRight(preferred, "/")
	candidates := []string{preferred}
	for _, u := range []string{"http://localhost:3000", "http://localhost:3100"} {
		if u != "" && u != preferred {
			candidates = append(candidates, u)
		}
	}

	client := http.Client{
		Timeout: probeTimeout,
		// The dialer goes through the seam so the startup test can prove no
		// connection is attempted; the per-request Timeout still bounds the whole
		// exchange, not just the dial.
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           onboardingDial,
			TLSHandshakeTimeout:   probeTimeout,
			ResponseHeaderTimeout: probeTimeout,
		},
	}
	for _, u := range candidates {
		if u == "" {
			continue
		}
		resp, err := client.Get(u + "/register?from=cli")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < http.StatusInternalServerError {
				return u
			}
		}
	}
	if preferred != "" {
		return preferred
	}
	return clientcfg.DefaultConsoleURL
}

// openBrowser opens the console URL in the user's default browser.
//
// #nosec G204 -- the command is a per-OS literal and the URL is passed as a
// separate argv element, so it cannot be reinterpreted as shell syntax. The URL
// comes from the configured console origin or the compiled-in default.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
