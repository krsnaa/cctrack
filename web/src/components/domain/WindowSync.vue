<template>
  <div class="window-sync">
    <div class="ws-head">
      <span class="ws-title">{{ title }}</span>
      <span v-if="latest" class="ws-status">
        synced {{ relativeAgo(latest.synced_at) }} ago
        <span v-if="latest.inferred_cap != null" class="ws-cap">
          · cap ≈ ${{ latest.inferred_cap.toFixed(2) }}
        </span>
      </span>
      <span v-else class="ws-status muted">never synced</span>
    </div>

    <div class="ws-fields">
      <!-- 5h: time-left in h/m. 7d: pick reset weekday + HH:MM (more natural
           given Anthropic's "Resets Tue 03:00" UI affordance). -->
      <div class="ws-field" v-if="windowType === '5h'">
        <label>Time left</label>
        <div class="ws-duo">
          <input
            type="number"
            v-model.number="hours"
            min="0"
            max="4"
            placeholder="h"
            aria-label="Hours"
          />
          <span class="sep">h</span>
          <input
            type="number"
            v-model.number="minutes"
            min="0"
            max="59"
            placeholder="m"
            aria-label="Minutes"
          />
          <span class="sep">m</span>
        </div>
      </div>

      <div class="ws-field" v-else>
        <label>Resets on</label>
        <div class="ws-duo">
          <select v-model.number="weekday" class="ws-select" aria-label="Weekday">
            <option :value="0">Sun</option>
            <option :value="1">Mon</option>
            <option :value="2">Tue</option>
            <option :value="3">Wed</option>
            <option :value="4">Thu</option>
            <option :value="5">Fri</option>
            <option :value="6">Sat</option>
          </select>
          <span class="sep">@</span>
          <input
            type="time"
            v-model="resetTime"
            class="ws-time"
            aria-label="Reset time"
          />
        </div>
      </div>

      <div class="ws-field">
        <label>% used <span class="optional">(optional)</span></label>
        <div class="input-with-suffix">
          <input
            type="number"
            v-model.number="pct"
            min="0"
            max="100"
            step="0.1"
            placeholder="59"
          />
          <span class="suffix">%</span>
        </div>
      </div>

      <button class="ws-sync-btn" :disabled="!canSubmit || submitting" @click="submit">
        {{ submitting ? 'Syncing…' : 'Sync' }}
      </button>
    </div>

    <div v-if="status" class="ws-msg" :class="statusClass">{{ status }}</div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { fetchWindowAnchors, postWindowAnchor } from '../../api'
import type { WindowAnchor } from '../../types'

const props = defineProps<{ windowType: '5h' | '7d' }>()

// 5h fields
const hours = ref<number | null>(null)
const minutes = ref<number | null>(null)

// 7d fields — weekday (0=Sun..6=Sat) and clock time HH:MM, used to compute
// the next future occurrence. Default to "now" so the user starts from a
// sensible value and adjusts.
const weekday = ref<number>(new Date().getDay())
const resetTime = ref<string>(formatHM(new Date()))

const pct = ref<number | null>(null)

function formatHM(d: Date): string {
  const h = d.getHours().toString().padStart(2, '0')
  const m = d.getMinutes().toString().padStart(2, '0')
  return `${h}:${m}`
}

const latest = ref<WindowAnchor | null>(null)
const submitting = ref(false)
const status = ref('')
const statusClass = ref<'ok' | 'err' | ''>('')

const title = computed(() => (props.windowType === '5h' ? '5h Window' : '7d Window'))

const totalMinutes = computed(() => {
  if (props.windowType === '5h') {
    const h = hours.value ?? 0
    const m = minutes.value ?? 0
    return h * 60 + m
  }
  // 7d: derive minutes-until from chosen weekday + HH:MM, picking the next
  // future occurrence. If the chosen moment is today but already past, roll
  // forward 7 days.
  if (!resetTime.value) return 0
  const [hh, mm] = resetTime.value.split(':').map(Number)
  if (Number.isNaN(hh) || Number.isNaN(mm)) return 0
  const now = new Date()
  const target = new Date(now)
  const daysAhead = (weekday.value - now.getDay() + 7) % 7
  target.setDate(now.getDate() + daysAhead)
  target.setHours(hh, mm, 0, 0)
  if (target.getTime() <= now.getTime()) {
    target.setDate(target.getDate() + 7)
  }
  return Math.floor((target.getTime() - now.getTime()) / 60000)
})

const canSubmit = computed(() => totalMinutes.value > 0)

async function loadLatest() {
  try {
    const r = await fetchWindowAnchors(props.windowType, 1)
    latest.value = r.anchors?.[0] ?? null
  } catch {
    latest.value = null
  }
}

async function submit() {
  if (!canSubmit.value) return
  submitting.value = true
  status.value = ''
  try {
    await postWindowAnchor(
      props.windowType,
      totalMinutes.value,
      pct.value != null && pct.value > 0 ? pct.value : undefined,
    )
    status.value = 'Anchor saved. Window bars will reflect this on next refresh.'
    statusClass.value = 'ok'
    await loadLatest()
    hours.value = null
    minutes.value = null
    pct.value = null
    // Re-default 7d weekday/time to "now" for the next sync cycle.
    weekday.value = new Date().getDay()
    resetTime.value = formatHM(new Date())
  } catch (e: any) {
    status.value = `Sync failed: ${e?.message ?? 'unknown error'}`
    statusClass.value = 'err'
  } finally {
    submitting.value = false
  }
}

function relativeAgo(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 0) return 'just now'
  const m = Math.floor(ms / 60000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ${m % 60}m`
  const d = Math.floor(h / 24)
  return `${d}d ${h % 24}h`
}

onMounted(loadLatest)
</script>

<style scoped>
.window-sync {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4) var(--space-5);
  background: var(--bg-subtle);
  border: 1px solid var(--border-subtle);
}
.ws-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
}
.ws-title {
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  color: var(--text-primary);
}
.ws-status {
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--text-secondary);
}
.ws-status.muted { color: var(--text-disabled); }
.ws-cap { color: var(--amber-400); }

.ws-fields {
  display: grid;
  grid-template-columns: 1fr 1fr auto;
  gap: var(--space-4);
  align-items: end;
}
.ws-field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.ws-field label {
  font-size: 11px;
  color: var(--text-tertiary);
  text-transform: uppercase;
  letter-spacing: 0.1em;
}
.optional {
  color: var(--text-disabled);
  text-transform: none;
  letter-spacing: normal;
}

.ws-duo {
  display: flex;
  align-items: center;
  gap: 4px;
}
.ws-duo input {
  width: 56px;
  background: var(--bg-base);
  border: 1px solid var(--border-default);
  color: var(--text-primary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  padding: 6px 8px;
}
.ws-duo .sep {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  color: var(--text-tertiary);
}
.ws-select,
.ws-time {
  background: var(--bg-base);
  border: 1px solid var(--border-default);
  color: var(--text-primary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  padding: 6px 8px;
}
.ws-select:hover,
.ws-time:hover { border-color: var(--amber-500); }
.ws-select:focus,
.ws-time:focus { outline: none; border-color: var(--amber-500); }
.input-with-suffix {
  display: flex;
  align-items: center;
}
.input-with-suffix input {
  background: var(--bg-base);
  border: 1px solid var(--border-default);
  color: var(--text-primary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  padding: 6px 8px;
  width: 80px;
}
.input-with-suffix .suffix {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  color: var(--text-tertiary);
  margin-left: 4px;
}

.ws-sync-btn {
  background: var(--amber-500);
  color: var(--bg-base);
  border: none;
  padding: 8px 16px;
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: background 120ms;
  text-transform: uppercase;
  letter-spacing: 0.08em;
}
.ws-sync-btn:hover:not(:disabled) { background: var(--amber-400); }
.ws-sync-btn:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}

.ws-msg {
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
}
.ws-msg.ok { color: #4ade80; }
.ws-msg.err { color: var(--cost-high); }
</style>
