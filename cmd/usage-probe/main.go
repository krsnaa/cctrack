//go:build discovery

// Command usage-probe is a one-shot schema-discovery tool for the private
// Anthropic OAuth usage endpoint. It is gated behind the `discovery` build
// tag so default `go build` and `go test ./...` invocations exclude it.
//
// Run once, locally, on a machine with a fresh Claude Code login:
//
//	go run -tags discovery ./cmd/usage-probe
//
// The probe emits name + type + presence for the four cctrack-used fields
// only (five_hour and seven_day, each with utilization and resets_at).
// Every other field name and value in the response is silently dropped —
// names that are not in the allowlist do not appear in output at all. An
// allowlisted path missing from the live response is reported as MISSING;
// one with the wrong JSON type is reported as TYPE_MISMATCH.
//
// This inverse-allowlist model means surprise additions to the live
// schema (including any decoy or canary names) cannot leak through into
// stdout. The single source of truth for emitted paths is the same
// allowlist the production usageprovider parses against (see walk.go).
//
// Exit codes:
//
//	0 — all four allowlisted paths present with expected types
//	1 — credentials, network, or decode failure
//	2 — non-200 HTTP response
//	3 — allowlisted path missing or type-mismatched (schema drift)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ksred/cctrack/internal/credentials"
	"github.com/ksred/cctrack/internal/usageprovider"
)

const probeTimeout = 10 * time.Second

func main() {
	creds, err := credentials.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageprovider.DefaultEndpoint, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request build: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("anthropic-beta", usageprovider.BetaHeader)
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

	fmt.Println("--- allowlisted schema check ---")
	if !walkResponse(os.Stdout, raw) {
		os.Exit(3)
	}
}
