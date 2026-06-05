import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Session, ProjectGroup, DayDrilldown } from '../types'
import { fetchSessions, fetchSessionsGrouped, fetchSession, fetchDayDrilldown } from '../api'

// Sessions view shows projects grouped by working directory. Children expand
// inline on demand and are cached per-project so collapsing/re-expanding is
// instant.
//
// Two data-source modes share the same store shape:
//
//   • No date filter (Invariant B path):
//       loadGroups()  → fetchSessionsGrouped()       → /sessions/grouped
//       loadChildren  → fetchSessions(...,date='')   → /sessions
//     Both surface session-lifetime rollups, the canonical browse view.
//
//   • Date filter active (request-day path, F1 fix):
//       loadGroups()  → fetchDayDrilldown(date)      → /day-drilldown?date=D
//     The drilldown response carries BOTH project groups and per-session day
//     totals in one payload; loadGroups pre-populates childSessions, so
//     loadChildren becomes a no-op for already-cached projects. day_cost /
//     day_tokens are stored in total_cost / total_tokens of the existing
//     ProjectGroup / Session shapes — interpretation shifts but the table
//     columns ("Cost", "Tokens") still read correctly.
type ErrorKind = 'invalid_date' | 'network' | 'unknown' | null

const groupSortKeys: Record<string, keyof ProjectGroup> = {
  cost: 'total_cost',
  date: 'last_activity',
  started: 'started_at',
  tokens: 'total_tokens',
  project: 'project',
}

function sortGroups(groups: ProjectGroup[], sortBy: string, sortDir: 'asc' | 'desc'): ProjectGroup[] {
  const key = groupSortKeys[sortBy] ?? 'last_activity'
  const factor = sortDir === 'desc' ? -1 : 1
  return [...groups].sort((a, b) => {
    const av = a[key] as string | number
    const bv = b[key] as string | number
    if (av === bv) return 0
    return av < bv ? -1 * factor : 1 * factor
  })
}

// Token total shown in the Sessions table (and the same definition the backend
// sorts "tokens" by): input + output + cache_read + cache_write (5m + 1h).
function sessionTotalTokens(s: Session): number {
  return s.total_input + s.total_output + s.total_cache_read + s.total_cache_write
}

const sessionSortValues: Record<string, (s: Session) => string | number> = {
  cost: s => s.total_cost,
  date: s => s.last_activity,
  started: s => s.started_at,
  tokens: sessionTotalTokens,
  project: s => s.project,
}

// sortSessions orders a project's child sessions by the same column/direction
// the table header drives, so expanding a project (or toggling the sort while
// expanded) keeps the inner rows in sync with the parent group ordering.
function sortSessions(sessions: Session[], sortBy: string, sortDir: 'asc' | 'desc'): Session[] {
  const valueOf = sessionSortValues[sortBy] ?? sessionSortValues.date
  const factor = sortDir === 'desc' ? -1 : 1
  return [...sessions].sort((a, b) => {
    const av = valueOf(a)
    const bv = valueOf(b)
    if (av === bv) return 0
    return av < bv ? -1 * factor : 1 * factor
  })
}

// mapDrilldownToGroups adapts the wire shape (DayDrilldown) into the existing
// store shape (ProjectGroup[] + Session[] keyed by project). Field mapping:
//   • day_cost   → total_cost
//   • day_tokens → total_tokens (project) / total_input (session, so the
//     per-session-row display sums to day_tokens — SessionRow computes
//     total_input + total_output + total_cache_* and we want that sum to
//     equal the day's tokens for that session, not lifetime)
//   • started_at / last_activity on the project group are min/max across
//     the session rows that landed on day D (lifetime values, surfaced as-is
//     so the column meaning matches the column label)
function mapDrilldownToGroups(dd: DayDrilldown): {
  groups: ProjectGroup[]
  sessionsByProject: Map<string, Session[]>
} {
  const sessionsByProject = new Map<string, Session[]>()
  for (const dsr of dd.sessions) {
    const sess: Session = {
      id: dsr.id,
      project: dsr.project,
      slug: dsr.slug,
      model: dsr.model,
      started_at: dsr.started_at,
      last_activity: dsr.last_activity,
      total_input: dsr.day_tokens,
      total_output: 0,
      total_cache_read: 0,
      total_cache_write_5m: 0,
      total_cache_write_1h: 0,
      total_cache_write: 0,
      total_cost: dsr.day_cost,
    }
    const list = sessionsByProject.get(dsr.project) ?? []
    list.push(sess)
    sessionsByProject.set(dsr.project, list)
  }

  const groups: ProjectGroup[] = dd.projects.map(p => {
    const projectSessions = sessionsByProject.get(p.project) ?? []
    let earliestStart = ''
    let latestActivity = ''
    for (const s of projectSessions) {
      if (!earliestStart || s.started_at < earliestStart) earliestStart = s.started_at
      if (!latestActivity || s.last_activity > latestActivity) latestActivity = s.last_activity
    }
    return {
      project: p.project,
      session_count: p.session_count,
      total_cost: p.day_cost,
      total_tokens: p.day_tokens,
      started_at: earliestStart,
      last_activity: latestActivity,
    }
  })

  return { groups, sessionsByProject }
}

