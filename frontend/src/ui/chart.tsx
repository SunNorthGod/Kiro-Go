import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { animate, motion, useReducedMotion } from 'motion/react'
import { formatCompact, niceCeil } from '@/lib/format'
import { useIsMobile } from '@/lib/media'

type Pt = [number, number]

/** Monotone cubic (Fritsch–Carlson) path: smooth, never overshoots the data. */
function monotonePath(pts: Pt[]): string {
  const n = pts.length
  if (n === 0) return ''
  if (n === 1) return `M${pts[0][0]},${pts[0][1]}`
  const dx: number[] = []
  const slope: number[] = []
  for (let i = 0; i < n - 1; i++) {
    dx[i] = pts[i + 1][0] - pts[i][0]
    slope[i] = (pts[i + 1][1] - pts[i][1]) / (dx[i] || 1)
  }
  const m: number[] = [slope[0]]
  for (let i = 1; i < n - 1; i++) {
    if (slope[i - 1] * slope[i] <= 0) m[i] = 0
    else {
      const w1 = 2 * dx[i] + dx[i - 1]
      const w2 = dx[i] + 2 * dx[i - 1]
      m[i] = (w1 + w2) / (w1 / slope[i - 1] + w2 / slope[i])
    }
  }
  m[n - 1] = slope[n - 2]
  let d = `M${pts[0][0].toFixed(2)},${pts[0][1].toFixed(2)}`
  for (let i = 0; i < n - 1; i++) {
    const [x0, y0] = pts[i]
    const [x1, y1] = pts[i + 1]
    const h = dx[i] / 3
    d += `C${(x0 + h).toFixed(2)},${(y0 + m[i] * h).toFixed(2)} ${(x1 - h).toFixed(2)},${(y1 - m[i + 1] * h).toFixed(2)} ${x1.toFixed(2)},${y1.toFixed(2)}`
  }
  return d
}

/** Smoothly follows a numeric target (used for the y-axis ceiling). */
function useTween(target: number, duration = 0.6) {
  const [v, setV] = useState(target)
  const cur = useRef(target)
  const reduce = useReducedMotion()
  useEffect(() => {
    if (reduce) {
      cur.current = target
      setV(target)
      return
    }
    const c = animate(cur.current, target, {
      duration,
      ease: [0.16, 1, 0.3, 1],
      onUpdate: (x) => {
        cur.current = x
        setV(x)
      },
    })
    return () => c.stop()
  }, [target, duration, reduce])
  return v
}

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null)
  const [w, setW] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    setW(el.clientWidth)
    const ro = new ResizeObserver(([e]) => setW(e.contentRect.width))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return [ref, w] as const
}

/**
 * Live streaming area chart. `samples` is a rolling window (newest last) and
 * `seq` increments with every new sample; on each increment the plot slides one
 * step left over `tick` seconds, so the line scrolls instead of jumping.
 */
