//go:build !windows

package clientcfg

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "qeuro-cli"
	keyringAccount = "default"
)

// keyringState tracks whether the OS keychain accepted the last operation, so
// Save knows whether config.json (0600, owner-only) must keep the token as a
// fallback and the UI can warn the user.
var keyringState struct {
	sync.Mutex
	unavailable bool
}

func setKeyringUnavailable(v bool) {
	keyringState.Lock()
	keyringState.unavailable = v
	keyringState.Unlock()
}

func keyringUnavailable() bool {
	keyringState.Lock()
	defer keyringState.Unlock()
	return keyringState.unavailable
}

// loadStoredToken reads the token from the OS keychain (Secret Service /
// D-Bus on Linux, Keychain on macOS) under service "qeuro-cli", account
// "default". A missing entry is not an error. When the keychain is
// unreachable (headless session without D-Bus, locked keychain), it flags
// unavailability so the owner-only config-file fallback stays active; the
// caller then uses the token from config.json if present.
func loadStoredToken() (string, error) {
	token, err := keyring.Get(keyringService, keyringAccount)
	if err == nil {
		setKeyringUnavailable(false)
		return token, nil
	}
	if err == keyring.ErrNotFound {
		setKeyringUnavailable(false)
		return "", nil
	}
	// Keychain unavailable — not fatal: fall back to config.json (0600).
	setKeyringUnavailable(true)
	return "", nil
}

// saveStoredToken writes (or deletes) the token in the OS keychain. Failures
// are not fatal: the token is then kept in config.json with 0600 permissions
// (see omitTokenFromConfig), matching the pre-keychain behavior.
func saveStoredToken(token string) error {
	if token == "" {
		err := keyring.Delete(keyringService, keyringAccount)
		if err != nil && err != keyring.ErrNotFound {
			setKeyringUnavailable(true)
		}
		clearSentinel()
		return nil
	}
	if err := keyring.Set(keyringService, keyringAccount, token); err != nil {
		setKeyringUnavailable(true)
		clearSentinel()
		return nil
	}
	setKeyringUnavailable(false)
	writeSentinel()
	return nil
}

// sentinelName marks that this CLI put a token in the OS keychain.
//
// It exists because the keychain APIs have no existence check (see
// storedTokenPresent): without it, every launch on Linux and macOS would either
// pay a D-Bus round trip to decide what the status line says, or say "offline"
// until something needed the token. The file is empty — it is a bit, not a
// secret, and a bit is exactly what LoggedIn needs.
const sentinelName = "token.keychain"

func sentinelPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, sentinelName), nil
}

func writeSentinel() {
	p, err := sentinelPath()
	if err != nil {
		return
	}
	// Best effort throughout: a missing sentinel costs a wrong first frame that
	// self-corrects, and failing `qeuro login` because a marker file could not be
	// written would be a much worse trade.
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(p, nil, 0o600)
}

func clearSentinel() {
	if p, err := sentinelPath(); err == nil {
		_ = os.Remove(p)
	}
}

func sentinelPresent() bool {
	p, err := sentinelPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// omitTokenFromConfig strips the token from config.json only when the OS
// keychain actually stored it; otherwise the owner-only file (0600) is the
// documented fallback for sessions without D-Bus/Keychain access.
func omitTokenFromConfig() bool {
	return !keyringUnavailable()
}

// storedTokenPresent reports whether the keychain holds a token, without paying
// for the value.
//
// It cannot do that. The Secret Service and macOS Keychain APIs have no
// existence check separate from a read — `keyring.Get` *is* the probe — so any
// answer here either performs the round trip roadmap §8 asks us to avoid, or
// guesses.
//
// So it guesses in the one direction that is safe, and returns false. The cost is
// bounded and visible: on a platform where the keychain holds the token and
// nothing else supplies one, the first frame says "offline session" for the few
// milliseconds until something calls Secret(). The alternative — reading the
// keychain to decide what to print — is the D-Bus wait on the startup path, which
// is the whole thing this row removes. A wrong first frame that self-corrects is
// better than a slow prompt, and it is why the sentinel file below exists: it
// makes the common case (a token this CLI stored itself) answerable by a stat.
func storedTokenPresent() bool {
	return sentinelPresent()
}

func tokenStorageWarning() string {
	if keyringUnavailable() {
		return "OS keychain is unavailable (no D-Bus/Keychain session); token stored in an owner-only config file instead"
	}
	return ""
}
