package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/ksred/cctrack/internal/calculator"
	"github.com/ksred/cctrack/internal/config"
	"github.com/ksred/cctrack/internal/hub"
	"github.com/ksred/cctrack/internal/store"
)

type API struct {
	store *store.Store
	hub   *hub.Hub
	cfg   *config.Config
}

func New(s *store.Store, h *hub.Hub, cfg *config.Config) *API {
	return &API{store: s, hub: h, cfg: cfg}
}

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/summary", a.handleSummary)
	mux.HandleFunc("GET /api/v1/sessions", a.handleSessions)
	mux.HandleFunc("GET /api/v1/sessions/grouped", a.handleSessionsGrouped)
	mux.HandleFunc("POST /api/v1/window-anchors", a.handlePostWindowAnchor)
	mux.HandleFunc("GET /api/v1/window-anchors", a.handleListWindowAnchors)
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.handleSession)
	mux.HandleFunc("GET /api/v1/recent", a.handleRecent)
	mux.HandleFunc("GET /api/v1/daily", a.handleDaily)
	mux.HandleFunc("GET /api/v1/settings", a.handleGetSettings)
	mux.HandleFunc("POST /api/v1/settings", a.handlePostSettings)
	mux.HandleFunc("GET /api/v1/projects", a.handleProjects)
	mux.HandleFunc("GET /api/v1/projects/monthly", a.handleProjectMonthly)
	mux.HandleFunc("GET /api/v1/projects/prev-month", a.handleProjectsPrevMonth)
	mux.HandleFunc("GET /api/v1/rates", a.handleRates)
	mux.HandleFunc("GET /api/v1/models", a.handleModels)
	mux.HandleFunc("GET /api/v1/heatmap", a.handleHeatmap)
	mux.HandleFunc("GET /api/v1/sessions/{id}/requests", a.handleSessionRequests)
	mux.HandleFunc("GET /api/v1/ws", a.handleWS)
}

func (a *API) handleSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := a.store.GetSummary()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	input, output, cacheRead, cacheWrite, err := a.store.GetTokenBreakdown()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	costBreakdown, err := a.store.GetCostBreakdown()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	trends, err := a.store.GetTrends()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	resp := map[string]any{
		"window_5h": summary.Window5h,
		"today":     summary.Today,
		"window_7d": summary.Window7d,
		"month":     summary.Month,
		"projected": summary.Projected,
		"tokens": map[string]int64{
			"input":       input,
			"output":      output,
			"cache_read":  cacheRead,
			"cache_write": cacheWrite,
		},
		"cost_breakdown": costBreakdown,
		"trends":         trends,
		"budget":         a.cfg.MonthlyBudgetUSD,
	}
	writeJSON(w, resp)
}

func (a *API) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 25)
	offset := queryInt(r, "offset", 0)
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "cost"
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "desc"
	}
	date := r.URL.Query().Get("date")       // YYYY-MM-DD (local day) or empty
	project := r.URL.Query().Get("project") // exact project match or empty

	sessions, total, err := a.store.ListSessions(limit, offset, sort, dir, date, project)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{
		"sessions": sessions,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"date":     date,
		"project":  project,
	})
}

