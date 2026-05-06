package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCreds(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// fixedNow is the validation clock used by every test case so ExpiresAt
// values can be expressed relative to a stable reference.
var fixedNow = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

func TestLoadFrom(t *testing.T) {
	futureMs := fixedNow.Add(1 * time.Hour).UnixMilli()
	pastMs := fixedNow.Add(-1 * time.Hour).UnixMilli()
	tokenSentinel := "sk-ant-oat01-FAKE-FOR-TEST-DO-NOT-USE"

	cases := []struct {
		name        string
		body        string // empty => do not create file
		wantErr     error  // matched with errors.Is
		wantToken   string
		wantExpires time.Time
	}{
		{
			name:    "missing file",
			wantErr: ErrCredentialsMissing,
		},
		{
			name:    "malformed JSON",
			body:    "{not json",
			wantErr: ErrCredentialsMalformed,
		},
		{
			name:    "missing claudeAiOauth section",
			body:    `{"otherSection":{"foo":"bar"}}`,
			wantErr: ErrCredentialsMalformed,
		},
		{
			name:    "empty access token",
			body:    fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"","expiresAt":%d}}`, futureMs),
			wantErr: ErrCredentialsMalformed,
		},
		{
			name:    "zero expiresAt",
			body:    fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":0}}`, tokenSentinel),
			wantErr: ErrCredentialsMalformed,
		},
		{
			name:        "expired token returns sentinel + populated fields",
			body:        fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tokenSentinel, pastMs),
			wantErr:     ErrTokenExpired,
			wantToken:   tokenSentinel,
			wantExpires: time.UnixMilli(pastMs),
		},
		{
			name:        "valid token",
			body:        fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tokenSentinel, futureMs),
			wantToken:   tokenSentinel,
			wantExpires: time.UnixMilli(futureMs),
		},
		{
			// Confirms the parser ignores fields it does not consume. The F2
			// invariant is that cctrack reads ONLY accessToken + expiresAt; any
			// other keys present in the file must be silently dropped without
			// affecting the returned Credentials value.
			name: "extra unrelated fields are ignored",
			body: fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"alpha":"x","beta":42,"gamma":["a","b"],"delta":{"nested":true}}}`, tokenSentinel, futureMs),
			wantToken:   tokenSentinel,
			wantExpires: time.UnixMilli(futureMs),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".credentials.json")
			if c.body != "" {
				path = writeCreds(t, dir, c.body)
			}

			got, err := loadFrom(path, fixedNow)

			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want errors.Is(%v) = true", err, c.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.AccessToken != c.wantToken {
				t.Errorf("AccessToken = %q, want %q", got.AccessToken, c.wantToken)
			}
			if !c.wantExpires.IsZero() && !got.ExpiresAt.Equal(c.wantExpires) {
				t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, c.wantExpires)
			}

			// Hard bar: the typed error string must never leak the access token,
			// regardless of which failure path produced it. This is a paranoid
			// regression guard against future code adding %v of the parsed body.
			if err != nil && c.body != "" && strings.Contains(c.body, tokenSentinel) {
				if strings.Contains(err.Error(), tokenSentinel) {
					t.Errorf("error string leaked access token: %q", err.Error())
				}
			}
		})
	}
}

// TestLoadFrom_PathLeak confirms the read-error path also redacts the body.
// We do this by writing a file with the token then chmod-ing it 000 to force
// a non-NotExist read error; we verify the surfaced error has no token.
func TestLoadFrom_NonExistError_NoTokenLeak(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod-based denial cannot be exercised")
	}
	dir := t.TempDir()
	tokenSentinel := "sk-ant-oat01-FAKE-FOR-TEST-DO-NOT-USE"
	path := writeCreds(t, dir, fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":1}}`, tokenSentinel))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })

	_, err := loadFrom(path, fixedNow)
	if err == nil {
		t.Skip("file readable despite chmod 0; environment-dependent")
	}
	if strings.Contains(err.Error(), tokenSentinel) {
		t.Errorf("error string leaked token: %q", err.Error())
	}
}

// validBody returns a minimal credentials payload with the given token and
// expiresAt (in unix millis). Keeps cascade-test cases readable.
func validBody(token string, expiresMs int64) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, token, expiresMs))
}

