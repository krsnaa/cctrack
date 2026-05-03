package store

import (
	"fmt"
	"time"

	"github.com/ksred/cctrack/internal/calculator"
)

type Summary struct {
	Window5h  WindowBucket `json:"window_5h"`
	Today     SpendBucket  `json:"today"`
	Window7d  WindowBucket `json:"window_7d"`
	Month     SpendBucket  `json:"month"`
	Projected float64      `json:"projected"`
}

// WindowBucket describes a rolling usage window (5h or 7d) computed by walking
// the request timestamp stream forward and opening a new window every time a
// request lands at-or-after the previous window's end. Cascading windows
// (each closes on its own clock, not on inactivity) — see GetWindowBucket.
type WindowBucket struct {
	Start        string  `json:"start"`         // ISO 8601 UTC
	End          string  `json:"end"`           // ISO 8601 UTC
	Cost         float64 `json:"cost"`          // total cost of requests in this window
	Tokens       int64   `json:"tokens"`        // total tokens
	RequestCount int     `json:"request_count"` // number of requests
	PrevCost     float64 `json:"prev_cost"`     // cost of the previous window
	PrevStart    string  `json:"prev_start"`    // ISO 8601 UTC; "" if no prev window
}

type SpendBucket struct {
	Cost   float64 `json:"cost"`
	Tokens int64   `json:"tokens"`
}

type DailySpend struct {
	Date string  `json:"date"`
	Cost float64 `json:"cost"`
}

