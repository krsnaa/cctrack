<template>
  <div class="window-bars">
    <div class="window-bar" v-for="(w, i) in bars" :key="i">
      <div class="window-bar-head">
        <span class="window-bar-title">{{ w.title }}</span>
        <span class="window-bar-meta">
          {{ w.pct }}%<span class="sep">·</span>{{ w.remaining }} left
        </span>
      </div>
      <div class="window-bar-track" :title="w.tooltip">
        <div class="window-bar-fill" :style="{ width: w.pct + '%' }"></div>
        <div class="window-bar-marker" :style="{ left: w.pct + '%' }"></div>
      </div>
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

const bars = computed(() => {
  const out: Array<{ title: string; pct: number; remaining: string; tooltip: string }> = []
  if (props.fiveHour?.start) {
    const pct = Math.round(pctElapsed(props.fiveHour.start, props.fiveHour.end) * 10) / 10
    out.push({
      title: '5h Window',
      pct,
      remaining: remainingLabel(props.fiveHour.end),
      tooltip: fmtRange(props.fiveHour.start, props.fiveHour.end),
    })
  }
  if (props.sevenDay?.start) {
    const pct = Math.round(pctElapsed(props.sevenDay.start, props.sevenDay.end) * 10) / 10
    out.push({
      title: '7d Window',
      pct,
      remaining: remainingLabel(props.sevenDay.end),
      tooltip: fmtRange(props.sevenDay.start, props.sevenDay.end),
    })
  }
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
}
.window-bar-meta .sep {
  margin: 0 6px;
  color: var(--text-disabled);
}
.window-bar-track {
  position: relative;
  height: 8px;
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
.window-bar-marker {
  position: absolute;
  top: 0;
  bottom: 0;
  width: 2px;
  background: var(--text-primary);
  transform: translateX(-1px);
  transition: left 600ms cubic-bezier(0.16, 1, 0.3, 1);
}
</style>