export const useSessionsStore = defineStore('sessions', () => {
  const groups = ref<ProjectGroup[]>([])
  const total = ref(0)
  const sortBy = ref('date')
  const sortDir = ref<'asc' | 'desc'>('desc')
  const dateFilter = ref('') // YYYY-MM-DD (local day) or '' for no filter

  const expanded = ref<Set<string>>(new Set())
  const childSessions = ref<Map<string, Session[]>>(new Map())
  const childLoading = ref<Set<string>>(new Set())

  const selectedSession = ref<Session | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)
  const errorKind = ref<ErrorKind>(null)

  function classifyError(msg: string): ErrorKind {
    if (/\b400\b/.test(msg)) return 'invalid_date'
    if (msg.includes('Failed to fetch') || msg.toLowerCase().includes('network')) return 'network'
    return 'unknown'
  }

  async function loadGroups() {
    loading.value = true
    error.value = null
    errorKind.value = null
    try {
      if (dateFilter.value) {
        const dd = await fetchDayDrilldown(dateFilter.value)
        const mapped = mapDrilldownToGroups(dd)
        groups.value = sortGroups(mapped.groups, sortBy.value, sortDir.value)
        total.value = mapped.groups.length
        // Pre-populate the per-project child cache from the same payload —
        // saves a second round-trip on group expand and guarantees children
        // agree with the parent rollup (same response, same date predicate).
        // Order each project's sessions by the active sort so the inner rows
        // match the column the user is sorting by.
        const sorted = new Map<string, Session[]>()
        for (const [proj, sess] of mapped.sessionsByProject) {
          sorted.set(proj, sortSessions(sess, sortBy.value, sortDir.value))
        }
        childSessions.value = sorted
      } else {
        const res = await fetchSessionsGrouped(sortBy.value, sortDir.value, '')
        groups.value = res.groups || []
        total.value = res.total
        // Re-order (rather than discard) any already-expanded project's cached
        // sessions so toggling the sort header re-sorts the inner rows in place
        // instead of blanking them until the next expand.
        const resorted = new Map<string, Session[]>()
        for (const [proj, sess] of childSessions.value) {
          resorted.set(proj, sortSessions(sess, sortBy.value, sortDir.value))
        }
        childSessions.value = resorted
      }
    } catch (e: any) {
      const msg = e?.message || 'Failed to load sessions'
      error.value = msg
      errorKind.value = classifyError(msg)
      groups.value = []
      total.value = 0
      childSessions.value = new Map()
    } finally {
      loading.value = false
    }
  }

  async function loadChildren(project: string) {
    if (childSessions.value.has(project) || childLoading.value.has(project)) return
    if (dateFilter.value) {
      // Date-filtered mode pre-populates childSessions in loadGroups; a missing
      // entry here means this project has no day-D sessions. Cache empty array
      // so the row toggles cleanly without trying to fetch.
      childSessions.value = new Map(childSessions.value).set(project, [])
      return
    }
    childLoading.value.add(project)
    try {
      // Pull all sessions for this project in the current date slice. The
      // backend default limit is 25; widen so a heavy project shows them all
      // without inner pagination on the expanded section. Fetch (and re-sort
      // client-side) by the active sort so the inner rows follow the column the
      // user picked rather than always falling back to most-recent-first.
      const res = await fetchSessions(500, 0, sortBy.value, sortDir.value, '', project)
      childSessions.value = new Map(childSessions.value).set(
        project,
        sortSessions(res.sessions || [], sortBy.value, sortDir.value),
      )
    } finally {
      childLoading.value.delete(project)
    }
  }

  function toggleExpand(project: string) {
    const next = new Set(expanded.value)
    if (next.has(project)) {
      next.delete(project)
    } else {
      next.add(project)
      loadChildren(project)
    }
    expanded.value = next
  }

  function setSort(col: string) {
    if (sortBy.value === col) {
      sortDir.value = sortDir.value === 'desc' ? 'asc' : 'desc'
    } else {
      sortBy.value = col
      sortDir.value = 'desc'
    }
    loadGroups()
  }

  function setDateFilter(date: string) {
    dateFilter.value = date
    expanded.value = new Set()
    loadGroups()
  }

  async function selectSession(id: string) {
    // Slide-over always shows full-lifetime detail regardless of how the
    // session was reached — drilldown's day-scoped numbers wouldn't make
    // sense in the per-session detail view.
    selectedSession.value = await fetchSession(id)
  }

  function clearSelection() {
    selectedSession.value = null
  }

  return {
    groups, total, sortBy, sortDir, dateFilter,
    expanded, childSessions, childLoading,
    selectedSession, loading,
    error, errorKind,
    loadGroups, loadChildren, toggleExpand,
    setSort, setDateFilter, selectSession, clearSelection,
  }
})
