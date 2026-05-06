package store_test

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/store"
)

// newTestStore opens a fresh on-disk SQLite file inside t.TempDir(), runs the
// migration, and registers cleanup. Each test gets an isolated DB.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("opening test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func floatEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// fixtureBuilder ingests synthetic activity through the same Upsert*
// surfaces the parser uses in production. UpsertSession is called *before*
// UpsertRequest to match the FK direction (requests.session_id → sessions.id)
// and the production parser order.
type fixtureBuilder struct {
	t   *testing.T
	s   *store.Store
	seq map[string]int // sessionID → next request sequence; gives deterministic request IDs
}

func newFixtureBuilder(t *testing.T, s *store.Store) *fixtureBuilder {
	return &fixtureBuilder{t: t, s: s, seq: map[string]int{}}
}

// ingest writes one synthetic request: advances session totals + last_activity
// via UpsertSession (additive), then writes the matching requests row via
// UpsertRequest. Timestamp is formatted as UTC RFC3339Nano so lex-compare in
// UpsertSession's last_activity advance works correctly.
func (f *fixtureBuilder) ingest(sessionID, project string, ts time.Time, cost float64, inputTok, outputTok int64) {
	f.t.Helper()
	f.seq[sessionID]++
	requestID := fmt.Sprintf("%s-%02d", sessionID, f.seq[sessionID])
	tsStr := ts.UTC().Format(time.RFC3339Nano)

	if err := f.s.UpsertSession(store.SessionDelta{
		ID:           sessionID,
		Project:      project,
		Slug:         sessionID,
		Model:        "claude-opus-4-7",
		Timestamp:    tsStr,
		DeltaInput:   inputTok,
		DeltaOutput:  outputTok,
		DeltaCost:    cost,
	}); err != nil {
		f.t.Fatalf("UpsertSession %s: %v", requestID, err)
	}
	if err := f.s.UpsertRequest(store.RequestRecord{
		RequestID:    requestID,
		SessionID:    sessionID,
		Timestamp:    tsStr,
		Model:        "claude-opus-4-7",
		InputTokens:  inputTok,
		OutputTokens: outputTok,
		Cost:         cost,
	}); err != nil {
		f.t.Fatalf("UpsertRequest %s: %v", requestID, err)
	}
}

// buildBugShapeFixture seeds the store with three sessions that demonstrate
// the F1 click-through bug from BOTH directions on a single day D:
//
//   - Session A "cross-day-tail" (UNDERcount): expensive $50 request lands on
//     day D, but last_activity advances to D+1 because of a tail request the
//     next morning. Current GetProjectGroups(date=D) excludes this session
//     entirely — kiku's "cents instead of thousands" symptom.
//
//   - Session B "same-day-cheap" (control): trivial single $0.05 request on D.
//     Same-day session, included by both views, no bug interaction.
//
//   - Session C "spillover-overcount" (OVERcount): expensive $30 request on
//     D-1 plus a tiny $0.05 request on D. last_activity = D, so current
//     GetProjectGroups(date=D) includes the FULL lifetime $30.05 even though
//     only $0.05 was actually incurred on day D — the symmetric error.
//
// D is two days ago in local time so it always sits inside the
// GetDailySummary(30) window regardless of when the test runs. Returns D as
// "YYYY-MM-DD" for assertion convenience.
func buildBugShapeFixture(t *testing.T, s *store.Store) string {
	t.Helper()
	f := newFixtureBuilder(t, s)

	now := time.Now()
	dMid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -2)
	dMinus1Mid := dMid.AddDate(0, 0, -1)
	dPlus1Mid := dMid.AddDate(0, 0, 1)

	at := func(mid time.Time, hour, minute int) time.Time {
		return mid.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}

	// Session A: started D-1, expensive on D, tail on D+1.
	f.ingest("session-A", "project-alpha", at(dMinus1Mid, 14, 0), 0.10, 100, 50)
	f.ingest("session-A", "project-alpha", at(dMid, 22, 30), 50.00, 50_000, 10_000)
	f.ingest("session-A", "project-alpha", at(dPlus1Mid, 0, 30), 0.10, 100, 50)

	// Session B: single same-day request.
	f.ingest("session-B", "project-beta", at(dMid, 18, 0), 0.05, 50, 25)

	// Session C: expensive on D-1, cheap on D, last_activity ends on D.
	f.ingest("session-C", "project-gamma", at(dMinus1Mid, 14, 0), 30.00, 30_000, 5_000)
	f.ingest("session-C", "project-gamma", at(dMid, 9, 0), 0.05, 50, 25)

	return dMid.Format("2006-01-02")
}

