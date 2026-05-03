package store

import (
	"database/sql"
	"time"
)

// WindowAnchor captures one user-driven sync against Anthropic's UI. Going
// forward, the latest anchor of each window_type overrides the cascading
// window detection so cctrack's window boundaries match what the user sees on
// claude.ai.
//
// AnthropicPct and InferredCap are optional — users who only want the timing
// fix can sync with just the time-left value. When AnthropicPct is provided,
// InferredCap is computed at insert time as observed_cost / (pct/100), giving
// a personal effective cap (cost-per-window for this user's surface mix).
type WindowAnchor struct {
	ID                int64    `json:"id"`
	SyncedAt          string   `json:"synced_at"` // ISO 8601 UTC
	WindowType        string   `json:"window_type"` // "5h" | "7d"
	TimeLeftMinutes   int      `json:"time_left_minutes"`
	AnthropicPct      *float64 `json:"anthropic_pct,omitempty"`
	ObservedCost      float64  `json:"observed_cost"`
	InferredCap       *float64 `json:"inferred_cap,omitempty"`
}

// SaveWindowAnchor inserts a new sync event. observed_cost is the cost
// cctrack has tracked in the *current* rolling window at the moment the
// user pressed sync — caller is responsible for computing it before calling.
func (s *Store) SaveWindowAnchor(a WindowAnchor) (int64, error) {
	if a.SyncedAt == "" {
		a.SyncedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	var pct, cap any = nil, nil
	if a.AnthropicPct != nil {
		pct = *a.AnthropicPct
		if *a.AnthropicPct > 0 {
			c := a.ObservedCost / (*a.AnthropicPct / 100)
			cap = c
		}
	}
	res, err := s.db.Exec(`
		INSERT INTO window_anchors
			(synced_at, window_type, time_left_minutes, anthropic_pct, observed_cost, inferred_cap)
		VALUES (?, ?, ?, ?, ?, ?)
	`, a.SyncedAt, a.WindowType, a.TimeLeftMinutes, pct, a.ObservedCost, cap)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetLatestAnchor returns the most recent anchor for a window_type, or nil
// if none exist. Caller checks staleness — if the anchor's window has
// already fully elapsed, the cascading detection should take over.
func (s *Store) GetLatestAnchor(windowType string) (*WindowAnchor, error) {
	row := s.db.QueryRow(`
		SELECT id, synced_at, window_type, time_left_minutes,
			anthropic_pct, observed_cost, inferred_cap
		FROM window_anchors
		WHERE window_type = ?
		ORDER BY synced_at DESC LIMIT 1`, windowType)
	a := &WindowAnchor{}
	var pct, cap sql.NullFloat64
	if err := row.Scan(&a.ID, &a.SyncedAt, &a.WindowType, &a.TimeLeftMinutes,
		&pct, &a.ObservedCost, &cap); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if pct.Valid {
		v := pct.Float64
		a.AnthropicPct = &v
	}
	if cap.Valid {
		v := cap.Float64
		a.InferredCap = &v
	}
	return a, nil
}

// GetLatestCap returns the most recent non-null inferred cap for a
// window_type, regardless of whether that anchor's window is still active.
// Caps are properties of the user's *plan* (and surface mix), not of any
// single window — so even after an anchor's timing has elapsed, its cap
// estimate remains the best guess until the user syncs again with a fresh
// percentage. Returns nil if no anchor with a cap exists yet.
func (s *Store) GetLatestCap(windowType string) (*float64, error) {
	row := s.db.QueryRow(`
		SELECT inferred_cap
		FROM window_anchors
		WHERE window_type = ? AND inferred_cap IS NOT NULL
		ORDER BY synced_at DESC LIMIT 1`, windowType)
	var cap sql.NullFloat64
	if err := row.Scan(&cap); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !cap.Valid {
		return nil, nil
	}
	v := cap.Float64
	return &v, nil
}

// ListAnchors returns the N most recent anchors for a window_type, newest
// first. Drives any future "cap-over-time" history view.
func (s *Store) ListAnchors(windowType string, limit int) ([]WindowAnchor, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, synced_at, window_type, time_left_minutes,
			anthropic_pct, observed_cost, inferred_cap
		FROM window_anchors
		WHERE window_type = ?
		ORDER BY synced_at DESC LIMIT ?`, windowType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WindowAnchor
	for rows.Next() {
		var a WindowAnchor
		var pct, cap sql.NullFloat64
		if err := rows.Scan(&a.ID, &a.SyncedAt, &a.WindowType, &a.TimeLeftMinutes,
			&pct, &a.ObservedCost, &cap); err != nil {
			return nil, err
		}
		if pct.Valid {
			v := pct.Float64
			a.AnthropicPct = &v
		}
		if cap.Valid {
			v := cap.Float64
			a.InferredCap = &v
		}
		out = append(out, a)
	}
	return out, nil
}
