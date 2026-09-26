// Number and time formatting shared by both apps. Credits are the billing unit and
// get magnitude-aware precision so a real charge is never printed as "0".

export function formatNumber(n: number | null | undefined): string {
  if (n == null || Number.isNaN(Number(n))) return '0'
  const v = Number(n)
  if (Math.abs(v) >= 1 && Math.floor(v) === v) return v.toLocaleString('en-US')
  return v.toLocaleString('en-US', { maximumFractionDigits: 1 })
}

/** Compact number: 1.7B / 21.9K / 940. */
export function formatCompact(n: number | null | undefined): string {
  const v = Number(n) || 0
  const abs = Math.abs(v)
  if (abs >= 1e9) return (v / 1e9).toFixed(abs >= 1e10 ? 0 : 1) + 'B'
  if (abs >= 1e6) return (v / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + 'M'
  if (abs >= 1e3) return (v / 1e3).toFixed(abs >= 1e4 ? 0 : 1) + 'K'
  if (Math.floor(v) === v) return String(v)
  return v.toFixed(1)
}

/**
 * Credits for a single request are routinely below 0.05. Scale the precision to
 * the magnitude so a real charge is never shown as 0; an exact 0 stays "0".
 */
export function formatCredits(value: number | null | undefined): string {
  const n = Number(value)
  if (!Number.isFinite(n) || n === 0) return '0'
  const abs = Math.abs(n)
  if (abs >= 1000) return n.toLocaleString('en-US', { maximumFractionDigits: 1 })
  if (abs >= 100) return n.toFixed(1)
  if (abs >= 1) return n.toFixed(2)
  if (abs >= 0.01) return n.toFixed(3)
  if (abs >= 0.0001) return n.toFixed(4)
  return n.toExponential(1)
}

export function formatPercent(ratio: number | null | undefined, digits = 1): string {
  if (ratio == null || !Number.isFinite(ratio)) return '—'
  return (ratio * 100).toFixed(digits) + '%'
}

const pad = (n: number) => String(n).padStart(2, '0')

/** Unix seconds → "2026-09-26 23:05". */
export function formatDateTime(ts: number | null | undefined, withSeconds = false): string {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  if (Number.isNaN(d.getTime())) return '—'
  const base = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
  return withSeconds ? `${base}:${pad(d.getSeconds())}` : base
}

/** Unix seconds → "09-26 23:05:07" (log rows). */
export function formatLogTime(ts: number): string {
  const d = new Date(ts * 1000)
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function formatDate(ts: number | null | undefined): string {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** "刚刚" / "5 分钟前" / "3 天前". */
export function formatRelTime(ts: number | null | undefined): string {
  if (!ts) return '—'
  let diff = Date.now() / 1000 - ts
  if (diff < 0) diff = 0
  if (diff < 60) return '刚刚'
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  if (diff < 86400 * 30) return `${Math.floor(diff / 86400)} 天前`
  if (diff < 86400 * 365) return `${Math.floor(diff / (86400 * 30))} 个月前`
  return `${Math.floor(diff / (86400 * 365))} 年前`
}

/** Seconds → "2 天 3 小时" / "5 小时 12 分" / "4 分". */
export function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return h > 0 ? `${d} 天 ${h} 小时` : `${d} 天`
  if (h > 0) return m > 0 ? `${h} 小时 ${m} 分` : `${h} 小时`
  if (m > 0) return `${m} 分钟`
  return `${s} 秒`
}

/** Time left until a unix-seconds deadline, compact. */
export function formatTimeLeft(ts: number | null | undefined): string {
  if (!ts) return '—'
  const diff = ts - Date.now() / 1000
  if (diff <= 0) return '已过期'
  if (diff < 3600) return `${Math.max(1, Math.floor(diff / 60))} 分钟`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时`
  return `${Math.floor(diff / 86400)} 天`
}

/** <input type="datetime-local"> value ↔ unix seconds (local time zone). */
export function toDatetimeLocal(ts: number | null | undefined): string {
  if (!ts) return ''
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function fromDatetimeLocal(value: string): number {
  if (!value) return 0
  const ms = new Date(value).getTime()
  return Number.isNaN(ms) ? 0 : Math.floor(ms / 1000)
}

/** Round a chart ceiling up to a readable step (1, 2, 2.5, 5 × 10^n). */
export function niceCeil(v: number): number {
  if (!Number.isFinite(v) || v <= 0) return 1
  const exp = Math.floor(Math.log10(v))
  const base = Math.pow(10, exp)
  const f = v / base
  const nice = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10
  return nice * base
}
