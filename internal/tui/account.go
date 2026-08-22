package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"qeuro/internal/client"
	"qeuro/internal/clientcfg"
)

// accountTimeout bounds the async /login, /logout and /providers requests.
const accountTimeout = 20 * time.Second

// loginDoneMsg reports an async /login attempt. On success the token has
// already been persisted via clientcfg.Save.
type loginDoneMsg struct {
	token string
	me    *client.MeResponse
	err   error
}

// logoutDoneMsg reports an async /logout. The local token is already cleared;
// revokeErr is the best-effort server-side revocation outcome.
type logoutDoneMsg struct {
	saveErr   error
	revokeErr error
}

// providersMsg carries the provider credentials linked on the web console.
type providersMsg struct {
	providers []client.ProviderConfig
	quiet     bool // startup refresh: do not open the panel or report errors
	err       error
}

// loginCmd verifies the pasted token against the backend (GET /v1/me),
// persists it with clientcfg.Save (OS keychain when available) and reports
// the result. Mirrors `qeuro login <token>` from the shell.
func loginCmd(baseURL, token string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
		defer cancel()
		c := client.New(baseURL, token)
		me, err := c.Me(ctx)
		if err != nil {
			return loginDoneMsg{err: err}
		}
		cfg, err := clientcfg.Load()
		if err != nil {
			return loginDoneMsg{err: err}
		}
		cfg.SetToken(token)
		if err := clientcfg.Save(cfg); err != nil {
			return loginDoneMsg{err: err}
		}
		return loginDoneMsg{token: token, me: me}
	}
}

// logoutCmd revokes the token server-side (best effort) and removes it from
// local storage. Mirrors `qeuro logout` from the shell.
func logoutCmd(cli *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
		defer cancel()
		var revokeErr error
		if cli != nil {
			revokeErr = cli.RevokeToken(ctx)
		}
		cfg, saveErr := clientcfg.Load()
		if saveErr == nil {
			cfg.SetToken("")
			saveErr = clientcfg.Save(cfg)
		}
		return logoutDoneMsg{saveErr: saveErr, revokeErr: revokeErr}
	}
}

// fetchProviders is the quiet startup variant of providersCmd: failures stay
// silent and the panel is not opened.
func fetchProviders(cli *client.Client, consoleURL string) tea.Cmd {
	return providersCmd(cli, consoleURL, true)
}

// providersCmd loads the provider list linked on the web console.
func providersCmd(cli *client.Client, consoleURL string, quiet bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
		defer cancel()
		list, err := cli.ConsoleProviders(ctx, consoleURL)
		return providersMsg{providers: list, quiet: quiet, err: err}
	}
}
