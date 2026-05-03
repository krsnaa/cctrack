import { ref, watch, type Ref } from 'vue'

export function useCountUp(target: Ref<number>, duration = 800) {
  const display = ref(0)
  let raf: number
  let firstRun = true

  watch(target, (to) => {
    // Snap on the first invocation rather than animating 0 → target. The
    // 0 → x animation passes through values < $0.01 for a meaningful chunk
    // of its duration, which the StatCard formatter renders as $0.0000 —
    // looks like the card is broken on initial paint, especially for small
    // values like the Hour bucket. Live updates after mount still animate.
    if (firstRun) {
      firstRun = false
      display.value = to
      return
    }
    const from = display.value
    const start = performance.now()
    cancelAnimationFrame(raf)

    function tick(now: number) {
      const elapsed = now - start
      const progress = Math.min(elapsed / duration, 1)
      const ease = 1 - Math.pow(1 - progress, 4) // easeOutQuart
      display.value = from + (to - from) * ease
      if (progress < 1) raf = requestAnimationFrame(tick)
    }

    raf = requestAnimationFrame(tick)
  }, { immediate: true })

  return display
}
