import { useEffect, useMemo, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Search, ScrollText, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { api, type RequestLog } from '../api'
import { qk, useAccounts, useLogs } from '../queries'
import { TopbarActions } from '../Shell'
import { Button } from '@/ui/Button'
import { Input } from '@/ui/Field'
import { Badge, Segmented, Tip } from '@/ui/controls'
import { Empty, Pager, TableSkeleton } from '@/ui/misc'
import { confirm } from '@/lib/confirm'
import { formatCompact, formatCredits, formatLogTime } from '@/lib/format'
import { useIsMobile } from '@/lib/media'

const PAGE = 100
const ERROR_TYPE: Record<string, string> = {
  quota: '配额',
  auth: '认证',
  suspended: '封禁',
  overage: '超额',
  profile: '配置',
  unknown: '未知',
}
const ENDPOINT: Record<string, string> = { claude: 'Claude', openai: 'OpenAI', responses: 'Responses' }

const logKey = (l: RequestLog) => `${l.time}|${l.accountId}|${l.model}|${l.duration}|${l.status}`

function fmtDuration(ms: number) {
  if (!ms) return '—'
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`
}

export function LogsPage() {
  const qc = useQueryClient()
  const logs = useLogs(true)
  const accounts = useAccounts()
  const [filter, setFilter] = useState<'all' | 'success' | 'error'>('all')
  const [kw, setKw] = useState('')
  const [page, setPage] = useState(1)
  const seen = useRef<Set<string> | null>(null)

  const emailOf = useMemo(() => {
    const m = new Map<string, string>()
    for (const a of accounts.data || []) m.set(a.id, a.email || a.id.slice(0, 12))
    return (id: string) => (id ? m.get(id) || id.slice(0, 8) : '—')
  }, [accounts.data])

  const all = logs.data || []
  const okCount = all.filter((l) => l.status === 'success').length
  const errCount = all.length - okCount

  const filtered = useMemo(() => {
    const q = kw.trim().toLowerCase()
    return all.filter((l) => {
      if (filter !== 'all' && l.status !== filter) return false
      if (!q) return true
      return `${l.model} ${emailOf(l.accountId)} ${l.error} ${l.endpoint}`.toLowerCase().includes(q)
    })
  }, [all, filter, kw, emailOf])

  // Stable per-row keys; identical rows get an occurrence suffix.
  const keyed = useMemo(() => {
    const seenCount = new Map<string, number>()
    return filtered.map((l) => {
      const base = logKey(l)
      const n = seenCount.get(base) || 0
      seenCount.set(base, n + 1)
      return { l, k: n ? `${base}#${n}` : base }
    })
  }, [filtered])
  const pageRows = keyed.slice((page - 1) * PAGE, page * PAGE)
  const mobile = useIsMobile()

  // Rows that were not present on the previous poll get a short highlight.
  const fresh = useMemo(() => {
    const prev = seen.current
    if (!prev) return new Set<string>()
    return new Set(all.map(logKey).filter((k) => !prev.has(k)))
  }, [all])
  useEffect(() => {
    seen.current = new Set(all.map(logKey))
  }, [all])

  const clear = async () => {
    const ok = await confirm({
      title: '清空请求日志？',
      description: '所有请求日志会被删除，无法恢复。计费账本不受影响。',
      confirmText: '清空',
      danger: true,
    })
    if (!ok) return
    try {
      await api('/logs', { method: 'DELETE' })
      qc.setQueryData(qk.logs, [])
      toast.success('日志已清空')
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  return (
    <>
      <TopbarActions>
        <Button variant="danger-soft" size="sm" icon={<Trash2 />} className="btn-collapse" onClick={clear} disabled={!all.length}>
          清空日志
        </Button>
      </TopbarActions>
      <div className="card card-flush">
        <div className="toolbar">
          <Segmented
            id="logs-filter"
            value={filter}
            onChange={(v) => {
              setFilter(v)
              setPage(1)
            }}
            items={[
              { value: 'all', label: '全部', count: all.length },
              { value: 'success', label: '成功', count: okCount },
              { value: 'error', label: '失败', count: errCount },
            ]}
          />
          <div className="search">
            <Input
              icon={<Search />}
              placeholder="搜索模型、账号或错误信息"
              value={kw}
              onChange={(e) => {
                setKw(e.target.value)
                setPage(1)
              }}
            />
          </div>
          <span className="xs muted hide-mobile" style={{ marginLeft: 'auto' }}>
            保留最近 1000 条 · 每 2 秒刷新
          </span>
        </div>
        {logs.isLoading ? (
          <TableSkeleton rows={8} cols={7} />
        ) : !filtered.length ? (
          <Empty icon={<ScrollText />} title={all.length ? '没有匹配的日志' : '暂无请求日志'}>
            {all.length ? '换个筛选条件试试' : '有请求经过网关后会出现在这里'}
          </Empty>
        ) : mobile ? (
          <>
            <div className="mlist">
              {pageRows.map(({ l, k }, idx) => {
                const err = l.status === 'error'
                return (
                  <div key={k} className={`mcard${fresh.has(k) ? ' mcard-new' : ''}`} style={{ ['--i' as string]: idx, gap: 6, padding: '11px 14px' }}>
                    <div className="row row-between" style={{ gap: 10 }}>
                      <span className="row" style={{ gap: 8, minWidth: 0 }}>
                        {err ? <Badge tone="bad">失败</Badge> : <Badge tone="ok">成功</Badge>}
                        <span className="mono xs text-2 ellipsis">{l.model || '—'}</span>
                      </span>
                      <span className="xs muted num nowrap">{formatLogTime(l.time).slice(6)}</span>
                    </div>
                    <div className="mcard-sub" style={{ marginTop: 0 }}>
                      <span className="ellipsis" style={{ maxWidth: '60%' }}>
                        {emailOf(l.accountId)}
                      </span>
                      <span>{ENDPOINT[l.endpoint] || l.endpoint || '—'}</span>
                      <span className={l.duration > 60_000 ? 't-warn' : undefined}>{fmtDuration(l.duration)}</span>
                      {!!l.tokens && <span>{formatCompact(l.tokens)} tokens</span>}
                      {!err && !!l.credits && <span className="text-2">{formatCredits(l.credits)} 积分</span>}
                    </div>
                    {err && (
                      <div className="xs t-bad" style={{ wordBreak: 'break-word', lineHeight: 1.5 }}>
                        <Badge tone="outline">{ERROR_TYPE[l.errorType || 'unknown'] || l.errorType}</Badge> {l.error}
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
            <Pager page={page} pageSize={PAGE} total={filtered.length} onChange={setPage} />
          </>
        ) : (
          <>
            <div className="table-wrap">
              <table className="table table-compact" style={{ minWidth: 980 }}>
                <thead>
                  <tr>
                    <th style={{ width: 132 }}>时间</th>
                    <th style={{ width: 70 }}>状态</th>
                    <th style={{ width: 92 }}>端点</th>
                    <th>模型</th>
                    <th>账号</th>
                    <th className="right" style={{ width: 84 }}>
                      Tokens
                    </th>
                    <th className="right" style={{ width: 84 }}>
                      耗时
                    </th>
                    <th style={{ width: '30%' }}>详情</th>
                  </tr>
                </thead>
                <tbody>
                  {pageRows.map(({ l, k }, idx) => {
                    const err = l.status === 'error'
                    return (
                      <tr key={k} className={fresh.has(k) ? 'row-new' : undefined} style={{ ['--i' as string]: idx }}>
                        <td className="num muted">{formatLogTime(l.time)}</td>
                        <td>{err ? <Badge tone="bad">失败</Badge> : <Badge tone="ok">成功</Badge>}</td>
                        <td className="xs">{ENDPOINT[l.endpoint] || l.endpoint || '—'}</td>
                        <td>
                          <span className="mono xs text-2 ellipsis" style={{ display: 'block', maxWidth: 240 }} title={l.model}>
                            {l.model || '—'}
                          </span>
                        </td>
                        <td>
                          <span className="ellipsis" style={{ display: 'block', maxWidth: 220 }} title={emailOf(l.accountId)}>
                            {emailOf(l.accountId)}
                          </span>
                        </td>
                        <td className="right num">{l.tokens ? formatCompact(l.tokens) : '—'}</td>
                        <td className={`right num${l.duration > 60_000 ? ' t-warn' : ''}`}>{fmtDuration(l.duration)}</td>
                        <td>
                          {err ? (
                            <Tip content={l.error}>
                              <span className="row" style={{ gap: 8, minWidth: 0 }}>
                                <Badge tone="outline">{ERROR_TYPE[l.errorType || 'unknown'] || l.errorType}</Badge>
                                <span className="ellipsis xs t-bad" style={{ minWidth: 0 }}>
                                  {l.error}
                                </span>
                              </span>
                            </Tip>
                          ) : l.credits ? (
                            <span className="num text-2">{formatCredits(l.credits)} 积分</span>
                          ) : (
                            <span className="faint">—</span>
                          )}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
            <Pager page={page} pageSize={PAGE} total={filtered.length} onChange={setPage} />
          </>
        )}
      </div>
    </>
  )
}
