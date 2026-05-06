//go:build discovery

package main

import (
	"bytes"
	"strings"
	"testing"
)

// validResponse returns a synthetic decoded response that contains every
// allowlisted path with its expected JSON kind, plus a few unrelated extras
// (named with neutral non-account-shaped tokens) to verify they are dropped.
func validResponse() map[string]any {
	return map[string]any{
		"five_hour": map[string]any{
			"utilization": float64(42),
			"resets_at":   "2026-05-06T17:00:00Z",
		},
		"seven_day": map[string]any{
			"utilization": float64(73),
			"resets_at":   "2026-05-13T00:00:00Z",
		},
		// Decoy-style and unrelated extras: must NOT appear in output.
		"alpha":          "x",
		"beta":           float64(1),
		"gamma_qux": map[string]any{"nested": float64(2)},
		"delta_phi":      []any{float64(3), float64(4)},
	}
}

func TestWalkResponse_AllPresent(t *testing.T) {
	var buf bytes.Buffer
	ok := walkResponse(&buf, validResponse())
	if !ok {
		t.Fatalf("walkResponse returned false; want true. output:\n%s", buf.String())
	}
	out := buf.String()

	wantLines := []string{
		"five_hour.utilization",
		"five_hour.resets_at",
		"seven_day.utilization",
		"seven_day.resets_at",
	}
	for _, p := range wantLines {
		if !strings.Contains(out, p) {
			t.Errorf("output missing allowlisted path %q\nfull output:\n%s", p, out)
		}
	}
	for _, expectedKind := range []string{"present=true"} {
		count := strings.Count(out, expectedKind)
		if count != 4 {
			t.Errorf("expected 4 occurrences of %q, got %d. output:\n%s", expectedKind, count, out)
		}
	}
}

// TestWalkResponse_DropsUnknownNames is the load-bearing sanitization test.
// It asserts that NONE of the unrelated field names from validResponse appear
// in stdout under any circumstances. A regression here would mean canary or
// decoy names could leak through.
func TestWalkResponse_DropsUnknownNames(t *testing.T) {
	var buf bytes.Buffer
	walkResponse(&buf, validResponse())
	out := buf.String()

	for _, leakName := range []string{"alpha", "beta", "gamma_qux", "delta_phi", "nested"} {
		if strings.Contains(out, leakName) {
			t.Errorf("output leaked non-allowlisted field name %q\nfull output:\n%s", leakName, out)
		}
	}
}

func TestWalkResponse_MissingAllowlistedField(t *testing.T) {
	resp := validResponse()
	delete(resp["five_hour"].(map[string]any), "utilization")

	var buf bytes.Buffer
	ok := walkResponse(&buf, resp)
	if ok {
		t.Fatalf("walkResponse returned true with missing utilization; want false")
	}
	out := buf.String()
	if !strings.Contains(out, "five_hour.utilization") || !strings.Contains(out, "MISSING") {
		t.Errorf("expected MISSING line for five_hour.utilization. output:\n%s", out)
	}
}

func TestWalkResponse_TypeMismatch(t *testing.T) {
	resp := validResponse()
	// utilization should be a number; provide string instead.
	resp["seven_day"].(map[string]any)["utilization"] = "73"

	var buf bytes.Buffer
	ok := walkResponse(&buf, resp)
	if ok {
		t.Fatalf("walkResponse returned true with type mismatch; want false")
	}
	out := buf.String()
	if !strings.Contains(out, "seven_day.utilization") || !strings.Contains(out, "TYPE_MISMATCH") {
		t.Errorf("expected TYPE_MISMATCH line for seven_day.utilization. output:\n%s", out)
	}
	if !strings.Contains(out, "expected number") || !strings.Contains(out, "got string") {
		t.Errorf("type-mismatch line should name expected and actual types. output:\n%s", out)
	}
}

// TestWalkResponse_NoLeakageOnEmptyResponse confirms that an empty response
// produces only MISSING lines for the allowlisted paths and nothing else.
// Empty response is a stand-in for any pathological top-level shape; the
// invariant is that output never contains content sourced from non-allowlisted
// keys.
func TestWalkResponse_NoLeakageOnEmptyResponse(t *testing.T) {
	var buf bytes.Buffer
	ok := walkResponse(&buf, map[string]any{})
	if ok {
		t.Fatalf("walkResponse on empty input returned true; want false")
	}
	out := buf.String()
	missingCount := strings.Count(out, "MISSING")
	if missingCount != 4 {
		t.Errorf("expected 4 MISSING lines for empty input, got %d. output:\n%s", missingCount, out)
	}
}

func TestPathStatus(t *testing.T) {
	cases := []struct {
		path         string
		wantExact    bool
		wantPrefix   bool
		wantExpected string
	}{
		{"", false, true, ""},
		{"five_hour", false, true, ""},
		{"five_hour.utilization", true, false, "number"},
		{"five_hour.resets_at", true, false, "string"},
		{"seven_day", false, true, ""},
		{"seven_day.utilization", true, false, "number"},
		{"unknown_top", false, false, ""},
		{"five_hour.unknown", false, false, ""},
		{"five_hour_lookalike", false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			gotExact, gotPrefix, gotExpected := pathStatus(c.path)
			if gotExact != c.wantExact || gotPrefix != c.wantPrefix || gotExpected != c.wantExpected {
				t.Errorf("pathStatus(%q) = (%v, %v, %q), want (%v, %v, %q)",
					c.path, gotExact, gotPrefix, gotExpected,
					c.wantExact, c.wantPrefix, c.wantExpected)
			}
		})
	}
}

func TestJSONType(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{nil, "null"},
		{true, "bool"},
		{float64(1.5), "number"},
		{"hello", "string"},
		{map[string]any{}, "object"},
		{[]any{}, "array"},
		{int(1), "unknown"}, // Go ints don't appear after json.Decode into any
	}
	for _, c := range cases {
		got := jsonType(c.v)
		if got != c.want {
			t.Errorf("jsonType(%T) = %q, want %q", c.v, got, c.want)
		}
	}
}