export function LiveChart({
  samples,
  seq,
  capacity = 60,
  tick = 1,
  height: heightProp,
  color = 'var(--chart-1)',
  format = (v) => formatCompact(Math.round(v)),
  unit = '',
}: {
  samples: number[]
  seq: number
  capacity?: number
  tick?: number
  height?: number
  color?: string
  format?: (v: number) => string
  unit?: string
}) {
  const gid = useId().replace(/:/g, '')
  const mobile = useIsMobile()
  // Shorter plot on phones so two live charts don't eat two full screens.
  const height = heightProp ?? (mobile ? 112 : 188)
  const [wrapRef, width] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)
  const reduce = useReducedMotion()

  const padL = 44
  const padR = 10
  const padT = 12
  const padB = 12
  const innerW = Math.max(10, width - padL - padR)
  const innerH = height - padT - padB
  const step = innerW / (capacity - 1)

  const peak = samples.length ? Math.max(...samples) : 0
  // Floor of 2 keeps the 0 / ½ / 1 ticks distinct integers (a ceiling of 1
  // rendered "0, 1, 1" because the middle tick rounded 0.5 up).
  const ceil = useTween(niceCeil(Math.max(peak * 1.15, 2)))

  const n = samples.length
  // Until the window is full the line grows from the left edge (no empty left
  // half on a fresh load); once full, the newest point sits at the right edge and
  // the plot scrolls.
  const full = n >= capacity
  const xOf = (i: number) => (full ? padL + innerW - (n - 1 - i) * step : padL + i * step)
  const pts: Pt[] = useMemo(
    () => samples.map((v, i) => [xOf(i), padT + innerH - (Math.max(0, v) / ceil) * innerH] as Pt),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [samples, n, innerW, step, innerH, ceil, full],
  )
  const line = monotonePath(pts)
  const area = pts.length > 1 ? `${line}L${pts[n - 1][0].toFixed(2)},${padT + innerH}L${pts[0][0].toFixed(2)},${padT + innerH}Z` : ''
  const ticks = [0, 0.5, 1]
  const revealW = full ? innerW : n ? Math.max(0, pts[n - 1][0] - padL) + 2 : 0

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    if (!n) return
    const rect = e.currentTarget.getBoundingClientRect()
    const x = e.clientX - rect.left
    const idx = full ? n - 1 - Math.round((padL + innerW - x) / step) : Math.round((x - padL) / step)
    setHover(idx >= 0 && idx < n ? idx : null)
  }

  const hp = hover != null ? pts[hover] : null
  const ago = hover != null ? (n - 1 - hover) * tick : 0

  return (
    <div ref={wrapRef} style={{ position: 'relative', width: '100%', height }}>
      {width > 0 && (
        <svg width={width} height={height} onMouseMove={onMove} onMouseLeave={() => setHover(null)} style={{ overflow: 'visible' }}>
          <defs>
            <linearGradient id={`g${gid}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={color} stopOpacity={0.22} />
              <stop offset="100%" stopColor={color} stopOpacity={0} />
            </linearGradient>
            <clipPath id={`c${gid}`}>
              <motion.rect
                x={padL}
                y={0}
                height={height}
                initial={{ width: 0 }}
                animate={{ width: revealW }}
                transition={reduce ? { duration: 0 } : { duration: full ? 0 : tick, ease: 'linear' }}
              />
            </clipPath>
          </defs>
          {ticks.map((t) => {
            const y = padT + innerH - t * innerH
            return (
              <g key={t}>
                <line x1={padL} x2={padL + innerW} y1={y} y2={y} stroke="var(--divider)" strokeDasharray={t === 0 ? undefined : '3 4'} />
                <text x={padL - 10} y={y + 4} textAnchor="end" fontSize={11} fill="var(--faint)" className="num">
                  {format(ceil * t)}
                </text>
              </g>
            )
          })}
          <g clipPath={`url(#c${gid})`}>
            <motion.g
              key={full ? seq : 'grow'}
              initial={reduce || !full ? false : { x: step }}
              animate={{ x: 0 }}
              transition={{ duration: tick, ease: 'linear' }}
            >
              {area && <path d={area} fill={`url(#g${gid})`} />}
              {line && <path d={line} fill="none" stroke={color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />}
            </motion.g>
          </g>
          {hp && (
            <g pointerEvents="none">
              <line x1={hp[0]} x2={hp[0]} y1={padT} y2={padT + innerH} stroke="var(--border-strong)" strokeDasharray="3 3" />
              <circle cx={hp[0]} cy={hp[1]} r={4.5} fill="var(--surface)" stroke={color} strokeWidth={2} />
            </g>
          )}
        </svg>
      )}
      {hp && hover != null && (
        <div
          className="tooltip num"
          style={{
            position: 'absolute',
            left: Math.min(Math.max(hp[0] - 60, 0), width - 130),
            top: Math.max(hp[1] - 46, -6),
            pointerEvents: 'none',
            whiteSpace: 'nowrap',
            animation: 'none',
          }}
        >
          <b style={{ fontWeight: 620 }}>{format(samples[hover])}</b>
          {unit && <span style={{ opacity: 0.7 }}> {unit}</span>}
          <span style={{ opacity: 0.6 }}> · {ago === 0 ? '现在' : `${ago} 秒前`}</span>
        </div>
      )}
    </div>
  )
}

/** Small inline trend line for KPI tiles. */
export function Sparkline({ data, color = 'var(--chart-1)', width = 96, height = 32 }: { data: number[]; color?: string; width?: number; height?: number }) {
  const gid = useId().replace(/:/g, '')
  if (data.length < 2) return <svg width={width} height={height} />
  const max = Math.max(...data, 1)
  const stepX = width / (data.length - 1)
  const pts: Pt[] = data.map((v, i) => [i * stepX, height - 2 - (v / max) * (height - 4)])
  const d = monotonePath(pts)
  return (
    <svg width={width} height={height} style={{ overflow: 'visible' }} aria-hidden>
      <defs>
        <linearGradient id={`s${gid}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity={0.18} />
          <stop offset="100%" stopColor={color} stopOpacity={0} />
        </linearGradient>
      </defs>
      <path d={`${d}L${width},${height}L0,${height}Z`} fill={`url(#s${gid})`} />
      <path d={d} fill="none" stroke={color} strokeWidth={1.6} strokeLinecap="round" />
    </svg>
  )
}
