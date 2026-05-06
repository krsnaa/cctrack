import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { Summary, Session, WsEvent } from '../types'
import { fetchSummary, fetchRecent, fetchSessions } from '../api'

export const useDashboardStore = defineStore('dashboard', () => {
  const summary = ref<Summary | null>(null)
  const recentSessions = ref<Session[]>([])
  const topSessions = ref<Session[]>([])
  const loaded = ref(false)

  // Daily spend is owned by DailySpendChart itself so it can refetch when the
  // user changes the time-range dropdown without going through the store.
  async function load() {
    const [s, recent, top] = await Promise.all([
      fetchSummary(),
      fetchRecent(10),
      fetchSessions(5, 0, 'cost', 'desc'),
    ])
    summary.value = s
    recentSessions.value = recent || []
    topSessions.value = top.sessions || []
    loaded.value = true
  }

  // Summary-only refresh used by surfaces (e.g. the window-bar re-sync
  // button) that just need the bucket.state honest-state classification
  // to redraw after a backend status change. Narrower than load() so an
  // unrelated endpoint failure can't fail the refresh, and so we don't
  // refetch recent/top sessions on every manual sync.
  async function refreshSummary() {
    summary.value = await fetchSummary()
  }

  function applyEvent(event: WsEvent) {
    switch (event.type) {
      case 'summary.updated':
        if (event.payload) {
          summary.value = {
            ...summary.value,
            ...event.payload,
          } as Summary
        }
        break

      case 'session.updated':
        if (event.payload) {
          // Update in recent sessions
          const rIdx = recentSessions.value.findIndex(s => s.id === event.payload.id)
          if (rIdx >= 0) {
            recentSessions.value[rIdx] = event.payload
          }
          // Update in top sessions
          const tIdx = topSessions.value.findIndex(s => s.id === event.payload.id)
          if (tIdx >= 0) {
            topSessions.value[tIdx] = event.payload
          }
        }
        break

      case 'session.created':
        if (event.payload) {
          recentSessions.value.unshift(event.payload)
          if (recentSessions.value.length > 10) {
            recentSessions.value.pop()
          }
        }
        break

      case 'ping':
        break
    }
  }

  return { summary, recentSessions, topSessions, loaded, load, refreshSummary, applyEvent }
})