func (a *API) handlePostWindowAnchor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WindowType      string   `json:"window_type"`
		TimeLeftMinutes int      `json:"time_left_minutes"`
		AnthropicPct    *float64 `json:"anthropic_pct,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if body.WindowType != "5h" && body.WindowType != "7d" {
		http.Error(w, "window_type must be '5h' or '7d'", 400)
		return
	}
	if body.TimeLeftMinutes < 0 {
		http.Error(w, "time_left_minutes must be >= 0", 400)
		return
	}

	// Compute observed_cost over the window the *user is describing*, not
	// cctrack's cascading detection. The cascading detector picks its own
	// window boundaries from the request stream, which can diverge from
	// Anthropic's actual window — so dividing the wrong cost by the user's
	// pct yields a wildly wrong cap (off by a factor of 5–50× in practice).
	// Anchored bounds: [now-(duration-time_left), now].
	duration := 5 * time.Hour
	if body.WindowType == "7d" {
		duration = 7 * 24 * time.Hour
	}
	now := time.Now()
	anchoredEnd := now.Add(time.Duration(body.TimeLeftMinutes) * time.Minute)
	windowStart := anchoredEnd.Add(-duration)
	observed, err := a.store.CostInRange(windowStart, now)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	anchor := store.WindowAnchor{
		WindowType:      body.WindowType,
		TimeLeftMinutes: body.TimeLeftMinutes,
		AnthropicPct:    body.AnthropicPct,
		ObservedCost:    observed,
	}
	id, err := a.store.SaveWindowAnchor(anchor)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	saved, _ := a.store.GetLatestAnchor(body.WindowType)
	writeJSON(w, map[string]any{
		"id":     id,
		"anchor": saved,
	})
}

func (a *API) handleListWindowAnchors(w http.ResponseWriter, r *http.Request) {
	wt := r.URL.Query().Get("type")
	limit := queryInt(r, "limit", 50)
	if wt != "5h" && wt != "7d" {
		http.Error(w, "type must be '5h' or '7d'", 400)
		return
	}
	rows, err := a.store.ListAnchors(wt, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"anchors": rows})
}

func (a *API) handleSessionsGrouped(w http.ResponseWriter, r *http.Request) {
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "date"
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "desc"
	}
	date := r.URL.Query().Get("date") // YYYY-MM-DD (local day) or empty

	groups, err := a.store.GetProjectGroups(date, sort, dir)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{
		"groups": groups,
		"total":  len(groups),
		"date":   date,
	})
}

func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := a.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", 404)
		return
	}
	writeJSON(w, sess)
}

func (a *API) handleRecent(w http.ResponseWriter, r *http.Request) {
	n := queryInt(r, "n", 10)
	sessions, err := a.store.RecentSessions(n)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, sessions)
}

func (a *API) handleDaily(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	daily, err := a.store.GetDailySummary(days)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, daily)
}

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.cfg)
}

func (a *API) handlePostSettings(w http.ResponseWriter, r *http.Request) {
	var updates struct {
		MonthlyBudgetUSD   *float64 `json:"monthly_budget_usd"`
		OpenBrowserOnServe *bool    `json:"open_browser_on_serve"`
		LogDir             *string  `json:"log_dir"`
		ClaudePlan         *string  `json:"claude_plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	if updates.MonthlyBudgetUSD != nil {
		a.cfg.MonthlyBudgetUSD = *updates.MonthlyBudgetUSD
	}
	if updates.OpenBrowserOnServe != nil {
		a.cfg.OpenBrowserOnServe = *updates.OpenBrowserOnServe
	}
	if updates.LogDir != nil {
		a.cfg.LogDir = *updates.LogDir
	}
	if updates.ClaudePlan != nil {
		a.cfg.ClaudePlan = *updates.ClaudePlan
	}

	if err := a.cfg.Save(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, a.cfg)
}

func (a *API) handleProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := a.store.GetProjects()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, projects)
}

func (a *API) handleProjectMonthly(w http.ResponseWriter, r *http.Request) {
	data, err := a.store.GetProjectMonthly()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, data)
}

func (a *API) handleProjectsPrevMonth(w http.ResponseWriter, r *http.Request) {
	data, err := a.store.GetProjectsPrevMonth()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, data)
}

func (a *API) handleRates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"version": calculator.RatesVersion,
		"updated": calculator.RatesUpdated,
		"rates":   calculator.Rates,
	})
}

func (a *API) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.store.GetModelBreakdown()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, models)
}

func (a *API) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	cells, err := a.store.GetActivityHeatmap()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, cells)
}

func (a *API) handleSessionRequests(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	requests, err := a.store.GetSessionRequests(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, requests)
}

func (a *API) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // local-only server
	})
	if err != nil {
		log.Printf("WebSocket accept error: %v", err)
		return
	}

	// Send initial summary snapshot
	summary, err := a.store.GetSummary()
	if err == nil {
		payload, _ := json.Marshal(summary)
		event := hub.Event{Type: "summary.updated", Payload: payload}
		data, _ := json.Marshal(event)
		conn.Write(r.Context(), websocket.MessageText, data)
	}

	a.hub.HandleConnection(conn)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
