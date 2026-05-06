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
