<template>
  <div>
    <div class="page-header">
      <h1 class="page-title">Overview</h1>
      <div class="page-date">{{ currentDate }}</div>
    </div>

    <div class="stat-grid" v-if="store.summary">
      <StatCard
        :label="hourLabel"
        :value="store.summary.hour?.cost ?? 0"
        :tokens="store.summary.hour?.tokens ?? 0"
        :highlight="true"
        :trendPct="hourTrend"
        :prevName="prevHourLabel"
        :prevAmount="store.summary.trends?.prev_hour_cost"
      />
      <StatCard
        :label="todayLabel"
        :value="store.summary.today.cost"
        :tokens="store.summary.today.tokens"
        :trendPct="dayTrend"
        :prevName="yesterdayLabel"
        :prevAmount="store.summary.trends?.prev_day_cost"
      />
      <StatCard
        :label="weekLabel"
        :value="store.summary.week.cost"
        :tokens="store.summary.week.tokens"
        :trendPct="weekTrend"
        :prevName="prevWeekLabel"
        :prevAmount="store.summary.trends?.prev_week_cost"
      />
      <StatCard
        :label="monthLabel"
        :value="store.summary.month.cost"
        :tokens="store.summary.month.tokens"
        :budget="store.summary.budget"
        :trendPct="monthTrend"
        :prevName="prevMonthName"
        :prevAmount="store.summary.trends?.prev_month_cost"
      />
    </div>

    <div class="charts-row" v-if="store.summary">
      <DailySpendChart />
      <StatCard
        class="projected-slot"
        label="Projected"
        :value="store.summary.projected"
        subtext="est. this month"
      />
    </div>

    <!-- Model breakdown + two donuts (cost-by-type, project-by-prev-month) -->
    <div class="insights-row" v-if="models.length || costBreakdownSlices.length">
      <ModelBreakdown :models="models" />
      <Donut
        title="Cost Breakdown"
        subtitle="all time · by token type"
        :slices="costBreakdownSlices"
      />
      <Donut
        title="Spend by Project"
        :subtitle="prevMonthLabel + ' · top projects'"
        :slices="prevMonthProjectSlices"
        emptyText="No spend last month"
      />
    </div>

    <!-- Activity heatmap on its own row so the wider canvas reads clearly -->
    <div class="heatmap-row" v-if="heatmap.length">
      <ActivityHeatmap :cells="heatmap" />
    </div>

    <div class="section-header" v-if="store.recentSessions.length">
      <div class="section-title">Recent Sessions</div>
      <router-link class="view-all" to="/sessions">View all →</router-link>
    </div>

    <div class="sessions-table-wrap" v-if="store.recentSessions.length">
      <table>
        <thead>
          <tr>
            <th style="width:40px">#</th>
            <th>Session</th>
            <th>Model</th>
            <th>Last Active</th>
            <th class="right">Tokens</th>
            <th class="right">Cost</th>
          </tr>
        </thead>
        <tbody>
          <SessionRow
            v-for="(session, i) in store.recentSessions"
            :key="session.id"
            :session="session"
            :rank="i + 1"
            @select="openSession"
          />
        </tbody>
      </table>
    </div>

    <div class="section-header top-section" v-if="store.topSessions.length">
      <div class="section-title">Most Expensive</div>
    </div>

    <div class="sessions-table-wrap" v-if="store.topSessions.length">
      <table>
        <thead>
          <tr>
            <th style="width:40px">#</th>
            <th>Session</th>
            <th>Model</th>
            <th>Last Active</th>
            <th class="right">Tokens</th>
            <th class="right">Cost</th>
          </tr>
        </thead>
        <tbody>
          <SessionRow
            v-for="(session, i) in store.topSessions"
            :key="session.id"
            :session="session"
            :rank="i + 1"
            @select="openSession"
          />
        </tbody>
      </table>
    </div>

    <SlideOver :open="!!selectedSession" @close="selectedSession = null">
      <SessionDetail :session="selectedSession" />
    </SlideOver>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useDashboardStore } from '../stores/dashboard'