// TestF1Regression_DailySummary_DayDMatchesRequestDayTruth pins the truth: the
// day-D bar height computed from per-request cost (Invariant A). The daily
// summary already aggregates correctly because it filters DATE(timestamp,
// 'localtime') over the requests table. Sum: A's $50.00 (day D) + B's $0.05 +
// C's $0.05 (day D) = $50.10.
func TestF1Regression_DailySummary_DayDMatchesRequestDayTruth(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	daily, err := s.GetDailySummary(30)
	if err != nil {
		t.Fatalf("GetDailySummary: %v", err)
	}

	var got float64
	var found bool
	for _, d := range daily {
		if d.Date == dayD {
			got = d.Cost
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("day %s missing from GetDailySummary(30) output", dayD)
	}

	const want = 50.10
	if !floatEq(got, want) {
		t.Errorf("daily-summary day=%s: want $%.2f (request-day truth), got $%.2f", dayD, want, got)
	}
}

// TestF1Regression_GetProjectGroups_DateFilterReturnsLifetimeNotDayTruth
// documents the F1 click-through bug shape using only existing functions.
// GetProjectGroups filtered by last_activity returns a *session-lifetime
// rollup* (Invariant B's intended semantic for the canonical browse view),
// NOT request-day truth. Against this fixture the rollup misses Session A's
// $50 (excluded by the last_activity=D+1 filter — UNDERcount) AND includes
// Session C's full $30.05 lifetime (despite only $0.05 incurred on D —
// OVERcount). Daily truth is $50.10; the rollup returns $30.10.
//
// This test pins the *existing* GetProjectGroups semantic so the canonical
// browse view doesn't accidentally drift to request-day numbers later — the
// drilldown query (T1.1.2) is the new home for day-scoped totals.
func TestF1Regression_GetProjectGroups_DateFilterReturnsLifetimeNotDayTruth(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	groups, err := s.GetProjectGroups(dayD, "cost", "desc")
	if err != nil {
		t.Fatalf("GetProjectGroups: %v", err)
	}

	var total float64
	seen := map[string]float64{}
	for _, g := range groups {
		total += g.TotalCost
		seen[g.Project] = g.TotalCost
	}

	// Lifetime rollup: B's $0.05 + C's $30.05. A excluded (last_activity = D+1).
	const wantTotal = 30.10
	if !floatEq(total, wantTotal) {
		t.Errorf("GetProjectGroups date=%s: lifetime-rollup total want $%.2f, got $%.2f", dayD, wantTotal, total)
	}

	if _, ok := seen["project-alpha"]; ok {
		t.Errorf("Session A (cross-day): expected EXCLUDED by last_activity=D+1 filter; appeared with cost $%.2f", seen["project-alpha"])
	}
	if _, ok := seen["project-beta"]; !ok {
		t.Errorf("Session B (same-day): expected included; missing")
	} else if !floatEq(seen["project-beta"], 0.05) {
		t.Errorf("Session B lifetime: want $0.05, got $%.2f", seen["project-beta"])
	}
	if _, ok := seen["project-gamma"]; !ok {
		t.Errorf("Session C (overcount): expected included; missing")
	} else if !floatEq(seen["project-gamma"], 30.05) {
		t.Errorf("Session C lifetime: want $30.05 (proves the OVERcount direction), got $%.2f", seen["project-gamma"])
	}

	// The bug-shape contrast: project-groups disagrees with daily truth.
	daily, _ := s.GetDailySummary(30)
	var dayTruth float64
	for _, d := range daily {
		if d.Date == dayD {
			dayTruth = d.Cost
		}
	}
	if floatEq(total, dayTruth) {
		t.Errorf("project-groups total ($%.2f) unexpectedly equals daily truth ($%.2f); fixture is no longer demonstrating the F1 bug shape", total, dayTruth)
	}
}

// TestDayDrilldown_AgreesWithDailySummary is the central correctness assertion
// for T1.1.2: the new drilldown's project-group totals AND session-row totals
// both equal the daily-summary truth for D, satisfying Invariant A.
func TestDayDrilldown_AgreesWithDailySummary(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	dd, err := s.GetDayDrilldown(dayD)
	if err != nil {
		t.Fatalf("GetDayDrilldown: %v", err)
	}

	daily, err := s.GetDailySummary(30)
	if err != nil {
		t.Fatalf("GetDailySummary: %v", err)
	}
	var dayTruth float64
	for _, d := range daily {
		if d.Date == dayD {
			dayTruth = d.Cost
			break
		}
	}

	var projectTotal, sessionTotal float64
	for _, p := range dd.Projects {
		projectTotal += p.DayCost
	}
	for _, sess := range dd.Sessions {
		sessionTotal += sess.DayCost
	}

	if !floatEq(projectTotal, dayTruth) {
		t.Errorf("drilldown project-total $%.2f != daily-summary $%.2f", projectTotal, dayTruth)
	}
	if !floatEq(sessionTotal, dayTruth) {
		t.Errorf("drilldown session-total $%.2f != daily-summary $%.2f", sessionTotal, dayTruth)
	}
	if !floatEq(projectTotal, sessionTotal) {
		t.Errorf("drilldown project-total $%.2f != session-total $%.2f (internal inconsistency)", projectTotal, sessionTotal)
	}
}

// TestDayDrilldown_IncludesCrossDaySession proves Session A (the UNDERcount
// direction) is now captured: even though last_activity = D+1, A's $50.00
// request on day D shows up in the drilldown because the filter is on
// r.timestamp, not s.last_activity.
func TestDayDrilldown_IncludesCrossDaySession(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	dd, err := s.GetDayDrilldown(dayD)
	if err != nil {
		t.Fatalf("GetDayDrilldown: %v", err)
	}

	var sessionA *store.DaySessionRow
	for i := range dd.Sessions {
		if dd.Sessions[i].ID == "session-A" {
			sessionA = &dd.Sessions[i]
			break
		}
	}
	if sessionA == nil {
		t.Fatalf("session-A missing from drilldown sessions; cross-day request was excluded")
	}
	if !floatEq(sessionA.DayCost, 50.00) {
		t.Errorf("session-A day_cost: want $50.00 (D request only), got $%.2f", sessionA.DayCost)
	}
	if sessionA.DayRequestCount != 1 {
		t.Errorf("session-A day_request_count: want 1 (only D-22:30 request), got %d", sessionA.DayRequestCount)
	}
}

// TestDayDrilldown_OvercountSession_DayCostBoundedToDayRequests proves
// Session C (the OVERcount direction) is constrained: C's expensive $30.00
// request on D-1 must NOT appear in day_cost; only the $0.05 request on D
// counts. Lifetime $30.05 is preserved on lifetime_cost for the UI.
func TestDayDrilldown_OvercountSession_DayCostBoundedToDayRequests(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	dd, err := s.GetDayDrilldown(dayD)
	if err != nil {
		t.Fatalf("GetDayDrilldown: %v", err)
	}

	var sessionC *store.DaySessionRow
	for i := range dd.Sessions {
		if dd.Sessions[i].ID == "session-C" {
			sessionC = &dd.Sessions[i]
			break
		}
	}
	if sessionC == nil {
		t.Fatalf("session-C missing from drilldown")
	}
	if !floatEq(sessionC.DayCost, 0.05) {
		t.Errorf("session-C day_cost: want $0.05 (D request only), got $%.2f — OVERcount NOT prevented", sessionC.DayCost)
	}
	if !floatEq(sessionC.LifetimeCost, 30.05) {
		t.Errorf("session-C lifetime_cost: want $30.05 (lifetime preserved for UI), got $%.2f", sessionC.LifetimeCost)
	}
}

// TestDayDrilldown_LifetimeCostNotInDaySum is the explicit fence: lifetime_cost
// is exposed on each session row but never contributes to project-level
// day_cost or session-level day_cost sums. Sums of lifetime_cost would equal
// $50.20 + $0.05 + $30.05 = $80.30; the drilldown's day-scoped fields sum to
// $50.10 (the truth).
func TestDayDrilldown_LifetimeCostNotInDaySum(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	dd, err := s.GetDayDrilldown(dayD)
	if err != nil {
		t.Fatalf("GetDayDrilldown: %v", err)
	}

	var dayCostTotal, lifetimeTotal float64
	for _, sess := range dd.Sessions {
		dayCostTotal += sess.DayCost
		lifetimeTotal += sess.LifetimeCost
	}

	const wantDayTotal = 50.10
	const wantLifetimeTotal = 80.30 // 50.20 (A) + 0.05 (B) + 30.05 (C)
	if !floatEq(dayCostTotal, wantDayTotal) {
		t.Errorf("day-cost total: want $%.2f, got $%.2f", wantDayTotal, dayCostTotal)
	}
	if !floatEq(lifetimeTotal, wantLifetimeTotal) {
		t.Errorf("lifetime-cost total: want $%.2f, got $%.2f", wantLifetimeTotal, lifetimeTotal)
	}
	if floatEq(dayCostTotal, lifetimeTotal) {
		t.Errorf("day-cost ($%.2f) collapsed to lifetime-cost ($%.2f) — the lifetime-leaking-into-day-sums fence broke", dayCostTotal, lifetimeTotal)
	}
}

// TestDayDrilldown_ProjectGroup_FieldsConsistent walks the project-level shape
// and asserts session_count and day_request_count are sane: A has 1 day-D
// request, B has 1, C has 1; project-alpha → 1 session / 1 request,
// project-beta → 1 / 1, project-gamma → 1 / 1.
func TestDayDrilldown_ProjectGroup_FieldsConsistent(t *testing.T) {
	s := newTestStore(t)
	dayD := buildBugShapeFixture(t, s)

	dd, err := s.GetDayDrilldown(dayD)
	if err != nil {
		t.Fatalf("GetDayDrilldown: %v", err)
	}
	byProject := map[string]store.DayProjectGroup{}
	for _, p := range dd.Projects {
		byProject[p.Project] = p
	}

	cases := []struct {
		project       string
		wantDayCost   float64
		wantSessions  int
		wantRequests  int
	}{
		{"project-alpha", 50.00, 1, 1},
		{"project-beta", 0.05, 1, 1},
		{"project-gamma", 0.05, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.project, func(t *testing.T) {
			p, ok := byProject[c.project]
			if !ok {
				t.Fatalf("%s missing from drilldown projects", c.project)
			}
			if !floatEq(p.DayCost, c.wantDayCost) {
				t.Errorf("day_cost: want $%.2f, got $%.2f", c.wantDayCost, p.DayCost)
			}
			if p.SessionCount != c.wantSessions {
				t.Errorf("session_count: want %d, got %d", c.wantSessions, p.SessionCount)
			}
			if p.DayRequestCount != c.wantRequests {
				t.Errorf("day_request_count: want %d, got %d", c.wantRequests, p.DayRequestCount)
			}
		})
	}
}

// TestDayDrilldown_EmptyDay returns empty Projects/Sessions slices (not nil)
// when no requests landed on the date, which keeps the JSON shape stable for
// the UI (no null-vs-array discrimination).
func TestDayDrilldown_EmptyDay(t *testing.T) {
	s := newTestStore(t)
	buildBugShapeFixture(t, s)

	// 30 days ago: well outside the fixture range.
	farPast := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	dd, err := s.GetDayDrilldown(farPast)
	if err != nil {
		t.Fatalf("GetDayDrilldown empty day: %v", err)
	}
	if dd.Projects == nil {
		t.Errorf("Projects: want empty slice, got nil")
	}
	if dd.Sessions == nil {
		t.Errorf("Sessions: want empty slice, got nil")
	}
	if len(dd.Projects) != 0 || len(dd.Sessions) != 0 {
		t.Errorf("expected empty drilldown, got %d projects / %d sessions", len(dd.Projects), len(dd.Sessions))
	}
}

// TestValidateDrilldownDate covers the date-shape validator. The same
// function is used by the API handler to return 400; covering it at the
// store layer keeps the contract testable without spinning up a server.
func TestValidateDrilldownDate(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"2026-04-24", false},
		{"2026-12-31", false},
		{"2024-02-29", false}, // 2024 is a leap year
		{"", true},
		{"yesterday", true},
		{"2026/04/24", true},
		{"04-24-2026", true},
		{"2026-4-24", true},   // missing leading zero
		{"2026-04-24T", true}, // trailing junk
		{"26-04-24", true},    // 2-digit year
		{"2026-13-01", true},  // month out of range
		{"2026-02-30", true},  // day out of range for Feb
		{"2025-02-29", true},  // non-leap-year Feb 29
		{"2026-00-15", true},  // month zero
		{"2026-04-00", true},  // day zero
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			err := store.ValidateDrilldownDate(c.input)
			if (err != nil) != c.wantErr {
				t.Errorf("input %q: wantErr=%v, got %v", c.input, c.wantErr, err)
			}
		})
	}
}

