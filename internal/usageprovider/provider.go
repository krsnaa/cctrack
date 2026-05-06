// Package usageprovider queries Anthropic's private OAuth usage endpoint
// (/api/oauth/usage) using the bearer token from the credentials package.
//
// Endpoint contract is undocumented and explicitly accepted as risk by the
// project owner (cctrack is an internal utility). The adapter is fail-soft:
// any 4xx/5xx, network failure, or schema drift returns a typed sentinel
// error so the dashboard can render a "provider unavailable" state and
// fall back to manual sync without partial trust.
//
// Field allowlist (binding per F2 S2.1 ruling):
//   - five_hour.utilization
//   - seven_day.utilization
// Any other response fields are ignored without enumeration: not parsed,
// not surfaced, and not logged. Unknown extras are not treated as drift.
// SCHEMA.md is the authoritative committed allowlist.
package usageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/ksred/cctrack/internal/credentials"
)

// rfc3339Pattern is a strict shape gate for resets_at values. Go's
// time.Parse with the RFC3339Nano layout falls through to a generic
// layout parser that accepts widenings (e.g. comma fractional
// separator) which RFC3339 forbids. The regex pre-check rejects those
// inputs before time.Parse can be lenient. Per F2 S2.2 EM ruling msg
// 20565 binding constraint #3 + verifier finding chat msg 20579.
//
//	YYYY-MM-DDTHH:MM:SS[.fractional]Z|±HH:MM
var rfc3339Pattern = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`,
)

const (
	// DefaultEndpoint is the private Claude OAuth usage URL.
	DefaultEndpoint = "https://api.anthropic.com/api/oauth/usage"
	// BetaHeader is the value for the `anthropic-beta` request header
	// gating access to the OAuth usage endpoint.
	BetaHeader = "oauth-2025-04-20"
	// DefaultTimeout caps a single fetch.
	DefaultTimeout = 10 * time.Second
)

var (
	// ErrProviderUnavailable means the endpoint could not be reached or
	// returned an unrecognized error class. Caller should fall back to manual
	// sync UX. Never carries response body or token in the error string.
	ErrProviderUnavailable = errors.New("usageprovider: provider unavailable")
	// ErrSchemaDrift means a 200 response was received but required fields
	// are missing or unparseable. Caller must NOT trust partial data; this
	// fails closed per F2 S2.1 bar.
	ErrSchemaDrift = errors.New("usageprovider: schema drift")
	// ErrUnauthorized means the token was rejected (401/403). Caller should
	// surface "open Claude Code to refresh" state.
	ErrUnauthorized = errors.New("usageprovider: unauthorized")
	// ErrRateLimited means the endpoint returned 429. Caller should back off.
	ErrRateLimited = errors.New("usageprovider: rate limited")
)

// Snapshot is the allowlisted subset of /api/oauth/usage that cctrack consumes.
// Schema discovery (T2.1.3) confirmed both `utilization` numbers and
// `resets_at` strings are exposed; S2.2 extended this struct with the two
// reset times so the auto-sync scheduler can write SaveWindowAnchor rows
// with correct TimeLeftMinutes derivation.
//
// resets_at strings are parsed as RFC3339 / RFC3339Nano per F2 S2.2 EM
// ruling (chat msg 20565 binding constraint #3). Other formats fail
// closed with ErrSchemaDrift.
type Snapshot struct {
	FiveHourUtilizationPercent int       // five_hour.utilization
	SevenDayUtilizationPercent int       // seven_day.utilization
	FiveHourResetsAt           time.Time // five_hour.resets_at (parsed)
	SevenDayResetsAt           time.Time // seven_day.resets_at (parsed)
	Observed                   time.Time // wall clock at fetch completion
}

// Client is a single-flight HTTP client for the OAuth usage endpoint. A single
// process-wide mutex serializes Fetch calls; this is sufficient for cctrack's
// single-scheduler use case and avoids pulling in golang.org/x/sync for one
// call site. Callers waiting on an in-flight fetch will block until it
// completes (or the context is canceled).
type Client struct {
	httpc    *http.Client
	endpoint string
	now      func() time.Time

	mu sync.Mutex
}

// New returns a Client that talks to DefaultEndpoint with DefaultTimeout and
// no automatic redirect following.
func New() *Client {
	return &Client{
		httpc: &http.Client{
			Timeout: DefaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		endpoint: DefaultEndpoint,
		now:      time.Now,
	}
}

// Fetch performs a single GET against the configured endpoint with the bearer
// token. The token never appears in returned errors. Any 4xx/5xx response is
// surfaced as a typed sentinel; the response body is read for status code
// only and never retained.
func (c *Client) Fetch(ctx context.Context, creds credentials.Credentials) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if creds.AccessToken == "" {
		return Snapshot{}, fmt.Errorf("%w: empty access token", ErrUnauthorized)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		// Build failures wrap the cause but neither the cause nor the URL
		// can carry the token; safe to surface.
		return Snapshot{}, fmt.Errorf("%w: build request: %v", ErrProviderUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("anthropic-beta", BetaHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		// net/http transport errors include the URL but not headers/body,
		// so the token cannot leak here.
		return Snapshot{}, fmt.Errorf("%w: transport: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// Continue to parse below.
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return Snapshot{}, fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return Snapshot{}, fmt.Errorf("%w: status %d", ErrRateLimited, resp.StatusCode)
	default:
		return Snapshot{}, fmt.Errorf("%w: status %d", ErrProviderUnavailable, resp.StatusCode)
	}

	// Decode only the allowlisted fields. Unknown extra fields are ignored.
	// Pointer types distinguish "field absent" from "field present and zero" /
	// "field present and empty string."
	var raw struct {
		FiveHour struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode", ErrSchemaDrift)
	}
	if raw.FiveHour.Utilization == nil || raw.SevenDay.Utilization == nil {
		return Snapshot{}, fmt.Errorf("%w: missing utilization fields", ErrSchemaDrift)
	}
	if raw.FiveHour.ResetsAt == nil || raw.SevenDay.ResetsAt == nil {
		return Snapshot{}, fmt.Errorf("%w: missing resets_at fields", ErrSchemaDrift)
	}

	fiveHourResetsAt, err := parseResetsAt(*raw.FiveHour.ResetsAt)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: five_hour.resets_at parse", ErrSchemaDrift)
	}
	sevenDayResetsAt, err := parseResetsAt(*raw.SevenDay.ResetsAt)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: seven_day.resets_at parse", ErrSchemaDrift)
	}

	return Snapshot{
		FiveHourUtilizationPercent: int(*raw.FiveHour.Utilization),
		SevenDayUtilizationPercent: int(*raw.SevenDay.Utilization),
		FiveHourResetsAt:           fiveHourResetsAt,
		SevenDayResetsAt:           sevenDayResetsAt,
		Observed:                   c.now(),
	}, nil
}

// parseResetsAt accepts only RFC3339 / RFC3339Nano timestamps. Any other
// format (including Go's other time layouts, Unix epoch, comma fractional
// separators, or natural-language dates) is rejected. Per F2 S2.2 EM
// ruling msg 20565 binding constraint #3 + verifier finding chat msg
// 20579, the parser MUST fail closed on unrecognized formats; widening
// to print observed value formats is forbidden.
//
// The shape pre-check via rfc3339Pattern is load-bearing: time.Parse(
// RFC3339Nano) alone would accept widenings like "2026-05-06T17:00:00,123Z"
// because Go's generic layout fallback honors comma fractional separators
// even though RFC3339 secfrac requires '.'.
func parseResetsAt(s string) (time.Time, error) {
	if !rfc3339Pattern.MatchString(s) {
		return time.Time{}, errors.New("not RFC3339")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Note: error message intentionally does NOT include the input string,
		// since "raw response body in error" leaks beyond what callers can audit.
		return time.Time{}, errors.New("not RFC3339")
	}
	return t.UTC(), nil
}
