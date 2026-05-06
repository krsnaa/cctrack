package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/api"
	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usagescheduler"
)

// minimal API harness for /api/v1/day-drilldown. The handler only reads
// a.store, so hub and cfg are passed as nil — keeps the test surface narrow.
func newDrilldownHarness(t *testing.T) (*api.API, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Drilldown tests don't exercise /api/v1/summary or /api/v1/usage-sync,
	// so stub SummaryFunc + SyncOnceFunc are sufficient for api.New.
	stubSync := func(ctx context.Context) usagescheduler.SyncStatus {
		return usagescheduler.SyncStatus{Status: "ok"}
	}
	a := api.New(s, nil, nil, s.GetSummary, stubSync)
	return a, s
}

// seedDrilldownFixture seeds a small two-session shape into the store so the
// handler returns a non-trivial response. This is intentionally smaller than
// the regression fixture in internal/store — the API tests are about
// HTTP-layer correctness (status, validation, JSON shape), not bug-shape
// regression which is already covered at the store layer.
func seedDrilldownFixture(t *testing.T, s *store.Store) string {
	t.Helper()
	now := time.Now()
	dMid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -2)
	tsAt := func(hour int) string {
		return dMid.Add(time.Duration(hour) * time.Hour).UTC().Format(time.RFC3339Nano)
	}

	ingest := func(sessionID, project string, seq int, ts string, cost float64, tokens int64) {
		t.Helper()
		if err := s.UpsertSession(store.SessionDelta{
			ID: sessionID, Project: project, Slug: sessionID, Model: "claude-opus-4-7",
			Timestamp: ts, DeltaInput: tokens / 2, DeltaOutput: tokens / 2, DeltaCost: cost,
		}); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
		if err := s.UpsertRequest(store.RequestRecord{
			RequestID: fmt.Sprintf("%s-%02d", sessionID, seq), SessionID: sessionID,
			Timestamp: ts, Model: "claude-opus-4-7",
			InputTokens: tokens / 2, OutputTokens: tokens / 2, Cost: cost,
		}); err != nil {
			t.Fatalf("UpsertRequest: %v", err)
		}
	}

	ingest("api-sess-1", "api-project-x", 1, tsAt(10), 1.50, 1000)
	ingest("api-sess-1", "api-project-x", 2, tsAt(14), 2.50, 2000)
	ingest("api-sess-2", "api-project-y", 1, tsAt(16), 3.00, 1500)

	return dMid.Format("2006-01-02")
}

// TestHandleDayDrilldown_ValidDate_200 verifies the round-trip: handler
// returns 200, the response JSON parses into the expected shape, snake_case
// tags work, and the day_cost totals match the seeded fixture.
func TestHandleDayDrilldown_ValidDate_200(t *testing.T) {
	a, s := newDrilldownHarness(t)
	dayD := seedDrilldownFixture(t, s)

	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/day-drilldown?date="+dayD, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", got)
	}

	var resp struct {
		Date     string `json:"date"`
		Projects []struct {
			Project         string  `json:"project"`
			DayCost         float64 `json:"day_cost"`
			DayTokens       int64   `json:"day_tokens"`
			SessionCount    int     `json:"session_count"`
			DayRequestCount int     `json:"day_request_count"`
		} `json:"projects"`
		Sessions []struct {
			ID              string  `json:"id"`
			Project         string  `json:"project"`
			Slug            string  `json:"slug"`
			Model           string  `json:"model"`
			StartedAt       string  `json:"started_at"`
			LastActivity    string  `json:"last_activity"`
			DayCost         float64 `json:"day_cost"`
			DayTokens       int64   `json:"day_tokens"`
			DayRequestCount int     `json:"day_request_count"`
			LifetimeCost    float64 `json:"lifetime_cost"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%q)", err, rec.Body.String())
	}

	if resp.Date != dayD {
		t.Errorf("response date: want %q, got %q", dayD, resp.Date)
	}

	const wantTotal = 7.00 // 1.50 + 2.50 + 3.00
	var projectTotal, sessionTotal float64
	for _, p := range resp.Projects {
		projectTotal += p.DayCost
	}
	for _, sess := range resp.Sessions {
		sessionTotal += sess.DayCost
	}
	if projectTotal != wantTotal {
		t.Errorf("project day_cost total: want $%.2f, got $%.2f", wantTotal, projectTotal)
	}
	if sessionTotal != wantTotal {
		t.Errorf("session day_cost total: want $%.2f, got $%.2f", wantTotal, sessionTotal)
	}

	// Smoke-check snake_case round-trip: at least one session row carries
	// the lifetime_cost field and it equals the per-session lifetime totals.
	if len(resp.Sessions) == 0 {
		t.Fatal("sessions empty; expected 2 rows")
	}
	wantLifetimes := map[string]float64{"api-sess-1": 4.00, "api-sess-2": 3.00}
	for _, sess := range resp.Sessions {
		if want, ok := wantLifetimes[sess.ID]; ok && sess.LifetimeCost != want {
			t.Errorf("session %s lifetime_cost: want $%.2f, got $%.2f", sess.ID, want, sess.LifetimeCost)
		}
	}
}

// TestHandleDayDrilldown_BadDate_400 covers every reject path on
// ValidateDrilldownDate via the HTTP boundary, plus the handler-level missing
// query param case.
func TestHandleDayDrilldown_BadDate_400(t *testing.T) {
	a, _ := newDrilldownHarness(t)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	cases := []struct {
		label string
		path  string
	}{
		{"missing", "/api/v1/day-drilldown"},
		{"empty", "/api/v1/day-drilldown?date="},
		{"word", "/api/v1/day-drilldown?date=yesterday"},
		{"slash-separator", "/api/v1/day-drilldown?date=2026/04/24"},
		{"reverse-order", "/api/v1/day-drilldown?date=04-24-2026"},
		{"missing-zero-pad", "/api/v1/day-drilldown?date=2026-4-24"},
		{"trailing-junk", "/api/v1/day-drilldown?date=2026-04-24T"},
		{"two-digit-year", "/api/v1/day-drilldown?date=26-04-24"},
		{"month-out-of-range", "/api/v1/day-drilldown?date=2026-13-01"},
		{"day-out-of-range-feb", "/api/v1/day-drilldown?date=2026-02-30"},
		{"non-leap-year-feb-29", "/api/v1/day-drilldown?date=2025-02-29"},
		{"month-zero", "/api/v1/day-drilldown?date=2026-00-15"},
		{"day-zero", "/api/v1/day-drilldown?date=2026-04-00"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("path %q: want 400, got %d (body=%q)", c.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleDayDrilldown_EmptyDay_200 confirms the endpoint returns 200 with
// empty arrays (not nil, not 404) when no requests landed on the date — the
// frontend can render "no spend that day" without special-casing nulls.
func TestHandleDayDrilldown_EmptyDay_200(t *testing.T) {
	a, _ := newDrilldownHarness(t)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	farPast := time.Now().AddDate(0, 0, -90).Format("2006-01-02")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/day-drilldown?date="+farPast, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Empty slices should marshal to "[]", not "null". Stable for the UI.
	if !strings.Contains(body, `"projects":[]`) {
		t.Errorf("expected projects:[], body=%q", body)
	}
	if !strings.Contains(body, `"sessions":[]`) {
		t.Errorf("expected sessions:[], body=%q", body)
	}
}
