// Package credentials reads the OAuth bearer token written by Claude Code at
// ~/.claude/.credentials.json. Read-only by design: cctrack never writes the
// file, never invokes the refresh-token flow, and never spawns
// `claude --init-only`. Refresh is Claude Code's responsibility; cctrack
// degrades to manual-sync UX when the token is expired or missing.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrCredentialsMissing means ~/.claude/.credentials.json was not found.
	ErrCredentialsMissing = errors.New("credentials: file not found")
	// ErrCredentialsMalformed means the file existed but JSON parse failed
	// or required fields were absent. The cause is intentionally elided so
	// caller-visible error strings never include credential file contents.
	ErrCredentialsMalformed = errors.New("credentials: malformed or missing required fields")
	// ErrTokenExpired means the file parsed cleanly but the OAuth token's
	// expiresAt is at or before the validation clock. The Credentials value
	// is still populated so callers can render staleness UX without a re-read.
	ErrTokenExpired = errors.New("credentials: oauth token expired")
)

// Credentials is the read-only subset of ~/.claude/.credentials.json that
// cctrack consumes. Any fields in the file beyond AccessToken and ExpiresAt
// are intentionally not parsed, not stored, and not surfaced.
type Credentials struct {
	AccessToken string
	ExpiresAt   time.Time
}

// Path returns the canonical Claude Code credentials path. Resolved against
// the current user's home directory; an empty string is returned only if
// home resolution fails (extremely unusual; surfaced as ErrCredentialsMissing
// downstream when ReadFile fails on the empty path).
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

// Load reads the OAuth credentials at the canonical path and validates expiry
// against the current wall clock. See loadFrom for the test seam.
func Load() (Credentials, error) {
	return loadFrom(Path(), time.Now())
}

func loadFrom(path string, now time.Time) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, ErrCredentialsMissing
		}
		// os.ReadFile errors include the path but never the file body,
		// so wrapping is safe. We still avoid wrapping ErrCredentialsMalformed
		// here because read failures are a different class than parse failures.
		return Credentials{}, fmt.Errorf("credentials: read: %w", err)
	}

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
