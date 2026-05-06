// Package credentials reads the OAuth bearer token issued by Claude Code.
//
// Claude Code stores credentials in two layouts:
//
//  1. The macOS login keychain under generic-password service
//     "Claude Code-credentials". Recent Claude Code versions on darwin
//     migrate credentials here and delete the legacy file. Authoritative
//     when present.
//  2. The legacy file at ~/.claude/.credentials.json. Used on all
//     platforms; on darwin this is the fallback when the keychain item
//     is not found.
//
// On darwin, Load consults the keychain first. Only a Keychain
// "item-not-found" (security exit 44, mapped to os.ErrNotExist) falls
// back to the legacy file. Any other Keychain failure
// (malformed/expired/denied/timeout/command-fails) returns the typed
// error directly without consulting the file — a Keychain that is
// authoritatively present but broken is a real problem to surface, not
// to paper over with potentially stale file data. On non-darwin, only
// the legacy file path is used.
//
// Read-only by design: cctrack never writes credentials, never invokes the
// refresh-token flow, and never spawns `claude --init-only`. Refresh is
// Claude Code's responsibility; cctrack degrades to manual-sync UX when the
// token is expired or missing in every source.
package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"time"
)

var (
	// ErrCredentialsMissing means no credentials were found in any source
	// (legacy file plus, on darwin, the keychain).
	ErrCredentialsMissing = errors.New("credentials: not found")
	// ErrCredentialsMalformed means a source returned bytes but JSON parse
	// failed or required fields were absent. The cause is intentionally
	// elided so caller-visible error strings never include credential bytes.
	ErrCredentialsMalformed = errors.New("credentials: malformed or missing required fields")
	// ErrTokenExpired means the credentials parsed cleanly but expiresAt is
	// at or before the validation clock. The Credentials value is still
	// populated so callers can render staleness UX without a re-read.
	ErrTokenExpired = errors.New("credentials: oauth token expired")
)

// keychainServiceName is the generic-password "service" attribute Claude
// Code writes when migrating credentials off the legacy file path on macOS.
// Stable identifier owned by Claude Code; not a secret.
const keychainServiceName = "Claude Code-credentials"

// Credentials is the read-only subset of the credentials payload that
// cctrack consumes. Fields beyond AccessToken and ExpiresAt are
// intentionally not parsed, not stored, and not surfaced.
type Credentials struct {
	AccessToken string
	ExpiresAt   time.Time
}

// Path returns the canonical legacy Claude Code credentials path. On macOS
// this file is typically absent on machines that have logged in with a
// recent Claude Code build (credentials live in the keychain instead).
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

// keychainReader is the resolved keychain accessor for the current
// platform. On darwin it shells out to `security`; elsewhere it is nil and
// the keychain step is skipped. Package-scope for the test seam.
var keychainReader = defaultKeychainReader()

func defaultKeychainReader() func() ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return securityKeychainRead
}

// Load reads OAuth credentials from the first available source and
// validates expiry against time.Now(). On darwin, the keychain takes
// precedence; only a Keychain item-not-found falls back to the legacy
// file. On non-darwin, keychainReader is nil and Load reads the legacy
// file directly.
func Load() (Credentials, error) {
	return loadComposed(keychainReader, fileBytesReader(Path()), time.Now())
}

// fileBytesReader returns a reader that does a single os.ReadFile of path.
// Bubbles os.ErrNotExist through unchanged so the cascade can distinguish
// "missing" from real I/O errors.
func fileBytesReader(path string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return os.ReadFile(path)
	}
}

// loadComposed reads credential bytes from primary, falling back to
// fallback only when primary signals os.ErrNotExist. Any other primary
// error short-circuits — an authoritative source that is present but
// broken is a real problem we surface, not paper over by consulting the
// fallback.
func loadComposed(primary, fallback func() ([]byte, error), now time.Time) (Credentials, error) {
	data, err := readCascade(primary, fallback)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, ErrCredentialsMissing
		}
		// Underlying errors from os.ReadFile and the security CLI carry
		// only paths/exit info, never credential bytes — so wrapping is
		// safe under the no-leak invariant.
		return Credentials{}, fmt.Errorf("credentials: read: %w", err)
	}
	return parseAndValidate(data, now)
}

