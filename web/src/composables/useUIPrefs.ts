import { ref, watch } from 'vue'

// Module-scoped, shared across the whole app and persisted to localStorage so
// the sidebar state (and any future UI toggles) survives reloads.
const STORAGE_KEY = 'cctrack.ui.sidebarCollapsed'

function readInitial(): boolean {
  if (typeof localStorage === 'undefined') return false
  return localStorage.getItem(STORAGE_KEY) === '1'
}

const sidebarCollapsed = ref(readInitial())

watch(sidebarCollapsed, (v) => {
  if (typeof localStorage !== 'undefined') {
    localStorage.setItem(STORAGE_KEY, v ? '1' : '0')
  }
})

export function useUIPrefs() {
  return {
    sidebarCollapsed,
    toggleSidebar() {
      sidebarCollapsed.value = !sidebarCollapsed.value
    },
  }
}
