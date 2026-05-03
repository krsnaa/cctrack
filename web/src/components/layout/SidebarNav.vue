<template>
  <aside class="sidebar" :class="{ collapsed: sidebarCollapsed }">
    <div class="sidebar-top">
      <router-link class="logo" to="/" :title="sidebarCollapsed ? 'cctrack' : ''">
        <div class="logo-glyph"></div>
        <span class="logo-text">CCTRACK</span>
      </router-link>
    </div>

    <nav class="sidebar-nav">
      <div class="nav-label">Dashboard</div>

      <router-link class="nav-item" to="/" exact-active-class="active" title="Overview">
        <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <rect x="1" y="1" width="6" height="6"/>
          <rect x="9" y="1" width="6" height="6"/>
          <rect x="1" y="9" width="6" height="6"/>
          <rect x="9" y="9" width="6" height="6"/>
        </svg>
        <span class="nav-label-text">Overview</span>
      </router-link>

      <router-link class="nav-item" to="/sessions" active-class="active" title="Sessions">
        <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <path d="M1 4h14M1 8h14M1 12h14"/>
          <circle cx="4" cy="4" r="1" fill="currentColor" stroke="none"/>
          <circle cx="4" cy="8" r="1" fill="currentColor" stroke="none"/>
          <circle cx="4" cy="12" r="1" fill="currentColor" stroke="none"/>
        </svg>
        <span class="nav-label-text">Sessions</span>
      </router-link>

      <router-link class="nav-item" to="/projects" active-class="active" title="Projects">
        <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <path d="M1 3h5l2 2h7v8H1V3z"/>
          <path d="M5 8h6"/>
        </svg>
        <span class="nav-label-text">Projects</span>
      </router-link>

      <div class="nav-label" style="margin-top: var(--space-4)">Config</div>

      <router-link class="nav-item" to="/settings" active-class="active" title="Settings">
        <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <circle cx="8" cy="8" r="2.5"/>
          <path d="M8 1v2M8 13v2M1 8h2M13 8h2M2.93 2.93l1.41 1.41M11.66 11.66l1.41 1.41M2.93 13.07l1.41-1.41M11.66 4.34l1.41-1.41"/>
        </svg>
        <span class="nav-label-text">Settings</span>
      </router-link>

      <router-link class="nav-item" to="/rates" active-class="active" title="Rate Card">
        <svg class="nav-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <path d="M2 4h12v8a1 1 0 01-1 1H3a1 1 0 01-1-1V4z"/>
          <path d="M5 4V3a1 1 0 011-1h4a1 1 0 011 1v1"/>
          <path d="M6 8h4M8 6v4"/>
        </svg>
        <span class="nav-label-text">Rate Card</span>
      </router-link>
    </nav>

    <div class="sidebar-footer">
      <ConnectionStatus :status="connectionStatus" />
      <div class="version-str">v0.1.0 · rates {{ ratesVersion || '—' }}</div>
      <button
        class="collapse-toggle"
        @click="toggleSidebar"
        :title="sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar'"
        :aria-label="sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar'"
      >
        <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
          <path :d="sidebarCollapsed ? 'M6 4l4 4-4 4' : 'M10 4l-4 4 4 4'" />
        </svg>
      </button>
    </div>
  </aside>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import type { ConnectionStatus as ConnStatus } from '../../types'
import ConnectionStatus from './ConnectionStatus.vue'
import { useRates } from '../../composables/useRates'
import { useUIPrefs } from '../../composables/useUIPrefs'

defineProps<{ connectionStatus: ConnStatus }>()

const { version: ratesVersion, load } = useRates()
const { sidebarCollapsed, toggleSidebar } = useUIPrefs()

onMounted(load)
</script>

<style scoped>
.sidebar {
  width: var(--sidebar-w);
  min-width: var(--sidebar-w);
  height: 100vh;
  background: var(--bg-surface);
  border-right: 1px solid var(--border-subtle);
  display: flex;
  flex-direction: column;
  position: sticky;
  top: 0;
  overflow: hidden;
  transition: width 200ms cubic-bezier(0.16, 1, 0.3, 1),
              min-width 200ms cubic-bezier(0.16, 1, 0.3, 1);
}
.sidebar.collapsed {
  width: 56px;
  min-width: 56px;
}
/* Hide everything that doesn't fit in 56px when collapsed. The icons stay
   visible and tooltips on hover identify each item. */
.sidebar.collapsed .logo-text,
.sidebar.collapsed .nav-label,
.sidebar.collapsed .nav-label-text,
.sidebar.collapsed .version-str,
.sidebar.collapsed :deep(.connection-label) {
  display: none;
}
.sidebar.collapsed .nav-item {
  justify-content: center;
  padding-left: 0;
  padding-right: 0;
}
.sidebar.collapsed .sidebar-top {
  padding: var(--space-6) 0;
  display: flex;
  justify-content: center;
}
.sidebar.collapsed .sidebar-footer {
  padding: var(--space-4) 0;
  align-items: center;
}
.sidebar-top {
  padding: var(--space-8) var(--space-6) var(--space-6);
  border-bottom: 1px solid var(--border-subtle);
}
.logo {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  text-decoration: none;
}
.logo-glyph {
  width: 22px;
  height: 22px;
  background: var(--amber-500);
  flex-shrink: 0;
  position: relative;
}
.logo-glyph::after {
  content: '';
  position: absolute;
  inset: 3px;
  background: var(--bg-surface);
}
.logo-text {
  font-family: 'Bebas Neue', sans-serif;
  font-size: 22px;
  letter-spacing: 0.06em;
  color: var(--text-primary);
  line-height: 1;
}
.sidebar-nav {
  flex: 1;
  padding: var(--space-5) var(--space-3);
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.nav-label {
  font-size: 10px;
  font-weight: 500;
  letter-spacing: 0.14em;
  text-transform: uppercase;
  color: var(--text-tertiary);
  padding: var(--space-4) var(--space-3) var(--space-2);
}
.nav-item {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: 9px var(--space-3);
  color: var(--text-secondary);
  text-decoration: none;
  font-size: 13.5px;
  font-weight: 400;
  cursor: pointer;
  transition: background 120ms, color 120ms;
  position: relative;
}
.nav-item:hover {
  background: var(--bg-elevated);
  color: var(--text-primary);
}
.nav-item.active {
  color: var(--text-primary);
  background: var(--amber-glow-sm);
}
.nav-item.active::before {
  content: '';
  position: absolute;
  left: 0;
  top: 0;
  bottom: 0;
  width: 2px;
  background: var(--amber-500);
}
.nav-icon {
  width: 16px;
  height: 16px;
  opacity: 0.6;
  flex-shrink: 0;
}
.nav-item.active .nav-icon {
  opacity: 1;
}
.sidebar-footer {
  padding: var(--space-5) var(--space-6);
  border-top: 1px solid var(--border-subtle);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.version-str {
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--text-tertiary);
}

.collapse-toggle {
  background: transparent;
  border: 1px solid var(--border-subtle);
  color: var(--text-tertiary);
  width: 24px;
  height: 24px;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  margin-top: var(--space-2);
  align-self: flex-end;
  transition: color 120ms, border-color 120ms;
  padding: 0;
}
.sidebar.collapsed .collapse-toggle {
  align-self: center;
}
.collapse-toggle:hover {
  color: var(--text-primary);
  border-color: var(--amber-500);
}
.collapse-toggle svg {
  width: 12px;
  height: 12px;
}
</style>
