<template>
  <div>
    <div class="page-header">
      <h1 class="page-title">Settings</h1>
    </div>

    <div class="settings-form" v-if="store.current">
      <section class="settings-section">
        <div class="section-label">Data Sources</div>
        <div class="field">
          <label>Log Directory</label>
          <input type="text" v-model="store.draft.log_dir" />
        </div>
        <div class="field">
          <label>Database Path</label>
          <div class="read-only">{{ store.current.db_path }}</div>
        </div>
      </section>

      <section class="settings-section">
        <div class="section-label">Plan & Budget</div>
        <div class="field">
          <label>Claude Plan</label>
          <select class="plan-select" v-model="store.draft.claude_plan">
            <option value="">— not set —</option>
            <option value="free">Free</option>
            <option value="pro">Pro</option>
            <option value="max-5x">Max (5x)</option>
            <option value="max-20x">Max (20x)</option>
            <option value="team">Team</option>
            <option value="enterprise">Enterprise</option>
          </select>
        </div>
        <div class="field">
          <label>Monthly Budget (USD)</label>
          <div class="input-with-prefix">
            <span class="prefix">$</span>
            <input type="number" v-model.number="store.draft.monthly_budget_usd" min="0" step="1" />
          </div>
        </div>
      </section>

      <section class="settings-section">
        <div class="section-label">Dashboard</div>
        <div class="field row">
          <label>Open browser on serve</label>
          <Toggle v-model="store.draft.open_browser_on_serve" />
        </div>
        <div class="field">
          <label>Port</label>
          <div class="read-only">{{ store.current.port }}</div>
        </div>
        <div class="field">
          <label>Debounce Window</label>
          <div class="read-only">250ms</div>
        </div>
      </section>

      <section class="settings-section">
        <div class="section-label">Window Sync</div>
        <p class="section-help">
          cctrack only sees claude-code activity on this machine, so its
          rolling window estimates can drift from what claude.ai shows. Paste
          the time-left value (and optionally % used) from claude.ai → Settings
          → Plan usage limits to anchor cctrack's windows to Anthropic's
          authoritative state.
        </p>

        <WindowSync window-type="5h" />
        <WindowSync window-type="7d" />
      </section>

      <section class="settings-section">
        <div class="section-label">About</div>
        <div class="about-grid">
          <span>Version</span><span class="mono">v0.1.0</span>
          <span>Rate Card</span>
          <span class="mono">
            {{ ratesVersion || '—' }}<span v-if="ratesUpdated" class="rates-updated"> · {{ ratesUpdated }}</span>
          </span>
        </div>
      </section>

      <div class="actions">
        <button class="save-btn" :disabled="!store.isDirty || store.saving" @click="store.save()">
          {{ store.saving ? 'Saving…' : store.saved ? 'Saved' : 'Save Settings' }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useSettingsStore } from '../stores/settings'
import { useRates } from '../composables/useRates'
import Toggle from '../components/primitives/Toggle.vue'
import WindowSync from '../components/domain/WindowSync.vue'

const store = useSettingsStore()
const { version: ratesVersion, updated: ratesUpdated, load: loadRates } = useRates()

onMounted(() => {
  store.load()
  loadRates()
})
</script>

<style scoped>
.page-header {
  margin-bottom: var(--space-8);
  animation: fadeSlideUp 0.4s ease both;
}
.page-title {
  font-family: 'Bebas Neue', sans-serif;
  font-size: 36px;
  letter-spacing: 0.04em;
  color: var(--text-primary);
  line-height: 1;
}

.settings-form {
  max-width: 600px;
  display: flex;
  flex-direction: column;
  gap: var(--space-10);
}
.settings-section {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}
.section-label {
  font-size: 10.5px;
  font-weight: 500;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--text-tertiary);
  padding-bottom: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
}
.section-help {
  font-size: 12px;
  color: var(--text-tertiary);
  line-height: 1.5;
  margin: 0;
  max-width: 60ch;
}
.plan-select {
  background: var(--bg-subtle);
  border: 1px solid var(--border-default);
  color: var(--text-primary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  padding: var(--space-3) var(--space-4);
  cursor: pointer;
  max-width: 240px;
}
.plan-select:hover { border-color: var(--amber-500); }
.plan-select:focus { outline: none; border-color: var(--amber-500); }
.field {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.field.row {
  flex-direction: row;
  justify-content: space-between;
  align-items: center;
}
.field label {
  font-size: 13px;
  color: var(--text-secondary);
}
.field input[type="text"],
.field input[type="number"] {
  background: var(--bg-subtle);
  border: 1px solid var(--border-default);
  color: var(--text-primary);
  padding: var(--space-3) var(--space-4);
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  outline: none;
  transition: border-color 150ms;
}
.field input:focus {
  border-color: var(--amber-500);
  box-shadow: 0 0 0 2px rgba(245, 158, 11, 0.15);
}
.input-with-prefix {
  display: flex;
  align-items: center;
}
.input-with-prefix .prefix {
  background: var(--bg-elevated);
  border: 1px solid var(--border-default);
  border-right: none;
  padding: var(--space-3) var(--space-3);
  color: var(--text-tertiary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
}
.input-with-prefix input {
  flex: 1;
}
.read-only {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  color: var(--text-tertiary);
  padding: var(--space-2) 0;
}

.about-grid {
  display: grid;
  grid-template-columns: 100px 1fr;
  gap: var(--space-2) var(--space-4);
  font-size: 13px;
  color: var(--text-secondary);
}
.about-grid .mono {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  color: var(--text-tertiary);
}
.about-grid .rates-updated {
  color: var(--text-disabled);
}

.actions {
  padding-top: var(--space-4);
}
.save-btn {
  background: var(--amber-500);
  color: var(--bg-base);
  border: none;
  padding: var(--space-3) var(--space-6);
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  transition: background 150ms;
}
.save-btn:hover:not(:disabled) {
  background: var(--amber-400);
}
.save-btn:disabled {
  opacity: 0.4;
  cursor: default;
}
</style>