// GetSummary builds the dashboard summary. Both 5h and 7d windows are rolling
// (each window opens at the first event after the previous one expired). When
// the user has synced an anchor from claude.ai (a "I see N min left right
// now" entry), we use that to override the cascading detection — it closes
// the data-source-scope gap that the cascading inference can't bridge.
func (s *Store) GetSummary() (*Summary, error) {
	now := time.Now()
	todayStr := now.Format("2006-01-02")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")

	summary := &Summary{}

	w5, err := s.windowFromAnchorOrCascade("5h", 5*time.Hour)
	if err != nil {
		return nil, err
	}
	summary.Window5h = w5

	w7, err := s.windowFromAnchorOrCascade("7d", 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	summary.Window7d = w7

	// last_activity is stored as a UTC ISO timestamp from the JSONL log; the
	// 'localtime' modifier converts to the host's local zone so "today" /
	// "this month" buckets reflect the user's calendar, not UTC's.
	err = s.db.QueryRow(`
		SELECT COALESCE(SUM(total_cost), 0),
		       COALESCE(SUM(total_input + total_output + total_cache_read + total_cache_write_5m + total_cache_write_1h), 0)
		FROM sessions WHERE DATE(last_activity, 'localtime') >= ?`, todayStr).Scan(&summary.Today.Cost, &summary.Today.Tokens)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRow(`
		SELECT COALESCE(SUM(total_cost), 0),
		       COALESCE(SUM(total_input + total_output + total_cache_read + total_cache_write_5m + total_cache_write_1h), 0)
		FROM sessions WHERE DATE(last_activity, 'localtime') >= ?`, monthStart).Scan(&summary.Month.Cost, &summary.Month.Tokens)
	if err != nil {
		return nil, err
	}

	// Projected: current month cost / days elapsed * days in month
	dayOfMonth := now.Day()
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
	if dayOfMonth > 0 && summary.Month.Cost > 0 {
		summary.Projected = summary.Month.Cost / float64(dayOfMonth) * float64(daysInMonth)
	}

	return summary, nil
}

// windowFromAnchorOrCascade prefers a fresh user-synced anchor for the given
// window type; falls back to cascading inference when no anchor exists or the
// anchored window has fully elapsed (in which case the user needs to re-sync).
func (s *Store) windowFromAnchorOrCascade(windowType string, duration time.Duration) (WindowBucket, error) {
	a, err := s.GetLatestAnchor(windowType)
	if err != nil {
		return WindowBucket{}, err
	}
	if a != nil {
		syncedAt, perr := time.Parse(time.RFC3339Nano, a.SyncedAt)
		if perr == nil {
			anchoredEnd := syncedAt.Add(time.Duration(a.TimeLeftMinutes) * time.Minute)
			if time.Now().Before(anchoredEnd) {
				// Anchor still valid — use it.
				start := anchoredEnd.Add(-duration)
				return s.computeAnchoredBucket(start, anchoredEnd, duration)
			}
		}
	}
	return s.GetWindowBucket(duration)
}

func (s *Store) computeAnchoredBucket(start, end time.Time, duration time.Duration) (WindowBucket, error) {
	startStr := start.UTC().Format(time.RFC3339Nano)
	endStr := end.UTC().Format(time.RFC3339Nano)
	prevStart := start.Add(-duration)
	prevStartStr := prevStart.UTC().Format(time.RFC3339Nano)

	bucket := WindowBucket{
		Start:     startStr,
		End:       endStr,
		PrevStart: prevStartStr,
	}
	if err := s.db.QueryRow(`
		SELECT COALESCE(SUM(cost), 0),
		       COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens
		                  + cache_write_5m_tokens + cache_write_1h_tokens), 0),
		       COUNT(*)
		FROM requests WHERE timestamp >= ? AND timestamp < ?`,
		startStr, endStr,
	).Scan(&bucket.Cost, &bucket.Tokens, &bucket.RequestCount); err != nil {
		return WindowBucket{}, err
	}
	if err := s.db.QueryRow(`
		SELECT COALESCE(SUM(cost), 0)
		FROM requests WHERE timestamp >= ? AND timestamp < ?`,
		prevStartStr, startStr,
	).Scan(&bucket.PrevCost); err != nil {
		return WindowBucket{}, err
	}
	return bucket, nil
}

// GetWindowBucket walks the request timestamps in order, opens a new rolling
// window of `duration` when a request arrives at-or-after the previous
// window's end, and returns the most-recent window's totals plus the prior
// window's cost (for trend comparisons).
func (s *Store) GetWindowBucket(duration time.Duration) (WindowBucket, error) {
	rows, err := s.db.Query(`
		SELECT timestamp, cost,
			input_tokens + output_tokens + cache_read_tokens
				+ cache_write_5m_tokens + cache_write_1h_tokens
		FROM requests ORDER BY timestamp ASC`)
	if err != nil {
		return WindowBucket{}, err
	}
	defer rows.Close()

	type win struct {
		start  time.Time
		cost   float64
		tokens int64
		count  int
	}
	var windows []win
	var currentEnd time.Time

	for rows.Next() {
		var tsStr string
		var cost float64
		var tokens int64
		if err := rows.Scan(&tsStr, &cost, &tokens); err != nil {
			return WindowBucket{}, err
		}
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			ts, err = time.Parse(time.RFC3339, tsStr)
			if err != nil {
				continue
			}
		}
		if currentEnd.IsZero() || !ts.Before(currentEnd) {
			windows = append(windows, win{start: ts})
			currentEnd = ts.Add(duration)
		}
		w := &windows[len(windows)-1]
		w.cost += cost
		w.tokens += tokens
		w.count++
	}

	if len(windows) == 0 {
		return WindowBucket{}, nil
	}
	cur := windows[len(windows)-1]
	out := WindowBucket{
		Start:        cur.start.UTC().Format(time.RFC3339Nano),
		End:          cur.start.Add(duration).UTC().Format(time.RFC3339Nano),
		Cost:         cur.cost,
		Tokens:       cur.tokens,
		RequestCount: cur.count,
	}
	if len(windows) >= 2 {
		prev := windows[len(windows)-2]
		out.PrevCost = prev.cost
		out.PrevStart = prev.start.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

func (s *Store) GetDailySummary(days int) ([]DailySpend, error) {
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")

	rows, err := s.db.Query(`
		SELECT DATE(last_activity, 'localtime') as day, SUM(total_cost)
		FROM sessions
		WHERE DATE(last_activity, 'localtime') >= ?
		GROUP BY day
		ORDER BY day ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Build a complete date range with zero-filled gaps
	result := make(map[string]float64)
	for rows.Next() {
		var day string
		var cost float64
		if err := rows.Scan(&day, &cost); err != nil {
			return nil, err
		}
		result[day] = cost
	}

	var daily []DailySpend
	for i := days; i >= 0; i-- {
		d := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		cost := result[d]
		daily = append(daily, DailySpend{Date: d, Cost: cost})
	}
	return daily, nil
}

func (s *Store) TopSessions(n int) ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, project, slug, model, started_at, last_activity,
		total_input, total_output, total_cache_read,
		total_cache_write_5m, total_cache_write_1h, total_cost
		FROM sessions ORDER BY total_cost DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.Project, &sess.Slug, &sess.Model,
			&sess.StartedAt, &sess.LastActivity,
			&sess.TotalInput, &sess.TotalOutput, &sess.TotalCacheRead,
			&sess.TotalCacheWrite5m, &sess.TotalCacheWrite1h,
			&sess.TotalCost); err != nil {
			return nil, err
		}
		sess.TotalCacheWrite = sess.TotalCacheWrite5m + sess.TotalCacheWrite1h
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

func (s *Store) RecentSessions(n int) ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, project, slug, model, started_at, last_activity,
		total_input, total_output, total_cache_read,
		total_cache_write_5m, total_cache_write_1h, total_cost
		FROM sessions ORDER BY last_activity DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.Project, &sess.Slug, &sess.Model,
			&sess.StartedAt, &sess.LastActivity,
			&sess.TotalInput, &sess.TotalOutput, &sess.TotalCacheRead,
			&sess.TotalCacheWrite5m, &sess.TotalCacheWrite1h,
			&sess.TotalCost); err != nil {
			return nil, err
		}
		sess.TotalCacheWrite = sess.TotalCacheWrite5m + sess.TotalCacheWrite1h
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

type ProjectSummary struct {
	Project        string  `json:"project"`
	SessionCount   int     `json:"session_count"`
	TotalCost      float64 `json:"total_cost"`
	TotalTokens    int64   `json:"total_tokens"`
	TotalInput     int64   `json:"total_input"`
	TotalOutput    int64   `json:"total_output"`
	TotalCacheRead int64   `json:"total_cache_read"`
	TotalCacheWrite int64  `json:"total_cache_write"`
	LastActivity   string  `json:"last_activity"`
}

type ProjectMonthly struct {
	Project string  `json:"project"`
	Month   string  `json:"month"`
	Cost    float64 `json:"cost"`
}

// ProjectGroup is one row in the grouped Sessions view: a project with its
// roll-up stats, optionally restricted to sessions active on a given local day.
type ProjectGroup struct {
	Project      string  `json:"project"`
	SessionCount int     `json:"session_count"`
	TotalCost    float64 `json:"total_cost"`
	TotalTokens  int64   `json:"total_tokens"`
	StartedAt    string  `json:"started_at"`    // MIN across child sessions
	LastActivity string  `json:"last_activity"` // MAX across child sessions
}

var projectGroupSortColumns = map[string]string{
	"cost":    "total_cost",
	"date":    "last_activity",
	"started": "started_at",
	"tokens":  "total_tokens",
	"project": "project",
}

// GetProjectGroups returns project-level rollups for the Sessions grouped view.
// When date is "" all projects are returned; otherwise only projects with at
// least one session whose local-day last_activity equals `date`. Roll-up totals
// are computed from the *full lifetime* of those matching sessions, matching
// the semantic the Daily Spend chart already uses.
func (s *Store) GetProjectGroups(date, sortBy, sortDir string) ([]ProjectGroup, error) {
	col, ok := projectGroupSortColumns[sortBy]
	if !ok {
		col = "last_activity"
	}
	dir := "DESC"
	if sortDir == "asc" {
		dir = "ASC"
	}

	whereClause := ""
	args := []any{}
	if date != "" {
		whereClause = "WHERE DATE(last_activity, 'localtime') = ?"
		args = append(args, date)
	}

	query := fmt.Sprintf(`
		SELECT project,
			COUNT(*) as session_count,
			SUM(total_cost) as total_cost,
			SUM(total_input + total_output + total_cache_read + total_cache_write_5m + total_cache_write_1h) as total_tokens,
			MIN(started_at) as started_at,
			MAX(last_activity) as last_activity
		FROM sessions
		%s
		GROUP BY project
		ORDER BY %s %s`, whereClause, col, dir)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []ProjectGroup
	for rows.Next() {
		var g ProjectGroup
		if err := rows.Scan(&g.Project, &g.SessionCount, &g.TotalCost, &g.TotalTokens,
			&g.StartedAt, &g.LastActivity); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, nil
}

func (s *Store) GetProjects() ([]ProjectSummary, error) {
	rows, err := s.db.Query(`
		SELECT project,
			COUNT(*) as session_count,
			SUM(total_cost) as total_cost,
			SUM(total_input + total_output + total_cache_read + total_cache_write_5m + total_cache_write_1h) as total_tokens,
			SUM(total_input) as total_input,
			SUM(total_output) as total_output,
			SUM(total_cache_read) as total_cache_read,
			SUM(total_cache_write_5m + total_cache_write_1h) as total_cache_write,
			MAX(last_activity) as last_activity
		FROM sessions
		GROUP BY project
		ORDER BY total_cost DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.Project, &p.SessionCount, &p.TotalCost, &p.TotalTokens,
			&p.TotalInput, &p.TotalOutput, &p.TotalCacheRead, &p.TotalCacheWrite,
			&p.LastActivity); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

// GetProjectsPrevMonth returns spend per project for the previous full local
// calendar month, descending by cost. Drives the "Spend by Project · last
// month" donut on the Overview.
func (s *Store) GetProjectsPrevMonth() ([]ProjectMonthly, error) {
	now := time.Now()
	prevMonthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	thisMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	monthLabel := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location()).Format("2006-01")

	rows, err := s.db.Query(`
		SELECT project, SUM(total_cost) as cost
		FROM sessions
		WHERE DATE(last_activity, 'localtime') >= ?
		  AND DATE(last_activity, 'localtime') < ?
		GROUP BY project
		HAVING cost > 0
		ORDER BY cost DESC`, prevMonthStart, thisMonthStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []ProjectMonthly
	for rows.Next() {
		var pm ProjectMonthly
		if err := rows.Scan(&pm.Project, &pm.Cost); err != nil {
			return nil, err
		}
		pm.Month = monthLabel
		data = append(data, pm)
	}
	return data, nil
}

func (s *Store) GetProjectMonthly() ([]ProjectMonthly, error) {
	rows, err := s.db.Query(`
		SELECT project,
			STRFTIME('%Y-%m', last_activity, 'localtime') as month,
			SUM(total_cost) as cost
		FROM sessions
		WHERE DATE(last_activity, 'localtime') >= DATE('now', '-6 months', 'localtime')
		GROUP BY project, month
		ORDER BY month ASC, cost DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []ProjectMonthly
	for rows.Next() {
		var pm ProjectMonthly
		if err := rows.Scan(&pm.Project, &pm.Month, &pm.Cost); err != nil {
			return nil, err
		}
		data = append(data, pm)
	}
	return data, nil
}

func (s *Store) GetTokenBreakdown() (input, output, cacheRead, cacheWrite int64, err error) {
	err = s.db.QueryRow(`
		SELECT COALESCE(SUM(total_input), 0),
		       COALESCE(SUM(total_output), 0),
		       COALESCE(SUM(total_cache_read), 0),
		       COALESCE(SUM(total_cache_write_5m + total_cache_write_1h), 0)
		FROM sessions`).Scan(&input, &output, &cacheRead, &cacheWrite)
	return
}

type CostByType struct {
	InputCost      float64 `json:"input_cost"`
	OutputCost     float64 `json:"output_cost"`
	CacheReadCost  float64 `json:"cache_read_cost"`
	CacheWriteCost float64 `json:"cache_write_cost"`
}

func (s *Store) GetCostBreakdown() (*CostByType, error) {
	rows, err := s.db.Query(`
		SELECT model, total_input, total_output, total_cache_read,
			total_cache_write_5m, total_cache_write_1h
		FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &CostByType{}
	for rows.Next() {
		var model string
		var inp, out, cr, cw5m, cw1h int64
		if err := rows.Scan(&model, &inp, &out, &cr, &cw5m, &cw1h); err != nil {
			return nil, err
		}
		cb := calculator.Calculate(model, calculator.TokenUsage{
			InputTokens:        inp,
			OutputTokens:       out,
			CacheReadTokens:    cr,
			CacheWrite5mTokens: cw5m,
			CacheWrite1hTokens: cw1h,
		})
		result.InputCost += cb.InputCost
		result.OutputCost += cb.OutputCost
		result.CacheReadCost += cb.CacheReadCost
		result.CacheWriteCost += cb.CacheWriteCost
	}
	return result, nil
}

// --- Feature: Model Usage Breakdown ---

type ModelSummary struct {
	Model        string  `json:"model"`
	Family       string  `json:"family"`
	SessionCount int     `json:"session_count"`
	TotalCost    float64 `json:"total_cost"`
	TotalTokens  int64   `json:"total_tokens"`
}

func (s *Store) GetModelBreakdown() ([]ModelSummary, error) {
	rows, err := s.db.Query(`
		SELECT model,
			COUNT(*) as session_count,
			SUM(total_cost) as total_cost,
			SUM(total_input + total_output + total_cache_read + total_cache_write_5m + total_cache_write_1h) as total_tokens
		FROM sessions
		WHERE model != ''
		GROUP BY model
		ORDER BY total_cost DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []ModelSummary
	for rows.Next() {
		var m ModelSummary
		if err := rows.Scan(&m.Model, &m.SessionCount, &m.TotalCost, &m.TotalTokens); err != nil {
			return nil, err
		}
		rates := calculator.GetRates(m.Model)
		m.Family = rates.Family
		results = append(results, m)
	}
	return results, nil
}

// --- Feature: Activity Heatmap ---

type HeatmapCell struct {
	Day  int     `json:"day"`  // 0=Sunday .. 6=Saturday
	Hour int     `json:"hour"` // 0..23
	Cost float64 `json:"cost"`
}

func (s *Store) GetActivityHeatmap() ([]HeatmapCell, error) {
	rows, err := s.db.Query(`
		SELECT CAST(STRFTIME('%w', last_activity, 'localtime') AS INTEGER) as dow,
			CAST(STRFTIME('%H', last_activity, 'localtime') AS INTEGER) as hour,
			SUM(total_cost) as cost
		FROM sessions
		WHERE last_activity != ''
		GROUP BY dow, hour
		ORDER BY dow, hour`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cells []HeatmapCell
	for rows.Next() {
		var c HeatmapCell
		if err := rows.Scan(&c.Day, &c.Hour, &c.Cost); err != nil {
			return nil, err
		}
		cells = append(cells, c)
	}
	return cells, nil
}

// --- Feature: Cost Velocity / Trend Comparison ---

type Trends struct {
	PrevDayCost   float64 `json:"prev_day_cost"`
	PrevMonthCost float64 `json:"prev_month_cost"`
}

func (s *Store) GetTrends() (*Trends, error) {
	now := time.Now()
	todayStr := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	prevMonthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	prevMonthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")

	t := &Trends{}

	// Previous day cost (yesterday) — local-day buckets so "yesterday" matches
	// the user's calendar regardless of where UTC midnight falls.
	s.db.QueryRow(`
		SELECT COALESCE(SUM(total_cost), 0)
		FROM sessions WHERE DATE(last_activity, 'localtime') >= ?
			AND DATE(last_activity, 'localtime') < ?`,
		yesterday, todayStr).Scan(&t.PrevDayCost)

	// Previous month cost
	s.db.QueryRow(`
		SELECT COALESCE(SUM(total_cost), 0)
		FROM sessions WHERE DATE(last_activity, 'localtime') >= ?
			AND DATE(last_activity, 'localtime') < ?`,
		prevMonthStart, prevMonthEnd).Scan(&t.PrevMonthCost)

	return t, nil
}
