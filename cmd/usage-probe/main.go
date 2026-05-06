//go:build discovery

// Command usage-probe is a one-shot schema-discovery tool for the private
// Anthropic OAuth usage endpoint. It is gated behind the `discovery` build
// tag so default `go build` and `go test ./...` invocations exclude it.
//
// Run once, locally, on a machine with a fresh Claude Code login:
//
//	go run -tags discovery ./cmd/usage-probe
//
// The probe walks the JSON response and prints a SANITIZED summary: top-level
// and nested field NAMES + Go JSON types. It never prints, persists, or
// otherwise surfaces raw response bodies, OAuth tokens, or account-shaped
// values. The two allowlisted utilization fields
// (five_hour.utilization, seven_day.utilization) have their integer values
// surfaced because integer percentages 0-100 are not sensitive. All other
// field values are elided. Field NAMES that match a defensive substring
// allowlist of generic auth/account vocabulary are also redacted.
//
// Use the output to update internal/usageprovider/SCHEMA.md by hand. Do NOT
// paste raw probe output into chat or commit it as-is.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ksred/cctrack/internal/credentials"
	"github.com/ksred/cctrack/internal/usageprovider"
)

const (
	endpointURL  = usageprovider.DefaultEndpoint
	betaHeader   = usageprovider.BetaHeader
	probeTimeout = 10 * time.Second
)

// allowlistedNumericPaths lists dotted JSON paths whose numeric values are
// safe to surface (utilization percentages, integers 0-100). Every other
// field's value is elided.
var allowlistedNumericPaths = map[string]bool{
	"five_hour.utilization": true,
	"seven_day.utilization": true,
}

// suppressedNameFragments is enforcement-only: a defensive substring filter
// of generic auth/account vocabulary. If any live-response field name
// contains one of these fragments (case-insensitive), the probe prints
// "<redacted>" instead of the name. These are common English words used as
// guardrail patterns, not field names observed from the endpoint.
var suppressedNameFragments = []string{
	"token", "secret", "subscription", "tier", "organization", "account", "email",
}

func main() {
	creds, err := credentials.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "credentials: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request build: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("Accept", "application/json")

	httpc := &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := httpc.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "transport: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	fmt.Printf("HTTP %d\n", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "non-200 response; not decoding body")
		os.Exit(2)
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("--- sanitized schema (field names + types) ---")
	walk("", raw)
}

// walk prints field paths and their JSON-derived types. Values are surfaced
// only for paths in allowlistedNumericPaths. Field names whose case-folded
// form contains any suppressed fragment are printed as <redacted>.
func walk(prefix string, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			displayName := k
			if isSuppressed(k) {
				displayName = "<redacted>"
				fmt.Printf("%-40s  type=%-8s  (account-shaped, not surfaced)\n", prefix+"."+displayName, jsonType(v[k]))
				continue
			}
			child := v[k]
			switch child.(type) {
			case map[string]any, []any:
				fmt.Printf("%-40s  type=%-8s\n", path, jsonType(child))
				walk(path, child)
			default:
				if allowlistedNumericPaths[path] {
					fmt.Printf("%-40s  type=%-8s  value=%v\n", path, jsonType(child), child)
				} else {
					fmt.Printf("%-40s  type=%-8s  value=<elided>\n", path, jsonType(child))
				}
			}
		}
	case []any:
		fmt.Printf("%-40s  type=%-8s  len=%d\n", prefix, "array", len(v))
		// Walk only the first element shape, not values.
		if len(v) > 0 {
			walk(prefix+"[0]", v[0])
		}
	}
}

func isSuppressed(name string) bool {
	lower := strings.ToLower(name)
	for _, frag := range suppressedNameFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64, json.Number:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "unknown"
	}
}

// Compile-time guard that the probe links against credentials and
// usageprovider so the build tag refactor doesn't accidentally orphan it.
var _ = errors.New
