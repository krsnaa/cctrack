import { ref, type Ref } from 'vue'
import type { ModelRate } from '../types'
import { fetchRates } from '../api'

const version = ref('')
const updated = ref('')
const rates = ref<ModelRate[]>([])
let inflight: Promise<void> | null = null

// Module-scoped cache: every component that calls useRates() shares the same
// reactive refs and the network fetch happens at most once per page load.
async function ensureLoaded() {
  if (inflight) return inflight
  if (version.value) return
  inflight = fetchRates()
    .then((r) => {
      version.value = r.version
      updated.value = r.updated
      rates.value = r.rates
    })
    .catch(() => {
      // Leave refs empty on failure; consumers fall back to '—'.
    })
    .finally(() => {
      inflight = null
    })
  return inflight
}

export function useRates(): {
  version: Ref<string>
  updated: Ref<string>
  rates: Ref<ModelRate[]>
  load: () => Promise<void>
} {
  return { version, updated, rates, load: ensureLoaded }
}
