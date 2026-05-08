export interface SpendBucket {
  cost: number
  tokens: number
}

export interface CostBreakdown {
  input_cost: number
  output_cost: number
  cache_read_cost: number
  cache_write_cost: number
}

export interface Trends {
  prev_day_cost: number
  prev_month_cost: number
}

export interface WindowBucket {
  start: string
  end: string
  cost: number
  tokens: number
  request_count: number
  prev_cost: number
  prev_start: string
  cap?: number | null
  // Upstream-reported utilization (0-100) for the active anchor's window.
  // When present, this is the authoritative signal for bar fill — preferred
  // over the cost/cap derivation, which can drift if the local request
  // ledger and the currently-authenticated account diverge (e.g. account
  // switch).
  pct?: number | null
  last_synced_at?: string | null
  // F2 S2.3 honest-state enum populated by usagestate.SummaryProvider on
  // the backend. One of: auto_fresh, auto_stale, token_expired,
  // provider_unavailable, manual_anchor, fallback_cascade, unknown.
  // Optional: missing on older backends or when augmentation hasn't run.
  state?: string
}

export interface WindowAnchor {
  id: number
  synced_at: string
  window_type: '5h' | '7d'
  time_left_minutes: number
  anthropic_pct?: number | null
  observed_cost: number
  inferred_cap?: number | null
}

export interface Summary {
  window_5h: WindowBucket
  today: SpendBucket
  window_7d: WindowBucket
  month: SpendBucket
  projected: number
  tokens: {
    input: number
    output: number
    cache_read: number
    cache_write: number
  }
  trends: Trends
  budget: number
}

export interface ModelSummary {
  model: string
  family: string
  session_count: number
  total_cost: number
  total_tokens: number
}

export interface HeatmapCell {
  day: number
  hour: number
  cost: number
}

export interface RequestRecord {
  request_id: string
  session_id: string
  timestamp: string
  model: string
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_5m_tokens: number
  cache_write_1h_tokens: number
  cache_write_tokens: number // derived: 5m + 1h
  cost: number
}

export interface Session {
  id: string
  project: string
  slug: string
  model: string
  started_at: string
  last_activity: string
  total_input: number
  total_output: number
  total_cache_read: number
  total_cache_write_5m: number
  total_cache_write_1h: number
  total_cache_write: number // derived: 5m + 1h
  total_cost: number
}

export interface DailySpend {
  date: string
  cost: number
}

export interface SessionsResponse {
  sessions: Session[]
  total: number
  limit: number
  offset: number
  date?: string
  project?: string
}

export interface ProjectGroup {
  project: string
  session_count: number
  total_cost: number
  total_tokens: number
  started_at: string
  last_activity: string
}

export interface ProjectGroupsResponse {
  groups: ProjectGroup[]
  total: number
  date?: string
}

// Day drilldown — request-day spend for a local YYYY-MM-DD. Mirrors the
// Go-side store.DayDrilldown / DayProjectGroup / DaySessionRow shape from
// internal/store/drilldown.go. day_cost / day_tokens / day_request_count are
// scoped to requests with timestamp on the given local day; lifetime_cost on
// each session is the session's full sessions.total_cost (separate from the
// day-scoped sums, never folded into them).
export interface DayProjectGroup {
  project: string
  day_cost: number
  day_tokens: number
  session_count: number
  day_request_count: number
}

export interface DaySessionRow {
  id: string
  project: string
  slug: string
  model: string
  started_at: string
  last_activity: string
  day_cost: number
  day_tokens: number
  day_request_count: number
  lifetime_cost: number
}

export interface DayDrilldown {
  date: string
  projects: DayProjectGroup[]
  sessions: DaySessionRow[]
}

export interface Settings {
  log_dir: string
  db_path: string
  port: number
  monthly_budget_usd: number
  open_browser_on_serve: boolean
  claude_plan: string
}

export interface ModelRate {
  Family: string
  Released: string
  InputPerMToken: number
  OutputPerMToken: number
  CacheReadPerMToken: number
  CacheWrite5mPerMToken: number
  CacheWrite1hPerMToken: number
}

export interface RatesResponse {
  version: string
  updated: string
  rates: ModelRate[]
}

export interface ProjectSummary {
  project: string
  session_count: number
  total_cost: number
  total_tokens: number
  total_input: number
  total_output: number
  total_cache_read: number
  total_cache_write: number
  last_activity: string
}

export interface ProjectMonthly {
  project: string
  month: string
  cost: number
}

export interface WsEvent {
  type: 'session.updated' | 'session.created' | 'summary.updated' | 'ping'
  payload: any
}

export type ConnectionStatus = 'connected' | 'reconnecting' | 'offline'