func readCascade(primary, fallback func() ([]byte, error)) ([]byte, error) {
	if primary != nil {
		data, err := primary()
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if fallback == nil {
		return nil, os.ErrNotExist
	}
	return fallback()
}

// loadFrom is the file-only entry point retained for the existing test
// surface. Production callers go through Load (which composes file +
// keychain).
func loadFrom(path string, now time.Time) (Credentials, error) {
	return loadComposed(fileBytesReader(path), nil, now)
}

func parseAndValidate(data []byte, now time.Time) (Credentials, error) {
	var raw struct {
		ClaudeAiOAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Credentials{}, ErrCredentialsMalformed
	}
	if raw.ClaudeAiOAuth.AccessToken == "" || raw.ClaudeAiOAuth.ExpiresAt == 0 {
		return Credentials{}, ErrCredentialsMalformed
	}
	expiresAt := time.UnixMilli(raw.ClaudeAiOAuth.ExpiresAt)
	creds := Credentials{
		AccessToken: raw.ClaudeAiOAuth.AccessToken,
		ExpiresAt:   expiresAt,
	}
	if !expiresAt.After(now) {
		return creds, ErrTokenExpired
	}
	return creds, nil
}

// keychainItemNotFoundExitCode is the status `security` returns when the
// requested item is absent (errSecItemNotFound surfaced as 44 by the CLI).
const keychainItemNotFoundExitCode = 44

// securityBinaryPath is the fixed absolute path to macOS's bundled
// `security` CLI. Using the fixed path avoids PATH-resolution hijack —
// a malicious binary named `security` earlier in $PATH would otherwise
// be invoked.
const securityBinaryPath = "/usr/bin/security"

// keychainReadTimeout caps a single keychain read. The real CLI returns
// in milliseconds; anything slower is pathological (locked keychain,
// blocked I/O) and we'd rather fail closed than block Load.
const keychainReadTimeout = 5 * time.Second

// maxKeychainOutputBytes bounds stdout. The credential JSON is ~1KB on
// observed installs; 64KB is generous headroom and a hard ceiling that
// prevents a malicious or buggy `security` from ballooning memory.
const maxKeychainOutputBytes = 64 * 1024

// securityKeychainRead invokes `/usr/bin/security find-generic-password`
// to fetch the Claude Code credential blob. Shelling out keeps the
// package pure-Go (no cgo / Security framework binding).
//
// Subprocess guardrails (binding per codex-2 chat msg 20559):
//   - fixed binary path (securityBinaryPath); no PATH lookup
//   - context-bound execution (keychainReadTimeout); guaranteed to
//     return within the timeout even if `security` hangs
//   - bounded stdout (maxKeychainOutputBytes); stdout is read through
//     an io.LimitReader so the buffer cannot exceed the cap
//
// Errors are wrapped with the `security:` prefix and never include the
// captured stdout bytes — `cmd.Output` captured only stdout (not stderr),
// so the credential payload cannot leak through error strings.
func securityKeychainRead() ([]byte, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("user lookup: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), keychainReadTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, securityBinaryPath, "find-generic-password",
		"-s", keychainServiceName,
		"-a", u.Username,
		"-w")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("security: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("security: start: %w", err)
	}

	// Read at most maxKeychainOutputBytes+1 so we can detect overflow without
	// allocating an unbounded buffer. The +1 lets us distinguish "exactly at
	// the cap" from "exceeded the cap."
	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxKeychainOutputBytes)+1))
	waitErr := cmd.Wait()

	if len(out) > maxKeychainOutputBytes {
		return nil, fmt.Errorf("security: output exceeded %d bytes", maxKeychainOutputBytes)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == keychainItemNotFoundExitCode {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("security: %w", waitErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("security: read: %w", readErr)
	}

	return bytes.TrimRight(out, "\r\n"), nil
}