// TestProjectGroups_NoDate_RollupSemanticsUnchanged is the Invariant B fence:
// when GetProjectGroups is called without a date filter, it still returns
// session-lifetime rollups identical to a baseline computed directly from
// the sessions table. Adding the new drilldown must not silently change the
// canonical browse view.
func TestProjectGroups_NoDate_RollupSemanticsUnchanged(t *testing.T) {
	s := newTestStore(t)
	buildBugShapeFixture(t, s)

	groups, err := s.GetProjectGroups("", "cost", "desc")
	if err != nil {
		t.Fatalf("GetProjectGroups: %v", err)
	}
	got := map[string]float64{}
	for _, g := range groups {
		got[g.Project] = g.TotalCost
	}

	// Baseline: lifetime totals straight from the fixture (matches sessions.total_cost).
	wantBaseline := map[string]float64{
		"project-alpha": 50.20, // A: $0.10 + $50.00 + $0.10
		"project-beta":  0.05,  // B
		"project-gamma": 30.05, // C: $30.00 + $0.05
	}
	for project, want := range wantBaseline {
		if !floatEq(got[project], want) {
			t.Errorf("%s lifetime total: want $%.2f, got $%.2f (rollup semantics drifted)", project, want, got[project])
		}
	}
}

// TestListSessions_NoDate_BehaviorUnchanged is the second half of the
// Invariant B fence: the canonical paginated session-browse path returns the
// same result regardless of the new endpoint's existence. Sessions sorted by
// total_cost descending, no filtering.
func TestListSessions_NoDate_BehaviorUnchanged(t *testing.T) {
	s := newTestStore(t)
	buildBugShapeFixture(t, s)

	sessions, total, err := s.ListSessions(50, 0, "cost", "desc", "", "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if total != 3 {
		t.Errorf("total: want 3 (A/B/C), got %d", total)
	}
	if len(sessions) != 3 {
		t.Errorf("returned: want 3, got %d", len(sessions))
	}
	// First by cost-desc should be Session A ($50.20).
	if len(sessions) > 0 && sessions[0].ID != "session-A" {
		t.Errorf("first session by cost-desc: want session-A, got %s", sessions[0].ID)
	}
	if len(sessions) > 0 && !floatEq(sessions[0].TotalCost, 50.20) {
		t.Errorf("session-A total_cost: want $50.20, got $%.2f", sessions[0].TotalCost)
	}
}
