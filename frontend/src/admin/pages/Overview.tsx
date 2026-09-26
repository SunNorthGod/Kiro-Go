import { useEffect, useSyncExternalStore, type ReactNode } from 'react'
import { motion } from 'motion/react'
import { Activity, ArrowRight, CalendarDays, CircleCheck, Coins, DatabaseZap, Gauge, KeyRound, Link2, Users, Waves } from 'lucide-react'
import { useOverview } from '../queries'
import { TopbarActions, useTab } from '../Shell'
import type { Overview } from '../api'
import { AnimatedNumber, Skeleton, staggerItem, staggerParent } from '@/ui/misc'
import { LiveChart } from '@/ui/chart'
import { Badge, Bar, Dot } from '@/ui/controls'
import { Button } from '@/ui/Button'
import { formatCompact, formatCredits, formatNumber, formatPercent } from '@/lib/format'
import { useIsMobile } from '@/lib/media'

/* Rolling client-side samples of the realtime RPM / TPM figures. Kept at module
 * scope so the trend survives switching tabs. */
const CAP = 60
const RING_KEY = 'kg_ov_ring'
// A page refresh keeps the trend if the saved window is at most 20 s old.
function restoreRing() {
  try {
    const s = JSON.parse(sessionStorage.getItem(RING_KEY) || 'null') as { t: number; rpm: number[]; tpm: number[] } | null
    if (s && Date.now() - s.t < 20_000 && Array.isArray(s.rpm) && Array.isArray(s.tpm)) {
      return { rpm: s.rpm.slice(-CAP), tpm: s.tpm.slice(-CAP) }
    }
  } catch {
    /* ignore */
  }
  return { rpm: [] as number[], tpm: [] as number[] }
}
const restored = restoreRing()
const ring = { rpm: restored.rpm, tpm: restored.tpm, seq: 0, last: 0 }
const ringListeners = new Set<() => void>()
let snapshot = { rpm: ring.rpm, tpm: ring.tpm, seq: 0 }
function pushSample(rpm: number, tpm: number, at: number) {
  if (at === ring.last) return
  ring.last = at
  ring.rpm = [...ring.rpm, rpm].slice(-CAP)
  ring.tpm = [...ring.tpm, tpm].slice(-CAP)
  ring.seq++
  snapshot = { rpm: ring.rpm, tpm: ring.tpm, seq: ring.seq }
  try {
    sessionStorage.setItem(RING_KEY, JSON.stringify({ t: Date.now(), rpm: ring.rpm, tpm: ring.tpm }))
  } catch {
    /* ignore */
  }
  ringListeners.forEach((l) => l())
}
function useSamples() {
  return useSyncExternalStore(
    (l) => {
      ringListeners.add(l)
      return () => ringListeners.delete(l)
    },
    () => snapshot,
  )
}

export function OverviewPage() {
  const { data, dataUpdatedAt, isError, error } = useOverview(true)
  const samples = useSamples()

  useEffect(() => {
    if (data) pushSample(data.totalRPM || 0, Math.round(data.totalTPM || 0), dataUpdatedAt)
  }, [data, dataUpdatedAt])

  return (
    <>
      <TopbarActions>
        <span className="live-chip">
          <Dot tone={isError ? 'bad' : 'ok'} live={!isError} />
          {isError ? '连接中断' : '实时 · 每秒更新'}
        </span>
      </TopbarActions>
      {isError && !data ? (
        <div className="callout callout-bad">
          <Activity />
          概览数据加载失败：{(error as Error)?.message}
        </div>
      ) : !data ? (
        <OverviewSkeleton />
      ) : (
        <OverviewBody d={data} samples={samples} />
      )}
    </>
  )
}

