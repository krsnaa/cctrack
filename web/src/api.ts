import type { Summary, SessionsResponse, Session, DailySpend, Settings, RatesResponse, ProjectSummary, ProjectMonthly, ProjectGroupsResponse, ModelSummary, HeatmapCell, RequestRecord, WindowAnchor } from './types'

const BASE = '/api/v1'

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`)
  if (!res.ok) throw new Error(`API error: ${res.status}`)
  return res.json()
}

export async function fetchSummary(): Promise<Summary> {
  return get<Summary>('/summary')
}

export async function fetchSessions(
  limit = 25,
  offset = 0,
  sort = 'cost',
  dir = 'desc',
  date = '',
  project = '',
): Promise<SessionsResponse> {
  const datePart = date ? `&date=${encodeURIComponent(date)}` : ''
  const projectPart = project ? `&project=${encodeURIComponent(project)}` : ''
  return get<SessionsResponse>(
    `/sessions?limit=${limit}&offset=${offset}&sort=${sort}&dir=${dir}${datePart}${projectPart}`,
  )
}

export async function fetchSessionsGrouped(
  sort = 'date',
  dir = 'desc',
  date = '',
): Promise<ProjectGroupsResponse> {
  const datePart = date ? `&date=${encodeURIComponent(date)}` : ''
  return get<ProjectGroupsResponse>(`/sessions/grouped?sort=${sort}&dir=${dir}${datePart}`)
}

export async function fetchSession(id: string): Promise<Session> {
  return get<Session>(`/sessions/${id}`)
}

export async function fetchRecent(n = 10): Promise<Session[]> {
  return get<Session[]>(`/recent?n=${n}`)
}

export async function fetchDaily(days = 30): Promise<DailySpend[]> {
  return get<DailySpend[]>(`/daily?days=${days}`)
}

export async function fetchSettings(): Promise<Settings> {
  return get<Settings>('/settings')
}

export async function updateSettings(data: Partial<Settings>): Promise<Settings> {
  const res = await fetch(`${BASE}/settings`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(data),
  })
  if (!res.ok) throw new Error(`API error: ${res.status}`)
  return res.json()
}

export async function fetchProjects(): Promise<ProjectSummary[]> {
  return get<ProjectSummary[]>('/projects')
}

export async function fetchProjectMonthly(): Promise<ProjectMonthly[]> {
  return get<ProjectMonthly[]>('/projects/monthly')
}

export async function fetchProjectsPrevMonth(): Promise<ProjectMonthly[]> {
  return get<ProjectMonthly[]>('/projects/prev-month')
}

export async function fetchRates(): Promise<RatesResponse> {
  return get<RatesResponse>('/rates')
}

export async function fetchModels(): Promise<ModelSummary[]> {
  return get<ModelSummary[]>('/models')
}

export async function fetchHeatmap(): Promise<HeatmapCell[]> {
  return get<HeatmapCell[]>('/heatmap')
}

export async function fetchSessionRequests(sessionId: string): Promise<RequestRecord[]> {
  return get<RequestRecord[]>(`/sessions/${sessionId}/requests`)
}

export async function fetchWindowAnchors(type: '5h' | '7d', limit = 50): Promise<{ anchors: WindowAnchor[] }> {
  return get<{ anchors: WindowAnchor[] }>(`/window-anchors?type=${type}&limit=${limit}`)
}

export async function postWindowAnchor(
  windowType: '5h' | '7d',
  timeLeftMinutes: number,
  anthropicPct?: number,
): Promise<{ id: number; anchor: WindowAnchor }> {
  const body: Record<string, unknown> = {
    window_type: windowType,
    time_left_minutes: timeLeftMinutes,
  }
  if (anthropicPct !== undefined && anthropicPct !== null) {
    body.anthropic_pct = anthropicPct
  }
  const res = await fetch(`${BASE}/window-anchors`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) throw new Error(`API error: ${res.status}`)
  return res.json()
}
