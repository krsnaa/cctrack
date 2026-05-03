import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Session, ProjectGroup } from '../types'
import { fetchSessions, fetchSessionsGrouped, fetchSession } from '../api'

// Sessions view shows projects grouped by working directory. Children expand
// inline on demand and are cached per-project so collapsing/re-expanding is
// instant. The same date filter applies to both levels of the view.
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

  async function loadGroups() {
    loading.value = true
    try {
      const res = await fetchSessionsGrouped(sortBy.value, sortDir.value, dateFilter.value)
      groups.value = res.groups || []
      total.value = res.total
      // Filter changes invalidate the per-project cache because the child set
      // depends on the date filter too.
      childSessions.value = new Map()
    } finally {
      loading.value = false
    }
  }

  async function loadChildren(project: string) {
    if (childSessions.value.has(project) || childLoading.value.has(project)) return
    childLoading.value.add(project)
    try {
      // Pull all sessions for this project in the current date slice. The
      // backend default limit is 25; widen so a heavy project shows them all
      // without inner pagination on the expanded section.
      const res = await fetchSessions(500, 0, 'date', 'desc', dateFilter.value, project)
      childSessions.value = new Map(childSessions.value).set(project, res.sessions || [])
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
    selectedSession.value = await fetchSession(id)
  }

  function clearSelection() {
    selectedSession.value = null
  }

  return {
    groups, total, sortBy, sortDir, dateFilter,
    expanded, childSessions, childLoading,
    selectedSession, loading,
    loadGroups, loadChildren, toggleExpand,
    setSort, setDateFilter, selectSession, clearSelection,
  }
})
