<template>
  <div class="window-bars">
    <div :class="['window-bar', w.barClass]" v-for="(w, i) in bars" :key="i">
      <div class="window-bar-head">
        <span class="window-bar-title">{{ w.title }}</span>
        <span v-if="w.stateBadge" class="state-badge-wrap">
          <button
            :class="['state-badge', w.stateBadgeClass]"
            :title="w.stateTooltip"
            :aria-expanded="openIdx === i"
            type="button"
            @click.stop="togglePopover(i)"
            @keydown.escape="openIdx = null"
          >
            <span class="state-dot" aria-hidden="true"></span>
            {{ w.stateBadge }}
          </button>
          <div
            v-if="openIdx === i"
            class="state-popover"
            role="dialog"
            @click.stop
          >
            {{ w.stateTooltip }}
          </div>
        </span>
        <span class="window-bar-meta">
          <span v-if="w.hasDenom" class="usage-pct">{{ w.usagePct.toFixed(1) }}%</span>
          <template v-if="w.paceText">
            <span v-if="w.hasDenom" class="sep">·</span>
            <span :class="['pace', w.paceClass]">{{ w.paceText }}</span>
          </template>
          <span class="sep">·</span>
          <span class="remaining">{{ w.remaining }} left</span>
        </span>
      </div>
      <div class="window-bar-track" :title="w.tooltip">
        <!-- Usage fill: cost as a fraction of cap (or prev-window cost when
             no cap is known). Clamped to 100% visually; actual % in pace. -->
        <div class="window-bar-fill" :style="{ width: w.usageWidth + '%' }"></div>
        <!-- Time marker: where we are in the window's clock. Sliding white
             line; if fill is to its right, you're over pace, and vice versa. -->
        <div class="window-bar-marker" :style="{ left: w.timePct + '%' }"></div>
      </div>
      <div class="window-bar-resets">
        <span>Resets {{ w.resetsAt }}</span>
        <span v-if="w.syncedLabel" class="sep">·</span>
        <span v-if="w.syncedLabel" :class="['synced', { stale: w.syncStale }]">
          synced {{ w.syncedLabel }}
        </span>
        <span v-if="w.capLabel" class="sep">·</span>
        <span v-if="w.capLabel" class="cap-label">{{ w.capLabel }}</span>
        <span class="sep">·</span>
        <button
          type="button"
          class="resync-link"
          :disabled="syncing"
          @click="onResyncClick"
        >{{ syncing ? 'syncing…' : 're-sync' }}</button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import type { WindowBucket } from '../../types'
import { triggerUsageSync } from '../../api'

const props = defineProps<{
  fiveHour: WindowBucket | null
  sevenDay: WindowBucket | null
}>()

// Tracks which bar's state-popover (if any) is currently open. Only one
// popover at a time. Per F2 S2.3 PO direction (chat msg 20616): hover OR
// click on the badge surfaces details. Hover is the native `title` attr;
// click toggles a small popover via openIdx + document outside-click.
const openIdx = ref<number | null>(null)
function togglePopover(i: number) {
  openIdx.value = openIdx.value === i ? null : i
}
function closePopover() {
  openIdx.value = null
}
onMounted(() => {
  document.addEventListener('click', closePopover)
})
onUnmounted(() => {
  document.removeEventListener('click', closePopover)
})

// Re-sync button state. The backend single-flights so duplicate clicks
// are safely no-ops; we still disable the button while a request is
// in flight for UX clarity. Failure surfaces through the per-bar
// honest-state badge (no parallel toast/error UI).
const syncing = ref(false)
async function onResyncClick(e: MouseEvent) {
  e.stopPropagation()
  if (syncing.value) return
  syncing.value = true
  try {
    await triggerUsageSync()
    // The backend OnAnchorsUpdated callback broadcasts a fresh summary
    // through the websocket hub when any anchor was written, so the
    // dashboard refreshes automatically on success.
  } catch {
    // Network/HTTP failure: state will resolve via the honest-state badge
    // on the next summary update; nothing to surface here.
  } finally {
    syncing.value = false
  }
}

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
  hasDenom: boolean  // true when cap or prev_cost give us a real denominator
  paceText: string
  paceClass: string  // 'over', 'under', or '' for neutral / no-pace
  remaining: string
  resetsAt: string   // human-readable absolute reset moment, e.g. "Tue 03:00"
  tooltip: string
  syncedLabel: string // "4h ago", or "" if never synced
  syncStale: boolean  // sync older than the window's own duration
  // F2 S2.3 honest-state surface. Empty stateBadge means "render as
  // healthy default" (auto_fresh OR state field absent on older backends).
  stateBadge: string       // short label shown in the badge ('' = no badge)
  stateBadgeClass: string  // CSS class for badge color (e.g. 'badge-warn', 'badge-error')
  stateTooltip: string     // hover/click popover text
  barClass: string         // CSS class applied to .window-bar (e.g. 'state-faded')
  // F3 S3.1 imputed cap on resets line, between "synced X ago" and
  // "re-sync". Empty when w.cap is null/zero so the surrounding
  // separators don't render either ("·  ·" gap on un-synced bars).
  // Format mirrors the existing tooltip wording "$X.XX of $Y.YY cap";
  // the tilde signals "estimated/inferred."
  capLabel: string         // e.g. "~$50.20 cap" or '' when no cap inferred
}