// TestLoadComposed exercises the keychain→file cascade independent of the
// real filesystem and macOS Security CLI by injecting fake byte readers.
// Verifies the binding source order from EM msg 20496 + codex-2 msg 20510:
// on darwin, keychain takes precedence; only "item-not-found" falls back
// to the file. Other keychain failure modes (malformed/expired/denied/
// timeout/command-fail) short-circuit without consulting the file.
func TestLoadComposed(t *testing.T) {
	futureMs := fixedNow.Add(1 * time.Hour).UnixMilli()
	pastMs := fixedNow.Add(-1 * time.Hour).UnixMilli()
	tokenSentinel := "sk-ant-oat01-FAKE-FOR-TEST-DO-NOT-USE"

	notExist := func() ([]byte, error) { return nil, os.ErrNotExist }
	emit := func(b []byte) func() ([]byte, error) {
		return func() ([]byte, error) { return b, nil }
	}
	fail := func(err error) func() ([]byte, error) {
		return func() ([]byte, error) { return nil, err }
	}

	// Helper aliases: in production primary == keychain, fallback == file.
	// Test data uses descriptive labels so the cascade direction is obvious.
	cases := []struct {
		name        string
		primary     func() ([]byte, error) // keychain in production
		fallback    func() ([]byte, error) // file in production
		wantErr     error                  // matched with errors.Is when non-nil
		wantWrap    string
		wantToken   string
		wantExpires time.Time
	}{
		// --- Source-order precedence and fallback (codex-2 msg 20510 bar) ---
		{
			// Both sources valid → keychain wins.
			name:        "keychain valid + file valid -> keychain wins",
			primary:     emit(validBody("keychain-token", futureMs)),
			fallback:    emit(validBody("file-token", futureMs)),
			wantToken:   "keychain-token",
			wantExpires: time.UnixMilli(futureMs),
		},
		{
			// Keychain item-not-found is the ONLY condition that falls back to file.
			name:        "keychain not found + file valid -> file wins",
			primary:     notExist,
			fallback:    emit(validBody(tokenSentinel, futureMs)),
			wantToken:   tokenSentinel,
			wantExpires: time.UnixMilli(futureMs),
		},
		{
			name:     "both sources missing -> ErrCredentialsMissing",
			primary:  notExist,
			fallback: notExist,
			wantErr:  ErrCredentialsMissing,
		},
		{
			// Non-darwin: keychainReader is nil; cascade falls through to file.
			name:        "nil keychain (non-darwin) uses file directly",
			primary:     nil,
			fallback:    emit(validBody("file-token", futureMs)),
			wantToken:   "file-token",
			wantExpires: time.UnixMilli(futureMs),
		},
		{
			name:     "nil keychain and nil file -> ErrCredentialsMissing",
			primary:  nil,
			fallback: nil,
			wantErr:  ErrCredentialsMissing,
		},

		// --- Keychain found-but-bad MUST NOT silently fall back to file ---
		// codex-2 msg 20510: "Keychain malformed/expired/denied/timeout/command
		// failure + file valid -> returns the Keychain error, no silent fallback."
		{
			// "denied / timeout / command-fail" surface as non-NotExist errors
			// from securityKeychainRead; cascade short-circuits without consulting file.
			name:     "keychain non-NotExist error short-circuits (file NOT consulted)",
			primary:  fail(errors.New("simulated keychain failure")),
			fallback: emit(validBody("would-be-masked", futureMs)),
			wantWrap: "credentials: read:",
		},
		{
			// Keychain returns bytes that don't parse → ErrCredentialsMalformed
			// from parseAndValidate; file never consulted because cascade returned
			// keychain's bytes successfully.
			name:     "keychain malformed + file valid -> ErrCredentialsMalformed (no fallback)",
			primary:  emit([]byte("{not json")),
			fallback: emit(validBody("would-be-masked", futureMs)),
			wantErr:  ErrCredentialsMalformed,
		},
		{
			// Keychain returns valid JSON but expired → ErrTokenExpired with
			// populated fields; file never consulted.
			name:        "keychain expired + file valid -> ErrTokenExpired (no fallback)",
			primary:     emit(validBody(tokenSentinel, pastMs)),
			fallback:    emit(validBody("would-be-masked", futureMs)),
			wantErr:     ErrTokenExpired,
			wantToken:   tokenSentinel,
			wantExpires: time.UnixMilli(pastMs),
		},

		// --- File-side errors when keychain is genuinely not present ---
		{
			name:     "keychain not found + file malformed -> ErrCredentialsMalformed",
			primary:  notExist,
			fallback: emit([]byte("{not json")),
			wantErr:  ErrCredentialsMalformed,
		},
		{
			name:        "keychain not found + file expired -> ErrTokenExpired",
			primary:     notExist,
			fallback:    emit(validBody(tokenSentinel, pastMs)),
			wantErr:     ErrTokenExpired,
			wantToken:   tokenSentinel,
			wantExpires: time.UnixMilli(pastMs),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := loadComposed(c.primary, c.fallback, fixedNow)

			switch {
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want errors.Is(%v) = true", err, c.wantErr)
				}
			case c.wantWrap != "":
				if err == nil || !strings.Contains(err.Error(), c.wantWrap) {
					t.Fatalf("err = %v, want substring %q", err, c.wantWrap)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
			}
			if got.AccessToken != c.wantToken {
				t.Errorf("AccessToken = %q, want %q", got.AccessToken, c.wantToken)
			}
			if !c.wantExpires.IsZero() && !got.ExpiresAt.Equal(c.wantExpires) {
				t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, c.wantExpires)
			}

			// No-leak invariant applies to every keychain-sourced byte too:
			// if the fallback emitted the sentinel and a non-nil error
			// surfaced, the error string must not echo the token.
			if err != nil && c.fallback != nil {
				if strings.Contains(err.Error(), tokenSentinel) {
					t.Errorf("error string leaked token: %q", err.Error())
				}
			}
		})
	}
}
