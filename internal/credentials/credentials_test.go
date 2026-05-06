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