function OverviewBody({ d, samples }: { d: Overview; samples: { rpm: number[]; tpm: number[]; seq: number } }) {
  const { go } = useTab()
  const total = d.totalRequests || 0
  const ok = d.successRequests || 0
  const rate = total > 0 ? ok / total : null
  const daily = Array.isArray(d.daily) ? d.daily : []
  const today = daily.length ? daily[daily.length - 1] : null
  const pc = d.promptCache || ({} as Overview['promptCache'])
  const sticky = (d.cache?.stickyHits || 0) + (d.cache?.stickyMisses || 0)
  const stickyRate = sticky > 0 ? (d.cache.stickyHits || 0) / sticky : null
  const peakRpm = samples.rpm.length ? Math.max(...samples.rpm) : 0
  const peakTpm = samples.tpm.length ? Math.max(...samples.tpm) : 0

  return (
    <motion.div className="stack" style={{ gap: 16 }} variants={staggerParent} initial="hidden" animate="show">
      <div className="grid grid-2">
        <motion.div variants={staggerItem} className="card">
          <ChartHead
            icon={<Gauge />}
            label="实时 RPM"
            value={<AnimatedNumber value={d.totalRPM || 0} />}
            unit="次/分钟"
            aside={
              <>
                <MiniKV label="进行中" value={d.concurrency?.inflight || 0} />
                <MiniKV label="窗口峰值" value={peakRpm} />
              </>
            }
          />
          <div className="chart-body">
            <LiveChart samples={samples.rpm} seq={samples.seq} capacity={CAP} color="var(--chart-1)" unit="RPM" />
          </div>
        </motion.div>
        <motion.div variants={staggerItem} className="card">
          <ChartHead
            icon={<Waves />}
            label="实时 TPM"
            value={<AnimatedNumber value={Math.round(d.totalTPM || 0)} />}
            unit="tokens/分钟"
            aside={
              <>
                <MiniKV label="累计 Tokens" value={d.totalTokens || 0} compact />
                <MiniKV label="窗口峰值" value={peakTpm} compact />
              </>
            }
          />
          <div className="chart-body">
            <LiveChart samples={samples.tpm} seq={samples.seq} capacity={CAP} color="var(--chart-2)" unit="tokens" />
          </div>
        </motion.div>
      </div>

      <div className="grid grid-4">
        <Kpi icon={<Activity />} tone="accent" label="累计请求" value={<AnimatedNumber value={total} />}>
          成功 <b>{formatNumber(ok)}</b> · 失败 <b>{formatNumber(d.failedRequests || 0)}</b>
        </Kpi>
        <Kpi
          icon={<CircleCheck />}
          tone="ok"
          label="成功率"
          value={rate == null ? '—' : <AnimatedNumber value={rate * 100} format={(v) => v.toFixed(1)} />}
          unit={rate == null ? undefined : '%'}
        >
          <div style={{ width: '100%' }}>
            <Bar value={rate ?? 0} tone={rate == null ? 'accent' : rate >= 0.95 ? 'ok' : rate >= 0.8 ? 'warn' : 'bad'} thin />
          </div>
        </Kpi>
        <Kpi icon={<Coins />} tone="warn" label="今日积分" value={<AnimatedNumber value={today?.credits || 0} format={(v) => v.toFixed(1)} />}>
          今日请求 <b>{formatNumber(today?.requests || 0)}</b> · 累计 <b>{formatCompact(d.totalCredits || 0)}</b>
        </Kpi>
        <Kpi
          icon={<DatabaseZap />}
          tone="teal"
          label={pc.windowDays ? `缓存命中率 · ${pc.windowDays} 天` : '缓存命中率 · 累计'}
          value={typeof pc.hitRate === 'number' ? <AnimatedNumber value={pc.hitRate * 100} format={(v) => v.toFixed(1)} /> : '—'}
          unit={typeof pc.hitRate === 'number' ? '%' : undefined}
        >
          读取 <b>{formatCompact(pc.readTokens || 0)}</b> / 输入 <b>{formatCompact(pc.inputTokens || 0)}</b>
        </Kpi>
      </div>

      <div className="grid grid-3">
        <GroupCard icon={<Users />} title="账号" onMore={() => go('accounts')}>
          <div className="kv">
            可用 / 总数
            <b>
              {d.accounts?.available || 0} <em>/ {d.accounts?.total || 0}</em>
            </b>
          </div>
          <div className="kv">
            已启用<b>{d.accounts?.enabled || 0}</b>
          </div>
          <div className="kv">
            已禁用<b className={d.accounts?.disabled ? 't-warn' : undefined}>{d.accounts?.disabled || 0}</b>
          </div>
          <div className="kv">
            正在服务<b>{d.concurrency?.activeAccounts || 0}</b>
          </div>
        </GroupCard>
        <GroupCard icon={<KeyRound />} title="卡密" onMore={() => go('keys')}>
          <div className="kv">
            生效 / 总数
            <b>
              {d.keys?.active || 0} <em>/ {d.keys?.total || 0}</em>
            </b>
          </div>
          <div className="kv">
            正在调用<b>{d.concurrency?.activeKeys || 0}</b>
          </div>
          <div style={{ padding: '12px 0 4px' }}>
            <div className="row row-between xs muted" style={{ marginBottom: 6 }}>
              <span>生效占比</span>
              <span className="num">{d.keys?.total ? formatPercent((d.keys.active || 0) / d.keys.total, 0) : '—'}</span>
            </div>
            <Bar value={d.keys?.total ? (d.keys.active || 0) / d.keys.total : 0} />
          </div>
        </GroupCard>
        <GroupCard icon={<Link2 />} title="会话粘性">
          <div className="kv">
            粘性会话<b>{formatNumber(d.cache?.stickySessions || 0)}</b>
          </div>
          <div className="kv">
            命中<b>{formatNumber(d.cache?.stickyHits || 0)}</b>
          </div>
          <div className="kv">
            未命中<b>{formatNumber(d.cache?.stickyMisses || 0)}</b>
          </div>
          <div className="kv">
            命中率<b className={stickyRate != null && stickyRate >= 0.9 ? 't-ok' : undefined}>{formatPercent(stickyRate)}</b>
          </div>
        </GroupCard>
      </div>

      <motion.div variants={staggerItem} className="card card-flush">
        <div className="card-header">
          <span className="card-title">
            <CalendarDays />
            每日统计
          </span>
          <span className="card-actions xs muted">近 {daily.length} 天 · 按北京时间</span>
        </div>
        <DailyTable daily={daily} />
      </motion.div>
    </motion.div>
  )
}