interface StateMeta {
  badge: string
  badgeClass: string
  tooltip: string
  barClass: string
}

// stateMetaFor maps the backend honest-state enum onto the badge + visual
// treatment. The auto_fresh case (and the missing-state default) returns
// empty strings so the bar renders normally — most-of-the-time state.
//
// Per F2 S2.3 PO direction (chat msg 20616): inline badge + subtle dot,
// hover/click for details, no default banners, subtle bar visual treatment.
// Copy is v0.1; PO will redline post-implementation per PM ruling msg 20645.
function stateMetaFor(state: string | undefined): StateMeta {
  switch (state) {
    case 'auto_fresh':
    case undefined:
      // Healthy or backend hasn't populated state — render plain.
      return { badge: '', badgeClass: '', tooltip: '', barClass: '' }
    case 'auto_stale':
      return {
        badge: 'Refreshing',
        badgeClass: 'badge-stale',
        tooltip: 'Window rolled over · auto-sync will pick up the new one shortly',
        barClass: 'state-stale',
      }
    case 'token_expired':
      return {
        badge: 'Sign in',
        badgeClass: 'badge-error',
        tooltip: 'OAuth token expired · open Claude Code to refresh, then restart cctrack to resume auto-sync. Manual sync still works.',
        barClass: 'state-faded',
      }
    case 'provider_unavailable':
      return {
        badge: 'Offline',
        badgeClass: 'badge-warn',
        tooltip: 'Anthropic endpoint unreachable · auto-sync is backing off and will retry. Manual sync still works.',
        barClass: 'state-faded',
      }
    case 'manual_anchor':
      return {
        badge: 'Manual',
        badgeClass: 'badge-neutral',
        tooltip: "Manually synced · auto-sync isn't running.",
        barClass: '',
      }
    case 'fallback_cascade':
    case 'unknown':
    default:
      return {
        badge: 'Estimate',
        badgeClass: 'badge-neutral',
        tooltip: 'No anchor · showing inferred window from request stream. Sync from claude.ai/usage for exact reset times.',
        barClass: 'state-cascade',
      }
  }
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

function syncAgeLabel(iso: string | null | undefined): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 0 || Number.isNaN(ms)) return ''
  const m = Math.floor(ms / 60000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  return `${d}d ago`
}

