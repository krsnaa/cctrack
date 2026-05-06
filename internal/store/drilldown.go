package store

import (
	"fmt"
	"time"
)

// DayDrilldown is the response shape for the daily-spend bar click-through.
// All numeric fields under Projects[] and Sessions[] derive from per-request
// rows in the requests table filtered by DATE(r.timestamp, 'localtime') = D
// (Invariant A). Only LifetimeCost on a session row reads sessions.total_cost
// — and never contributes to any *DayCost or DayTokens sum on this struct.
type DayDrilldown struct {
	Date     string            `json:"date"`
	Projects []DayProjectGroup `json:"projects"`
	Sessions []DaySessionRow   `json:"sessions"`
}

type DayProjectGroup struct {
	Project         string  `json:"project"`
	DayCost         float64 `json:"day_cost"`
	DayTokens       int64   `json:"day_tokens"`
	SessionCount    int     `json:"session_count"`
	DayRequestCount int     `json:"day_request_count"`
}

type DaySessionRow struct {
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
}

// ValidateDrilldownDate enforces both the YYYY-MM-DD shape AND calendar
// validity the drilldown query expects. Surfaced as a separate function so
// the API handler can return 400 on bad input without touching the DB.
//
// time.Parse("2006-01-02") is strict on both axes: the fixed-width layout
// rejects un-zero-padded inputs (`2026-4-24`), trailing junk (`2026-04-24T`),
// and wrong separators (`2026/04/24`); the underlying calendar logic rejects
// out-of-range months (`2026-13-01`), out-of-range days (`2026-02-30`), and
// non-leap-year Feb 29 (`2025-02-29`). Together these cover both the shape
// and semantic-validity bars.
func ValidateDrilldownDate(date string) error {
	if date == "" {
		return fmt.Errorf("date is required (format YYYY-MM-DD)")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return fmt.Errorf("date must be a valid YYYY-MM-DD; got %q: %w", date, err)
	}
	return nil
}

// GetDayDrilldown returns request-day spend for `date` (a local YYYY-MM-DD
// string), grouped both by project and by session. Both queries filter
// requests with the identical predicate `DATE(r.timestamp, 'localtime') = ?`
// so their day_cost totals agree with each other and with GetDailySummary —
// matching Invariant A.
//
// Sessions whose lifetime spans day D contribute only the per-request cost
// landing on D, not their full sessions.total_cost. lifetime_cost is carried
// alongside as a UI affordance and is never summed into day_cost or
// day_tokens.
func (s *Store) GetDayDrilldown(date string) (*DayDrilldown, error) {
	if err := ValidateDrilldownDate(date); err != nil {
		return nil, err
	}

	out := &DayDrilldown{Date: date, Projects: []DayProjectGroup{}, Sessions: []DaySessionRow{}}

	projectRows, err := s.db.Query(`
		SELECT s.project,
			SUM(r.cost) AS day_cost,
			SUM(r.input_tokens + r.output_tokens + r.cache_read_tokens
				+ r.cache_write_5m_tokens + r.cache_write_1h_tokens) AS day_tokens,
			COUNT(DISTINCT r.session_id) AS session_count,
			COUNT(*) AS day_request_count
		FROM requests r
		JOIN sessions s ON s.id = r.session_id
		WHERE DATE(r.timestamp, 'localtime') = ?
		GROUP BY s.project
		ORDER BY day_cost DESC`, date)
	if err != nil {
		return nil, err
	}
	defer projectRows.Close()
	for projectRows.Next() {
		var g DayProjectGroup
		if err := projectRows.Scan(&g.Project, &g.DayCost, &g.DayTokens, &g.SessionCount, &g.DayRequestCount); err != nil {
			return nil, err
		}
		out.Projects = append(out.Projects, g)
	}
	if err := projectRows.Err(); err != nil {
		return nil, err
	}

	sessionRows, err := s.db.Query(`
		SELECT s.id, s.project, s.slug, s.model, s.started_at, s.last_activity,
			SUM(r.cost) AS day_cost,
			SUM(r.input_tokens + r.output_tokens + r.cache_read_tokens
				+ r.cache_write_5m_tokens + r.cache_write_1h_tokens) AS day_tokens,
			COUNT(*) AS day_request_count,
			s.total_cost AS lifetime_cost
		FROM requests r
		JOIN sessions s ON s.id = r.session_id
		WHERE DATE(r.timestamp, 'localtime') = ?
		GROUP BY s.id
		ORDER BY day_cost DESC`, date)
	if err != nil {
		return nil, err
	}
	defer sessionRows.Close()
	for sessionRows.Next() {
		var row DaySessionRow
		if err := sessionRows.Scan(&row.ID, &row.Project, &row.Slug, &row.Model,
			&row.StartedAt, &row.LastActivity,
			&row.DayCost, &row.DayTokens, &row.DayRequestCount,
			&row.LifetimeCost); err != nil {
			return nil, err
		}
		out.Sessions = append(out.Sessions, row)
	}
	if err := sessionRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}