function ChartHead({ icon, label, value, unit, aside }: { icon: ReactNode; label: string; value: ReactNode; unit: string; aside?: ReactNode }) {
  return (
    <div className="chart-head">
      <div>
        <div className="kpi-label">
          <span className="kpi-icon accent">{icon}</span>
          {label}
        </div>
        <div className="chart-value">
          {value}
          <small>{unit}</small>
        </div>
      </div>
      <div className="row" style={{ gap: 20 }}>
        {aside}
      </div>
    </div>
  )
}

function MiniKV({ label, value, compact }: { label: string; value: number | null; compact?: boolean }) {
  if (value == null) return null
  return (
    <div style={{ textAlign: 'right' }}>
      <div className="xs muted">{label}</div>
      <div className="num strong" style={{ fontSize: 15, marginTop: 2 }}>
        {compact ? formatCompact(value) : formatNumber(value)}
      </div>
    </div>
  )
}

function Kpi({
  icon,
  tone,
  label,
  value,
  unit,
  children,
}: {
  icon: ReactNode
  tone: 'accent' | 'ok' | 'warn' | 'teal' | 'pink'
  label: string
  value: ReactNode
  unit?: string
  children?: ReactNode
}) {
  return (
    <motion.div variants={staggerItem} className="card kpi">
      <div className="kpi-label">
        <span className={`kpi-icon ${tone}`}>{icon}</span>
        {label}
      </div>
      <div className="kpi-value">
        {value}
        {unit && <small>{unit}</small>}
      </div>
      {children && <div className="kpi-foot">{children}</div>}
    </motion.div>
  )
}

function GroupCard({ icon, title, onMore, children }: { icon: ReactNode; title: string; onMore?: () => void; children: ReactNode }) {
  return (
    <motion.div variants={staggerItem} className={onMore ? 'card card-lift' : 'card'}>
      <div className="card-header">
        <span className="card-title">
          {icon}
          {title}
        </span>
        {onMore && (
          <div className="card-actions">
            <Button size="sm" variant="ghost" onClick={onMore} icon={<ArrowRight />} style={{ flexDirection: 'row-reverse' }}>
              查看
            </Button>
          </div>
        )}
      </div>
      <div style={{ padding: '4px 18px 10px' }}>{children}</div>
    </motion.div>
  )
}

function DailyTable({ daily }: { daily: Overview['daily'] }) {
  const mobile = useIsMobile()
  if (!daily.length) {
    return <div className="empty">暂无历史数据</div>
  }
  const rows = [...daily].reverse()
  const maxReq = Math.max(1, ...rows.map((r) => r.requests || 0))
  if (mobile) {
    return (
      <div className="mlist">
        {rows.map((r, i) => (
          <div key={r.date} className="mcard" style={{ ['--i' as string]: i, gap: 8, padding: '11px 14px' }}>
            <div className="row row-between">
              <span className="strong num" style={{ fontSize: 13 }}>
                {r.date.slice(5)}
                {i === 0 && (
                  <span style={{ marginLeft: 8 }}>
                    <Badge tone="accent">今天</Badge>
                  </span>
                )}
              </span>
              <span className="xs muted num">
                请求 <b className="text-2">{formatNumber(r.requests || 0)}</b> · 积分 <b className="text-2">{formatCredits(r.credits)}</b>
              </span>
            </div>
            <Bar value={(r.requests || 0) / maxReq} thin />
          </div>
        ))}
      </div>
    )
  }
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>日期</th>
            <th className="right">请求</th>
            <th className="right">积分</th>
            <th className="right">Tokens</th>
            <th style={{ width: '30%' }}>请求量</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={r.date} style={{ ['--i' as string]: i }}>
              <td className="strong num">
                <span className="row" style={{ gap: 8 }}>
                  {r.date}
                  {i === 0 && <Badge tone="accent">今天</Badge>}
                </span>
              </td>
              <td className="right num">{formatNumber(r.requests || 0)}</td>
              <td className="right num">{formatCredits(r.credits)}</td>
              <td className="right num">{formatCompact(r.tokens || 0)}</td>
              <td>
                <Bar value={(r.requests || 0) / maxReq} thin />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function OverviewSkeleton() {
  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="grid grid-2">
        {[0, 1].map((i) => (
          <div key={i} className="card" style={{ padding: 18, height: 280 }}>
            <Skeleton w={90} h={12} />
            <div style={{ height: 12 }} />
            <Skeleton w={120} h={28} />
            <div style={{ height: 24 }} />
            <Skeleton h={170} r={10} />
          </div>
        ))}
      </div>
      <div className="grid grid-4">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="card" style={{ padding: 18 }}>
            <Skeleton w={80} h={12} />
            <div style={{ height: 14 }} />
            <Skeleton w={100} h={26} />
          </div>
        ))}
      </div>
    </div>
  )
}
