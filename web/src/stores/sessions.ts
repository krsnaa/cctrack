import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Session } from '../types'
import { fetchSessions, fetchSession } from '../api'

export const useSessionsStore = defineStore('sessions', () => {
  const sessions = ref<Session[]>([])
  const total = ref(0)
  const limit = ref(25)
  const offset = ref(0)
  // Default sort by most-recent activity — surfacing today's work on /sessions
  // is more useful than ranking by all-time cost.
  const sortBy = ref('date')
  const sortDir = ref<'asc' | 'desc'>('desc')
  const selectedSession = ref<Session | null>(null)
  const loading = ref(false)

  async function load() {
    loading.value = true
    try {
      const res = await fetchSessions(limit.value, offset.value, sortBy.value, sortDir.value)
      sessions.value = res.sessions || []
      total.value = res.total
    } finally {
      loading.value = false
    }
  }

  function setSort(col: string) {
    if (sortBy.value === col) {
      sortDir.value = sortDir.value === 'desc' ? 'asc' : 'desc'
    } else {
      sortBy.value = col
      sortDir.value = 'desc'
    }
    offset.value = 0
    load()
  }

  function nextPage() {
    if (offset.value + limit.value < total.value) {
      offset.value += limit.value
      load()
    }
  }

  function prevPage() {
    if (offset.value > 0) {
      offset.value = Math.max(0, offset.value - limit.value)
      load()
    }
  }

  async function selectSession(id: string) {
    selectedSession.value = await fetchSession(id)
  }

  function clearSelection() {
    selectedSession.value = null
  }

  return {
    sessions, total, limit, offset, sortBy, sortDir,
    selectedSession, loading,
    load, setSort, nextPage, prevPage, selectSession, clearSelection,
  }
})
