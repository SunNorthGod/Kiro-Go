import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { animate, motion, useReducedMotion } from 'motion/react'
import { Check, ChevronLeft, ChevronRight, Copy, Inbox } from 'lucide-react'
import { toast } from 'sonner'
import clsx from 'clsx'
import { copyText } from '@/lib/clipboard'
import { Button } from './Button'
import { Tip } from './controls'

/* ---------------- Animated number ---------------- */
/**
 * Tweens from the previous value to the new one. The text node is written
 * directly, so a 1 s poll never re-renders the tree for the animation frames.
 */
export function AnimatedNumber({
  value,
  format = (v) => Math.round(v).toLocaleString('en-US'),
  duration = 0.7,
  className,
}: {
  value: number
  format?: (v: number) => string
  duration?: number
  className?: string
}) {
  const ref = useRef<HTMLSpanElement>(null)
  // Last value actually shown; the next tween starts from here.
  const shown = useRef<number | null>(null)
  const reduce = useReducedMotion()
  const fmt = useRef(format)
  fmt.current = format

  // The span has no React children: its text is owned by this effect, so React
  // re-renders from the poll can never fight the running tween.
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const from = shown.current ?? (reduce ? value : 0)
    if (reduce || from === value) {
      shown.current = value
      el.textContent = fmt.current(value)
      return
    }
    el.textContent = fmt.current(from)
    const controls = animate(from, value, {
      duration,
      ease: [0.16, 1, 0.3, 1],
      onUpdate: (v) => {
        shown.current = v
        el.textContent = fmt.current(v)
      },
      onComplete: () => {
        shown.current = value
      },
    })
    return () => controls.stop()
  }, [value, duration, reduce])

  return <span ref={ref} className={clsx('num', className)} />
}

/* ---------------- Empty state ---------------- */
export function Empty({ icon, title, children }: { icon?: ReactNode; title?: ReactNode; children?: ReactNode }) {
  return (
    <motion.div className="empty" initial={{ opacity: 0, y: 6 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <span className="empty-icon">{icon || <Inbox />}</span>
      {title && <span className="empty-title">{title}</span>}
      {children && <span>{children}</span>}
    </motion.div>
  )
}

export function Skeleton({ w = '100%', h = 14, r }: { w?: number | string; h?: number; r?: number }) {
  return <span className="skeleton" style={{ display: 'block', width: w, height: h, borderRadius: r }} />
}

export function TableSkeleton({ rows = 5, cols = 5 }: { rows?: number; cols?: number }) {
  return (
    <div style={{ padding: '14px 16px' }} className="stack">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="row" style={{ gap: 18 }}>
          {Array.from({ length: cols }).map((__, j) => (
            <Skeleton key={j} w={j === 0 ? '28%' : `${12 + ((i + j) % 3) * 4}%`} h={12} />
          ))}
        </div>
      ))}
    </div>
  )
}

/* ---------------- Copy button ---------------- */
export function CopyButton({
  value,
  label = '复制',
  done = '已复制',
  size = 'sm',
  text,
  variant = 'ghost',
  glyph,
}: {
  value: string | (() => string)
  label?: string
  done?: string
  size?: 'sm' | 'md'
  /** show text next to the icon */
  text?: string
  variant?: 'ghost' | 'secondary'
  /** resting icon, defaults to a copy glyph */
  glyph?: ReactNode
}) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const onClick = async (e: React.MouseEvent) => {
    e.stopPropagation()
    const v = typeof value === 'function' ? value() : value
    if (!v) return
    try {
      await copyText(v)
      setCopied(true)
      toast.success(done)
      window.clearTimeout(timer.current)
      timer.current = window.setTimeout(() => setCopied(false), 1600)
    } catch {
      toast.error('复制失败，请手动选择复制')
    }
  }
  const icon = (
    <span style={{ position: 'relative', width: 14, height: 14, display: 'inline-block' }}>
      <motion.span
        style={{ position: 'absolute', inset: 0, display: 'grid', placeItems: 'center' }}
        animate={{ opacity: copied ? 0 : 1, scale: copied ? 0.5 : 1 }}
        transition={{ duration: 0.18 }}
      >
        {glyph || <Copy size={14} />}
      </motion.span>
      <motion.span
        style={{ position: 'absolute', inset: 0, display: 'grid', placeItems: 'center', color: 'var(--ok)' }}
        initial={false}
        animate={{ opacity: copied ? 1 : 0, scale: copied ? 1 : 0.5 }}
        transition={{ type: 'spring', stiffness: 600, damping: 26 }}
      >
        <Check size={14} strokeWidth={2.6} />
      </motion.span>
    </span>
  )
  if (text) {
    return (
      <Button variant={variant} size={size} icon={icon} onClick={onClick}>
        {text}
      </Button>
    )
  }
  return (
    <Tip content={label}>
      <Button variant={variant} size={size} iconOnly icon={icon} onClick={onClick}>
        {label}
      </Button>
    </Tip>
  )
}

/* ---------------- Pager ---------------- */
export function Pager({
  page,
  pageSize,
  total,
  onChange,
}: {
  page: number
  pageSize: number
  total: number
  onChange: (p: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  if (total <= pageSize && page === 1) {
    return (
      <div className="pager">
        <span>共 {total.toLocaleString('en-US')} 条</span>
      </div>
    )
  }
  return (
    <div className="pager">
      <span>共 {total.toLocaleString('en-US')} 条</span>
      <div className="row" style={{ gap: 6 }}>
        <Button size="sm" variant="ghost" iconOnly icon={<ChevronLeft />} disabled={page <= 1} onClick={() => onChange(page - 1)}>
          上一页
        </Button>
        <span className="num" style={{ minWidth: 56, textAlign: 'center' }}>
          {page} / {pages}
        </span>
        <Button size="sm" variant="ghost" iconOnly icon={<ChevronRight />} disabled={page >= pages} onClick={() => onChange(page + 1)}>
          下一页
        </Button>
      </div>
    </div>
  )
}

/* ---------------- Page transition wrapper ---------------- */
export function PageFade({ children, k }: { children: ReactNode; k: string }) {
  return (
    <motion.div
      key={k}
      initial={{ opacity: 0, y: 8 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.32, ease: [0.16, 1, 0.3, 1] }}
    >
      {children}
    </motion.div>
  )
}

/** Stagger container + item for card grids. */
export const staggerParent = {
  hidden: {},
  show: { transition: { staggerChildren: 0.045, delayChildren: 0.02 } },
}
export const staggerItem = {
  hidden: { opacity: 0, y: 10 },
  show: { opacity: 1, y: 0, transition: { duration: 0.42, ease: [0.16, 1, 0.3, 1] as const } },
}
