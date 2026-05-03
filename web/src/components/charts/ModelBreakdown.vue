<template>
  <div class="model-card">
    <div class="chart-header">
      <div class="chart-title">Spend by Model</div>
      <div class="chart-meta" v-if="totalCost > 0">{{ formatCostDisplay(totalCost) }} total</div>
    </div>
    <div class="model-bars">
      <div v-for="(group, i) in familyGroups" :key="group.family" class="model-bar-row">
        <div class="bar-label">
          <span class="bar-family">{{ formatFamily(group.family) }}</span>
          <span class="bar-cost">{{ formatCostDisplay(group.cost) }}</span>
        </div>
        <div class="bar-track">
          <div
            class="bar-fill"
            :style="{ width: barWidth(group.cost) + '%', background: familyColors[i % familyColors.length] }"
          ></div>
        </div>
        <div class="bar-meta">
          <span>{{ group.pct }}% of spend</span>
          <span class="bar-sessions">{{ group.sessions }} sessions</span>
        </div>
      </div>
    </div>
    <div class="savings-hint" v-if="savingsEstimate > 0">
      <div class="savings-icon">$</div>
      <div class="savings-text">
        Switching Opus sessions to Sonnet could save ~<strong>{{ formatCostDisplay(savingsEstimate) }}</strong>/mo
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import type { ModelSummary } from '../../types'
import { formatCostDisplay, formatFamily } from '../../composables/useFormatCost'
import { useRates } from '../../composables/useRates'

const props = defineProps<{ models: ModelSummary[] }>()

// Eight-color palette ordered for high contrast on adjacent bars. Earlier
// versions had 4 colors which started repeating once the rate card was split
// into per-version families (Opus 4 / 4.1 / 4.5 / 4.6 / 4.7 etc).
const familyColors = [
  '#f59e0b', // amber
  '#0ea5e9', // sky
  '#10b981', // emerald
  '#a78bfa', // violet
  '#fbbf24', // amber light
  '#ec4899', // pink
  '#78716c', // stone
  '#44403c', // stone dark
]

const { rates, load: loadRates } = useRates()
onMounted(loadRates)

const totalCost = computed(() => props.models.reduce((s, m) => s + m.total_cost, 0))

const familyGroups = computed(() => {
  const map = new Map<string, { cost: number; sessions: number }>()
  for (const m of props.models) {
    const existing = map.get(m.family) || { cost: 0, sessions: 0 }
    existing.cost += m.total_cost
    existing.sessions += m.session_count
    map.set(m.family, existing)
  }
  return Array.from(map.entries())
    .sort((a, b) => b[1].cost - a[1].cost)
    .map(([family, data]) => ({
      family,
      cost: data.cost,
      sessions: data.sessions,
      pct: totalCost.value > 0 ? Math.round((data.cost / totalCost.value) * 100) : 0,
    }))
})

function barWidth(cost: number) {
  const max = familyGroups.value[0]?.cost || 1
  return Math.max((cost / max) * 100, 2)
}

// Estimate savings if Opus spend were redirected to Sonnet. Computed from the
// live rate card: for each Opus group, scale its actual cost by Sonnet's output
// rate / that Opus version's output rate. Output dominates LLM costs and
// scaling input identically would over-state cache-heavy workloads.
const savingsEstimate = computed(() => {
  if (!rates.value.length) return 0
  const sonnetRate = rates.value
    .filter(r => r.Family.startsWith('claude-sonnet-4'))
    .reduce((min, r) => Math.min(min, r.OutputPerMToken), Infinity)
  if (!isFinite(sonnetRate)) return 0
  let saved = 0
  for (const group of familyGroups.value) {
    if (!group.family.includes('opus')) continue
    const opus = rates.value.find(r => r.Family === group.family)
    if (!opus || opus.OutputPerMToken <= sonnetRate) continue
    saved += group.cost * (1 - sonnetRate / opus.OutputPerMToken)
  }
  return saved
})
</script>

<style scoped>
.model-card {
  background: var(--bg-surface);
  border: 1px solid var(--border-subtle);
  padding: var(--space-6);
  animation: fadeSlideUp 0.45s ease both;
  animation-delay: 400ms;
}
.chart-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-5);
}
.chart-title {
  font-size: 11px;
  font-weight: 500;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--text-tertiary);
}
.chart-meta {
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--text-tertiary);
}
.model-bars {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.model-bar-row {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.bar-label {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
}
.bar-family {
  font-size: 13px;
  font-weight: 500;
  color: var(--text-primary);
}
.bar-cost {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  color: var(--amber-400);
}
.bar-track {
  height: 6px;
  background: var(--bg-subtle);
  overflow: hidden;
}
.bar-fill {
  height: 100%;
  transition: width 800ms cubic-bezier(0.16, 1, 0.3, 1);
}
.bar-meta {
  display: flex;
  justify-content: space-between;
  font-family: 'JetBrains Mono', monospace;
  font-size: 10px;
  color: var(--text-tertiary);
}
.bar-sessions {
  color: var(--text-disabled);
}
.savings-hint {
  margin-top: var(--space-5);
  padding: var(--space-3) var(--space-4);
  background: rgba(245, 158, 11, 0.06);
  border: 1px solid rgba(245, 158, 11, 0.12);
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}
.savings-icon {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  font-weight: 600;
  color: var(--amber-500);
  width: 20px;
  height: 20px;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgba(245, 158, 11, 0.12);
  flex-shrink: 0;
}
.savings-text {
  font-size: 11.5px;
  color: var(--text-secondary);
  line-height: 1.45;
}
.savings-text strong {
  color: var(--amber-400);
  font-weight: 500;
}
</style>