import StatCard from '../components/primitives/StatCard.vue'
import DailySpendChart from '../components/charts/DailySpendChart.vue'
import Donut from '../components/charts/Donut.vue'
import ModelBreakdown from '../components/charts/ModelBreakdown.vue'
import ActivityHeatmap from '../components/charts/ActivityHeatmap.vue'
import SessionRow from '../components/domain/SessionRow.vue'
import SessionDetail from '../components/domain/SessionDetail.vue'
import SlideOver from '../components/primitives/SlideOver.vue'
import type { Session, ModelSummary, HeatmapCell, ProjectMonthly } from '../types'
import { fetchSession, fetchModels, fetchHeatmap, fetchProjectsPrevMonth } from '../api'

const store = useDashboardStore()
const selectedSession = ref<Session | null>(null)
const models = ref<ModelSummary[]>([])
const heatmap = ref<HeatmapCell[]>([])
const prevMonthProjects = ref<ProjectMonthly[]>([])

const currentDate = computed(() => {
  const d = new Date()
  return d.toLocaleDateString('en-GB', {
    weekday: 'long', day: 'numeric', month: 'long', year: 'numeric'
  })
})

function trendPct(current: number, previous: number): number | null {
  if (previous <= 0) return null
  return Math.round(((current - previous) / previous) * 100)
}

const hourTrend = computed(() => {
  if (!store.summary?.trends) return null
  return trendPct(store.summary.hour?.cost ?? 0, store.summary.trends.prev_hour_cost)
})

const dayTrend = computed(() => {
  if (!store.summary?.trends) return null
  return trendPct(store.summary.today.cost, store.summary.trends.prev_day_cost)
})

const weekTrend = computed(() => {
  if (!store.summary?.trends) return null
  return trendPct(store.summary.week.cost, store.summary.trends.prev_week_cost)
})

const monthTrend = computed(() => {
  if (!store.summary?.trends) return null
  return trendPct(store.summary.month.cost, store.summary.trends.prev_month_cost)
})

// Previous calendar month's full name (e.g. "April"). Setting day=1 first
// avoids the JavaScript month-arithmetic trap where Mar 31 - 1 month = Mar 3.
const prevMonthName = computed(() => {
  const d = new Date()
  d.setDate(1)
  d.setMonth(d.getMonth() - 1)
  return d.toLocaleDateString('en-GB', { month: 'long' })
})

const prevMonthLabel = computed(() => {
  const d = new Date()
  d.setDate(1)
  d.setMonth(d.getMonth() - 1)
  return d.toLocaleDateString('en-GB', { month: 'long', year: 'numeric' })
})

const todayLabel = computed(() =>
  new Date().toLocaleDateString('en-GB', { day: 'numeric', month: 'short' }),
)

const monthLabel = computed(() =>
  new Date().toLocaleDateString('en-GB', { month: 'long' }),
)

// ISO 8601 week number: weeks run Mon–Sun and week 1 is the one containing
// the year's first Thursday. Avoids locale ambiguity around whether weeks
// start on Sunday or whether the year's first partial week is counted.
function isoWeek(d: Date): number {
  const target = new Date(d.getFullYear(), d.getMonth(), d.getDate())
  target.setDate(target.getDate() - ((target.getDay() + 6) % 7) + 3)
  const firstThursday = new Date(target.getFullYear(), 0, 4)
  return 1 + Math.round(
    ((target.getTime() - firstThursday.getTime()) / 86400000
      - 3 + ((firstThursday.getDay() + 6) % 7)) / 7,
  )
}

const weekLabel = computed(() => `Week ${isoWeek(new Date())}`)

const hourLabel = computed(() => {
  const h = new Date().getHours()
  return h.toString().padStart(2, '0') + ':00'
})

const prevHourLabel = computed(() => {
  const d = new Date()
  d.setHours(d.getHours() - 1)
  return d.getHours().toString().padStart(2, '0') + ':00'
})

