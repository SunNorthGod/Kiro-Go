import { useEffect } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import { useActivity } from '@/lib/activity'

/**
 * Fixed decorative backdrop: two slow-drifting colour fields, a fine grid that
 * fades out from the top, and a film-grain layer that stops the gradients from
 * banding. Pure CSS, GPU-composited transforms only.
 */
export function Ambient({ variant = 'app' }: { variant?: 'app' | 'hero' }) {
  return (
    <div className={`ambient ambient-${variant}`} aria-hidden>
      <span className="ambient-glow g1" />
      <span className="ambient-glow g2" />
      {variant === 'hero' && <span className="ambient-glow g3" />}
      <span className="ambient-grid" />
      <span className="ambient-grain" />
    </div>
  )
}

/**
 * Cursor spotlight on cards: one delegated listener writes the pointer position
 * into CSS variables on the hovered card; the highlight itself is CSS.
 */
export function useCardSpotlight() {
  useEffect(() => {
    if (!window.matchMedia('(hover: hover) and (pointer: fine)').matches) return
    let frame = 0
    let last: HTMLElement | null = null
    let ev: PointerEvent | null = null
    const apply = () => {
      frame = 0
      if (!ev) return
      const card = (ev.target as HTMLElement | null)?.closest?.<HTMLElement>('.card')
      if (last && last !== card) last.removeAttribute('data-spot')
      last = card || null
      if (!card) return
      const r = card.getBoundingClientRect()
      card.style.setProperty('--mx', `${ev.clientX - r.left}px`)
      card.style.setProperty('--my', `${ev.clientY - r.top}px`)
      card.setAttribute('data-spot', '')
    }
    const onMove = (e: PointerEvent) => {
      ev = e
      if (!frame) frame = requestAnimationFrame(apply)
    }
    const onLeave = () => {
      last?.removeAttribute('data-spot')
      last = null
    }
    window.addEventListener('pointermove', onMove, { passive: true })
    document.documentElement.addEventListener('pointerleave', onLeave)
    return () => {
      window.removeEventListener('pointermove', onMove)
      document.documentElement.removeEventListener('pointerleave', onLeave)
      if (frame) cancelAnimationFrame(frame)
    }
  }, [])
}

/** Thin indeterminate bar at the very top while a save/delete is in flight. */
export function TopProgress() {
  const n = useActivity()
  return (
    <AnimatePresence>
      {n > 0 && (
        <motion.div
          className="top-progress"
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0, transition: { duration: 0.35, delay: 0.1 } }}
        >
          <span />
        </motion.div>
      )}
    </AnimatePresence>
  )
}
