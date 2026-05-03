<template>
  <div class="window-bars">
    <div class="window-bar" v-for="(w, i) in bars" :key="i">
      <div class="window-bar-head">
        <span class="window-bar-title">{{ w.title }}</span>
        <span class="window-bar-meta">
          <span class="time-pct">{{ w.timePct.toFixed(1) }}%</span>
          <template v-if="w.paceText">
            <span class="sep">·</span>
            <span :class="['pace', w.paceClass]">{{ w.paceText }}</span>
          </template>
          <span class="sep">·</span>
          <span class="remaining">{{ w.remaining }} left</span>
        </span>
      </div>
      <div class="window-bar-track" :title="w.tooltip">
        <!-- Usage fill: cost so far as a fraction of the previous window's
             total cost. Clamped to 100% visually; actual value shown in pace. -->
        <div class="window-bar-fill" :style="{ width: w.usageWidth + '%' }"></div>
        <!-- Time marker: where we are in the window's clock. Sliding white
             line; if fill is to its right, you're over pace, and vice versa. -->
        <div class="window-bar-marker" :style="{ left: w.timePct + '%' }"></div>
      </div>
      <div class="window-bar-resets">Resets {{ w.resetsAt }}</div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { WindowBucket } from '../../types'

const props = defineProps<{
  fiveHour: WindowBucket | null
  sevenDay: WindowBucket | null
}>()

function pctElapsed(start: string, end: string): number {
  if (!start || !end) return 0
  const s = new Date(start).getTime()
  const e = new Date(end).getTime()
  const now = Date.now()
  if (e <= s) return 0
  return Math.max(0, Math.min(100, ((now - s) / (e - s)) * 100))
}

function remainingLabel(end: string): string {
  if (!end) return '—'
  const ms = new Date(end).getTime() - Date.now()
  if (ms <= 0) return 'expired'
  const totalMin = Math.floor(ms / 60000)
  const days = Math.floor(totalMin / (24 * 60))
  const hours = Math.floor((totalMin % (24 * 60)) / 60)
  const minutes = totalMin % 60
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

function fmtRange(start: string, end: string): string {
  if (!start || !end) return ''
  const s = new Date(start)
  const e = new Date(end)
  const opts: Intl.DateTimeFormatOptions = {
    weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false,
  }
  return `${s.toLocaleString('en-GB', opts)} → ${e.toLocaleString('en-GB', opts)}`
}

interface Bar {
  title: string
  timePct: number
  usagePct: number   // actual ratio (can exceed 100%)
  usageWidth: number // clamped 0-100 for rendering
  paceText: string
  paceClass: string  // 'over', 'under', or '' for neutral / no-pace
  remaining: string
  resetsAt: string   // human-readable absolute reset moment, e.g. "Tue 03:00"
  tooltip: string
}

function fmtResetMoment(end: string, longHorizon: boolean): string {
  if (!end) return '—'
  const d = new Date(end)
  if (longHorizon) {
    // Weekly window: surface day-of-week + time so the reset is recognizable
    // ("Resets Tue 03:00") without parsing a duration.
    return d.toLocaleString('en-GB', {
      weekday: 'short', hour: '2-digit', minute: '2-digit', hour12: false,
    })
  }
  // 5h window: same calendar day usually, just the time of day.
  return d.toLocaleString('en-GB', {
    hour: '2-digit', minute: '2-digit', hour12: false,
  })
}

function buildBar(title: string, w: WindowBucket | null, longHorizon: boolean): Bar | null {
  if (!w?.start) return null
  const timePct = Math.round(pctElapsed(w.start, w.end) * 10) / 10
  const remaining = remainingLabel(w.end)
  const tooltip = fmtRange(w.start, w.end)
  const resetsAt = fmtResetMoment(w.end, longHorizon)

  // Usage fill is normalized against the previous window's total cost. Without
  // a previous window we can't compute pace meaningfully — show time marker
  // only, no fill.
  if (!w.prev_cost || w.prev_cost <= 0) {
    return {
      title, timePct, usagePct: 0, usageWidth: 0,
      paceText: 'no prev window', paceClass: '',
      remaining, resetsAt, tooltip,
    }
  }
  const usagePct = (w.cost / w.prev_cost) * 100
  const usageWidth = Math.max(0, Math.min(100, usagePct))
  // Pace = how far ahead/behind the time marker the usage fill is. Same metric
  // for both directions; sign tells us which.
  const delta = usagePct - timePct
  const sign = delta > 0 ? '+' : ''
  const paceClass = delta > 5 ? 'over' : delta < -5 ? 'under' : ''
  const paceText = `${sign}${Math.round(delta)}% pace`
  return { title, timePct, usagePct, usageWidth, paceText, paceClass, remaining, resetsAt, tooltip }
}

const bars = computed(() => {
  const out: Bar[] = []
  const a = buildBar('5h Window', props.fiveHour, false)
  if (a) out.push(a)
  const b = buildBar('7d Window', props.sevenDay, true)
  if (b) out.push(b)
  return out
})
</script>

<style scoped>
.window-bars {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-5);
  margin-bottom: var(--space-6);
  animation: fadeSlideUp 0.4s ease both;
}
.window-bar {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.window-bar-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  font-size: 11px;
}
.window-bar-title {
  text-transform: uppercase;
  letter-spacing: 0.12em;
  color: var(--text-tertiary);
  font-weight: 500;
  font-size: 10.5px;
}
.window-bar-meta {
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--text-secondary);
  display: flex;
  align-items: baseline;
  gap: 6px;
}
.window-bar-meta .sep {
  color: var(--text-disabled);
}
.window-bar-meta .time-pct { color: var(--text-primary); }
.window-bar-meta .pace.over { color: var(--cost-high); }
.window-bar-meta .pace.under { color: #4ade80; }
.window-bar-meta .pace { color: var(--text-tertiary); }
.window-bar-meta .remaining { color: var(--text-tertiary); }

.window-bar-track {
  position: relative;
  height: 10px;
  background: var(--bg-subtle);
  border: 1px solid var(--border-subtle);
  border-radius: 999px;
  overflow: hidden;
}
.window-bar-fill {
  position: absolute;
  inset: 0 auto 0 0;
  background: var(--amber-500);
  transition: width 600ms cubic-bezier(0.16, 1, 0.3, 1);
}
/* Time marker: a high-contrast vertical line that slides across the track.
   The eye reads "fill behind marker = under pace" / "fill past marker = over
   pace" without needing the numeric label. */
.window-bar-marker {
  position: absolute;
  top: -2px;
  bottom: -2px;
  width: 3px;
  background: var(--text-primary);
  transform: translateX(-1.5px);
  box-shadow: 0 0 0 1px var(--bg-base);
  transition: left 600ms cubic-bezier(0.16, 1, 0.3, 1);
  z-index: 1;
}
.window-bar-resets {
  font-family: 'JetBrains Mono', monospace;
  font-size: 10.5px;
  color: var(--text-disabled);
  margin-top: 2px;
}
</style>