// Slices for the two Overview donuts.
const costBreakdownSlices = computed(() => {
  const cb = store.summary?.cost_breakdown
  if (!cb) return []
  return [
    { label: 'Input', value: cb.input_cost, color: '#f59e0b' },
    { label: 'Output', value: cb.output_cost, color: '#fbbf24' },
    { label: 'Cache Read', value: cb.cache_read_cost, color: '#78716c' },
    { label: 'Cache Write', value: cb.cache_write_cost, color: '#44403c' },
  ]
})

// Project palette for the second donut. Same set as ModelBreakdown so the
// dashboard reads as one color story; cycles if there are more than 8 projects.
const projectColors = ['#f59e0b', '#0ea5e9', '#10b981', '#a78bfa', '#fbbf24', '#ec4899', '#78716c', '#44403c']

const prevMonthProjectSlices = computed(() =>
  prevMonthProjects.value.slice(0, 8).map((p, i) => ({
    label: p.project || '(no project)',
    value: p.cost,
    color: projectColors[i % projectColors.length],
  })),
)

const yesterdayLabel = computed(() => {
  const d = new Date()
  d.setDate(d.getDate() - 1)
  return d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short' })
})

const prevWeekLabel = computed(() => {
  const d = new Date()
  d.setDate(d.getDate() - 7)
  return `Week ${isoWeek(d)}`
})

async function openSession(id: string) {
  selectedSession.value = await fetchSession(id)
}

onMounted(async () => {
  if (!store.loaded) store.load()
  const [m, h, p] = await Promise.all([
    fetchModels(),
    fetchHeatmap(),
    fetchProjectsPrevMonth(),
  ])
  models.value = m || []
  heatmap.value = h || []
  prevMonthProjects.value = p || []
})
</script>

<style scoped>
.page-header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
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
.page-date {
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  color: var(--text-tertiary);
  padding-bottom: 4px;
}

.stat-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--space-4);
  margin-bottom: var(--space-8);
}

.charts-row {
  display: grid;
  grid-template-columns: 1fr 300px;
  gap: var(--space-5);
  margin-bottom: var(--space-8);
  align-items: stretch;
}
/* The Projected card sits in the right slot of charts-row where the donut
   used to live. Stretching its background to match DailySpendChart's height
   keeps the visual rhythm of the row. */
.charts-row .projected-slot {
  display: flex;
  flex-direction: column;
  justify-content: center;
}

.insights-row {
  display: grid;
  grid-template-columns: 340px 1fr 1fr;
  gap: var(--space-5);
  margin-bottom: var(--space-8);
  align-items: stretch;
}

.heatmap-row {
  margin-bottom: var(--space-8);
  animation: fadeSlideUp 0.45s ease both;
  animation-delay: 360ms;
}

.section-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-4);
  animation: fadeSlideUp 0.45s ease both;
  animation-delay: 360ms;
}
.section-title {
  font-size: 11px;
  font-weight: 500;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--text-tertiary);
}
.view-all {
  font-size: 12px;
  color: var(--amber-500);
  text-decoration: none;
  display: flex;
  align-items: center;
  gap: 4px;
  transition: color 150ms;
}
.view-all:hover { color: var(--amber-300); }

.sessions-table-wrap {
  background: var(--bg-surface);
  border: 1px solid var(--border-subtle);
  overflow: hidden;
  animation: fadeSlideUp 0.45s ease both;
  animation-delay: 400ms;
}
table { width: 100%; font-size: 13px; }
thead th {
  padding: var(--space-3) var(--space-5);
  text-align: left;
  font-size: 10.5px;
  font-weight: 500;
  letter-spacing: 0.1em;
  text-transform: uppercase;
  color: var(--text-tertiary);
  border-bottom: 1px solid var(--border-subtle);
  white-space: nowrap;
}
thead th.right { text-align: right; }

.top-section {
  margin-top: var(--space-8);
  animation-delay: 440ms;
}
</style>