function buildBar(title: string, w: WindowBucket | null, longHorizon: boolean): Bar | null {
  if (!w?.start) return null
  const timePct = Math.round(pctElapsed(w.start, w.end) * 10) / 10
  const remaining = remainingLabel(w.end)
  const resetsAt = fmtResetMoment(w.end, longHorizon)
  const syncedLabel = syncAgeLabel(w.last_synced_at)
  // "Stale" = sync is older than this window's own duration. A 5h sync from
  // 6h ago has cycled at least once in reality, so the displayed end-time is
  // very likely past Anthropic's actual reset.
  const windowMs = new Date(w.end).getTime() - new Date(w.start).getTime()
  const syncMs = w.last_synced_at ? Date.now() - new Date(w.last_synced_at).getTime() : 0
  const syncStale = !!w.last_synced_at && syncMs > windowMs

  // Denominator preference: cap (synced from claude.ai) > prev_cost (cascade
  // fallback). The cap is plan-level, so it survives anchor expiry and gives
  // a stable cap-relative fill that mirrors what claude.ai shows.
  const denom = w.cap && w.cap > 0 ? w.cap : (w.prev_cost > 0 ? w.prev_cost : 0)
  const mode = w.cap && w.cap > 0 ? 'cap' : (w.prev_cost > 0 ? 'prev' : 'none')
  const baseTooltip = fmtRange(w.start, w.end)
  const tooltip = mode === 'cap'
    ? `${baseTooltip}\n$${w.cost.toFixed(2)} of $${(w.cap as number).toFixed(2)} cap`
    : mode === 'prev'
      ? `${baseTooltip}\n$${w.cost.toFixed(2)} this window · $${w.prev_cost.toFixed(2)} previous`
      : baseTooltip

  const meta = stateMetaFor(w.state)
  // F3 S3.1: imputed cap label, omitted when no cap is inferred yet.
  const capLabel = w.cap && w.cap > 0 ? `~$${w.cap.toFixed(2)} cap` : ''
  if (denom <= 0) {
    return {
      title, timePct, usagePct: 0, usageWidth: 0, hasDenom: false,
      paceText: 'sync to enable', paceClass: '',
      remaining, resetsAt, tooltip, syncedLabel, syncStale,
      stateBadge: meta.badge, stateBadgeClass: meta.badgeClass,
      stateTooltip: meta.tooltip, barClass: meta.barClass,
      capLabel,
    }
  }
  const usagePct = (w.cost / denom) * 100
  const usageWidth = Math.max(0, Math.min(100, usagePct))
  // Pace = how far ahead/behind the time marker the usage fill is. Same metric
  // for both directions; sign tells us which.
  const delta = usagePct - timePct
  const sign = delta > 0 ? '+' : ''
  const paceClass = delta > 5 ? 'over' : delta < -5 ? 'under' : ''
  const paceText = `${sign}${Math.round(delta)}% pace`
  return {
    title, timePct, usagePct, usageWidth, hasDenom: true, paceText, paceClass,
    remaining, resetsAt, tooltip, syncedLabel, syncStale,
    stateBadge: meta.badge, stateBadgeClass: meta.badgeClass,
    stateTooltip: meta.tooltip, barClass: meta.barClass,
    capLabel,
  }
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
.window-bar-meta .usage-pct { color: var(--text-primary); }
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
  display: flex;
  align-items: baseline;
  gap: 6px;
  flex-wrap: wrap;
}
.window-bar-resets .sep {
  color: var(--border-subtle);
}
.window-bar-resets .synced {
  color: var(--text-tertiary);
}
.window-bar-resets .synced.stale {
  color: var(--cost-high);
}
.window-bar-resets .cap-label {
  color: var(--text-tertiary);
  font-variant-numeric: tabular-nums;
}
.resync-link {
  color: var(--text-tertiary);
  text-decoration: underline;
  text-decoration-color: var(--border-default);
  text-underline-offset: 2px;
  transition: color 120ms;
  background: transparent;
  border: 0;
  padding: 0;
  font: inherit;
  cursor: pointer;
}
.resync-link:hover:not(:disabled) {
  color: var(--amber-400);
}
.resync-link:disabled {
  cursor: progress;
  opacity: 0.7;
}

/* F2 S2.3 honest-state badges. The badge sits in the bar head between the
   title and the metadata (usage %, pace, remaining). Only renders when
   stateBadge is non-empty — auto_fresh state and missing-state both render
   the bar without a badge. The wrap is the positioning context for the
   click-toggle popover. */
.state-badge-wrap {
  position: relative;
  display: inline-flex;
}
.state-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 10px;
  font-weight: 500;
  padding: 1px 6px;
  border-radius: 999px;
  letter-spacing: 0.04em;
  cursor: pointer;
  user-select: none;
  background: transparent;
  border: 1px solid transparent;
  color: inherit;
  font: inherit;
  font-family: 'JetBrains Mono', monospace;
  font-size: 10px;
  font-weight: 500;
  letter-spacing: 0.04em;
}
.state-badge:focus-visible {
  outline: 2px solid var(--amber-400);
  outline-offset: 1px;
}

/* Click-toggle popover. Anchored to the badge wrap; one open at a time
   (closes on outside click via document listener). Snap appearance — no
   transition — per kiku's UX bar (msg 20616 E). */
.state-popover {
  position: absolute;
  top: calc(100% + 4px);
  right: 0;
  z-index: 10;
  background: var(--bg-elevated, var(--bg-base));
  border: 1px solid var(--border-default);
  border-radius: 6px;
  padding: 8px 10px;
  font-size: 11px;
  line-height: 1.4;
  color: var(--text-secondary);
  max-width: 280px;
  width: max-content;
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.25);
}
.state-dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  display: inline-block;
  background: currentColor;
}
.state-badge.badge-stale {
  color: var(--text-tertiary);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-default);
}
.state-badge.badge-error {
  color: var(--cost-high);
  background: rgba(239, 68, 68, 0.1); /* red tint */
}
.state-badge.badge-warn {
  color: var(--amber-400);
  background: rgba(251, 191, 36, 0.1); /* amber tint */
}
.state-badge.badge-neutral {
  color: var(--text-tertiary);
  background: var(--bg-subtle);
}

/* Subtle bar visual treatments per state. The defaults (auto_fresh,
   undefined, manual_anchor) leave .window-bar untouched. */
.window-bar.state-faded {
  opacity: 0.85;
}
.window-bar.state-faded .window-bar-fill {
  opacity: 0.7;
}
.window-bar.state-stale .window-bar-track {
  border-style: dashed;
}
.window-bar.state-cascade {
  opacity: 0.75;
}
.window-bar.state-cascade .window-bar-fill {
  background: var(--text-tertiary); /* no claim of "real" usage data */
}
</style>
