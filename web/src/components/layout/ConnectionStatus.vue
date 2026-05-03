<template>
  <div class="connection-status" role="status" aria-live="polite" :title="label">
    <div class="status-dot" :class="status"></div>
    <span class="connection-label">{{ label }}</span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { ConnectionStatus } from '../../types'

const props = defineProps<{ status: ConnectionStatus }>()

const label = computed(() => {
  switch (props.status) {
    case 'connected': return 'Live'
    case 'reconnecting': return 'Reconnecting…'
    case 'offline': return 'Offline'
  }
})
</script>

<style scoped>
.connection-status {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 12px;
  color: var(--text-secondary);
}
.status-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  flex-shrink: 0;
}
.status-dot.connected {
  background: var(--status-live);
  animation: dot-breathe 2.4s ease-in-out infinite;
}
.status-dot.reconnecting {
  background: var(--status-reconnecting);
  animation: dot-breathe 1.2s ease-in-out infinite;
}
.status-dot.offline {
  background: var(--status-offline);
}
</style>
