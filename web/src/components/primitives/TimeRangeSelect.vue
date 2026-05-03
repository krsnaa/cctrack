<template>
  <select class="range-select" :value="modelValue" @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value as TimeRange)">
    <option v-for="o in options" :key="o.value" :value="o.value">{{ o.label }}</option>
  </select>
</template>

<script setup lang="ts">
export type TimeRange = '7d' | '30d' | 'mtd' | 'last_month' | 'all'

defineProps<{ modelValue: TimeRange }>()
const emit = defineEmits<{ 'update:modelValue': [value: TimeRange] }>()

const options: { value: TimeRange; label: string }[] = [
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: 'mtd', label: 'This month' },
  { value: 'last_month', label: 'Last month' },
  { value: 'all', label: 'All time' },
]
</script>

<style scoped>
.range-select {
  background: transparent;
  border: 1px solid var(--border-subtle);
  color: var(--text-tertiary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 10.5px;
  padding: 2px 6px;
  cursor: pointer;
  transition: border-color 120ms, color 120ms;
}
.range-select:hover {
  border-color: var(--amber-500);
  color: var(--text-secondary);
}
.range-select:focus {
  outline: none;
  border-color: var(--amber-500);
}
.range-select option {
  background: var(--bg-elevated);
  color: var(--text-primary);
}
</style>
