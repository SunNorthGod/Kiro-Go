import { motion } from 'motion/react'
import { Activity, CalendarClock, Coins, DatabaseZap, Gauge, Hash } from 'lucide-react'
import type { Me, Usage } from '../api'
import { AnimatedNumber, Skeleton, staggerItem, staggerParent } from '@/ui/misc'
import { Badge, Bar, usageTone } from '@/ui/controls'
import { formatCompact, formatCredits, formatDateTime, formatNumber, formatPercent, formatRelTime } from '@/lib/format'
import { useIsMobile } from '@/lib/media'

export function OverviewTab({ me, usage, loading }: { me?: Me; usage?: Usage; loading: boolean }) {
  const mobile = useIsMobile()
  if (loading || !me) {
    return (
      <div className="stack" style={{ gap: 16 }}>
        <div className="card" style={{ padding: 22 }}>
          <Skeleton w={90} h={12} />
          <div style={{ height: 14 }} />
          <Skeleton w={180} h={38} />
          <div style={{ height: 18 }} />
          <Skeleton h={8} />
        </div>
        <div className="grid grid-4">
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="card" style={{ padding: 18 }}>
              <Skeleton w={70} h={12} />
              <div style={{ height: 12 }} />
              <Skeleton w={90} h={22} />
            </div>
          ))}
        </div>
      </div>
    )
  }

  const granted = me.creditsGranted > 0 ? me.creditsGranted : usage?.creditsGranted && usage.creditsGranted > 0 ? usage.creditsGranted : me.creditLimit > 0 ? me.creditLimit : 0
  const used = usage?.creditsUsed ?? me.creditsUsed ?? 0
  const balance = usage?.balance ?? me.balance ?? (granted > 0 ? granted - used : 0)
  const tokens = usage?.tokensUsed ?? me.tokensUsed ?? 0
  const requests = usage?.requestsCount ?? me.requestsCount ?? 0
  const limited = granted > 0
  const ratio = limited ? Math.min(1, Math.max(0, used / granted)) : 0
  const hit = typeof usage?.cacheHitRate === 'number' ? usage.cacheHitRate : null
  const byModel = usage?.byModel || []
  const maxModel = Math.max(1e-9, ...byModel.map((m) => m.credits || 0))
  const daysLeft = me.expiresAt ? Math.ceil((me.expiresAt - Date.now() / 1000) / 86400) : null

  return (
    <motion.div className="stack" style={{ gap: 16 }} variants={staggerParent} initial="hidden" animate="show">
      <div className="grid hero-grid">
        <motion.div variants={staggerItem} className="card balance-hero">
          <div className="row row-between">
            <span className="kpi-label">
              <span className="kpi-icon ok">
                <Coins />
              </span>
              可用余额
            </span>
            <span className="row" style={{ gap: 6 }}>
              {me.isParent && <Badge tone="accent">父卡</Badge>}
              <Badge tone="ok">正常</Badge>
            </span>
          </div>
          <div className="big" style={{ marginTop: 16, color: limited && balance < 0 ? 'var(--bad)' : undefined }}>
            {limited ? <AnimatedNumber value={balance} format={(v) => formatNumber(Math.round(v * 100) / 100)} /> : '不限额'}
            {limited && <small>积分</small>}
          </div>
          {limited ? (
            <div style={{ marginTop: 18 }}>
              <Bar value={ratio} tone={usageTone(ratio)} />
              <div className="row row-between xs muted" style={{ marginTop: 8 }}>
                <span className="num">
                  已用 {formatNumber(Math.round(used * 100) / 100)} / 总额度 {formatNumber(granted)}
                </span>
                <span className="num">{(ratio * 100).toFixed(1)}%</span>
              </div>
            </div>
          ) : (
            <div className="xs muted" style={{ marginTop: 14 }}>
              已用 <span className="num text-2">{formatCredits(used)}</span> 积分
            </div>
          )}
        </motion.div>

        <motion.div variants={staggerItem} className="card" style={{ padding: '10px 18px' }}>
          <div className="kv">
            <span className="muted">名称</span>
            <b>{me.name || '未命名'}</b>
          </div>
          <div className="kv">
            <span className="muted">到期</span>
            <b className={daysLeft != null && daysLeft <= 3 ? 't-warn' : undefined}>
              {me.expiresAt ? (
                <>
                  {formatDateTime(me.expiresAt)}
                  {daysLeft != null && daysLeft >= 0 && <em> · 剩 {daysLeft} 天</em>}
                </>
              ) : (
                '永久有效'
              )}
            </b>
          </div>
          <div className="kv">
            <span className="muted">并发上限</span>
            <b>{me.maxConcurrency == null ? '系统默认' : me.maxConcurrency === 0 ? '不限制' : me.maxConcurrency}</b>
          </div>
          <div className="kv">
            <span className="muted">每分钟请求上限</span>
            <b>{me.maxRPM == null ? '系统默认' : me.maxRPM === 0 ? '不限制' : me.maxRPM}</b>
          </div>
          <div className="kv">
            <span className="muted">最近使用</span>
            <b>{me.lastUsedAt ? formatRelTime(me.lastUsedAt) : '还没用过'}</b>
          </div>
          <div className="kv">
            <span className="muted">开通时间</span>
            <b>{formatDateTime(me.createdAt)}</b>
          </div>
        </motion.div>
      </div>

      <div className="grid grid-4">
        <Tile icon={<Gauge />} tone="accent" label="已用积分" value={<AnimatedNumber value={used} format={(v) => formatCredits(v)} />} />
        <Tile
          icon={<Hash />}
          tone="teal"
          label="已用 Tokens"
          value={<AnimatedNumber value={tokens} format={(v) => formatCompact(Math.round(v))} />}
          hint={tokens >= 1000 ? formatNumber(tokens) : undefined}
        />
        <Tile icon={<Activity />} tone="pink" label="请求次数" value={<AnimatedNumber value={requests} />} />
        <Tile
          icon={<DatabaseZap />}
          tone="ok"
          label="缓存命中率"
          value={hit == null ? '—' : <AnimatedNumber value={hit * 100} format={(v) => v.toFixed(1) + '%'} />}
          hint={hit == null ? '还没有可统计的输入' : hit >= 0.6 ? '命中率不错' : '长对话里命中率会逐步升高'}
          valueTone={hit == null ? undefined : hit >= 0.6 ? 't-ok' : hit >= 0.3 ? 't-warn' : 't-bad'}
        />
      </div>

      <motion.div variants={staggerItem} className="card card-flush">
        <div className="card-header">
          <span className="card-title">
            <CalendarClock />
            按模型消耗
          </span>
          <span className="card-actions xs muted">积分是计费单位，Token 仅供参考</span>
        </div>
        {!byModel.length ? (
          <div className="empty">还没有按模型的用量</div>
        ) : mobile ? (
          <div className="mlist">
            {byModel.map((m, i) => (
              <div key={m.model} className="mcard" style={{ ['--i' as string]: i, gap: 7, padding: '12px 14px' }}>
                <div className="row row-between" style={{ gap: 10 }}>
                  <span className="mono xs text-2 ellipsis">{m.model}</span>
                  <span className="strong num nowrap">{formatCredits(m.credits)}</span>
                </div>
                <Bar value={(m.credits || 0) / maxModel} thin />
                <div className="mcard-sub" style={{ marginTop: 0 }}>
                  <span>{formatNumber(m.requests)} 次</span>
                  <span>输入 {formatCompact(m.inputTokens || 0)}</span>
                  <span>输出 {formatCompact(m.outputTokens || 0)}</span>
                  {used > 0 && <span>占 {formatPercent((m.credits || 0) / used, 0)}</span>}
                </div>
              </div>
            ))}
          </div>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>模型</th>
                  <th style={{ width: '32%' }}>占比</th>
                  <th className="right">积分</th>
                  <th className="right">请求</th>
                  <th className="right">输入</th>
                  <th className="right">输出</th>
                </tr>
              </thead>
              <tbody>
                {byModel.map((m) => (
                  <tr key={m.model}>
                    <td className="mono xs text-2">{m.model}</td>
                    <td>
                      <div className="row" style={{ gap: 10 }}>
                        <div className="grow">
                          <Bar value={(m.credits || 0) / maxModel} thin />
                        </div>
                        <span className="xs muted num" style={{ width: 44, textAlign: 'right' }}>
                          {used > 0 ? formatPercent((m.credits || 0) / used, 0) : '—'}
                        </span>
                      </div>
                    </td>
                    <td className="right num strong">{formatCredits(m.credits)}</td>
                    <td className="right num">{formatNumber(m.requests)}</td>
                    <td className="right num">{formatCompact(m.inputTokens || 0)}</td>
                    <td className="right num">{formatCompact(m.outputTokens || 0)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </motion.div>
    </motion.div>
  )
}

function Tile({
  icon,
  tone,
  label,
  value,
  hint,
  valueTone,
}: {
  icon: React.ReactNode
  tone: 'accent' | 'teal' | 'pink' | 'ok'
  label: string
  value: React.ReactNode
  hint?: string
  valueTone?: string
}) {
  return (
    <motion.div variants={staggerItem} className="card kpi">
      <div className="kpi-label">
        <span className={`kpi-icon ${tone}`}>{icon}</span>
        {label}
      </div>
      <div className={`kpi-value ${valueTone || ''}`} style={{ fontSize: 24 }}>
        {value}
      </div>
      {hint && <div className="kpi-foot">{hint}</div>}
    </motion.div>
  )
}
