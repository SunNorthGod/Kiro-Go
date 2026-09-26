import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { AnimatePresence, motion } from 'motion/react'
import { GitBranch } from 'lucide-react'
import { api, type KeyChild, type KeyUsage, type Paged, type RechargeRecord, type UsageRecord } from '../../api'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Badge, Segmented } from '@/ui/controls'
import { CopyButton, Empty, Pager, Skeleton, TableSkeleton } from '@/ui/misc'
import { formatCompact, formatCredits, formatDateTime, formatNumber, formatPercent } from '@/lib/format'
import { keyName, type NKey } from './util'

type Sub = 'summary' | 'recharges' | 'usage' | 'children'
const PAGE = 20

export function KeyDetailDialog({ k, keys, onClose, onCreateChild }: { k: NKey | null; keys: NKey[]; onClose: () => void; onCreateChild: (parentId: string) => void }) {
  const [tab, setTab] = useState<Sub>('summary')
  useEffect(() => {
    if (k) setTab('summary')
  }, [k])
  const kids = k ? keys.filter((x) => x.parentKeyId === k.id).length : 0

  return (
    <Dialog
      open={!!k}
      onOpenChange={(o) => !o && onClose()}
      title={k ? keyName(k) : ''}
      description={
        k ? (
          <span className="row" style={{ gap: 6 }}>
            <span className="mono">{k.keyMasked}</span>
            <CopyButton value={k.key || k.keyMasked} label="复制 Key" done="Key 已复制" />
          </span>
        ) : undefined
      }
      width={820}
    >
      {k && (
        <div className="stack" style={{ gap: 16 }}>
          <div>
            <Segmented
              id="key-detail-tab"
              value={tab}
              onChange={setTab}
              items={[
                { value: 'summary', label: '用量概览' },
                { value: 'usage', label: '消费明细' },
                { value: 'recharges', label: '充值记录' },
                ...(!k.parentKeyId ? [{ value: 'children' as Sub, label: '子卡', count: kids }] : []),
              ]}
            />
          </div>
          <AnimatePresence mode="wait" initial={false}>
            <motion.div key={tab} initial={{ opacity: 0, y: 6 }} animate={{ opacity: 1, y: 0 }} exit={{ opacity: 0, y: -4 }} transition={{ duration: 0.18 }}>
              {tab === 'summary' && <Summary k={k} />}
              {tab === 'usage' && <UsageRecords id={k.id} />}
              {tab === 'recharges' && <Recharges id={k.id} />}
              {tab === 'children' && <Children k={k} onCreate={() => onCreateChild(k.id)} />}
            </motion.div>
          </AnimatePresence>
        </div>
      )}
    </Dialog>
  )
}

function hitTone(r: number | null) {
  if (r == null) return undefined
  return r >= 0.6 ? 't-ok' : r >= 0.3 ? 't-warn' : 't-bad'
}

function Summary({ k }: { k: NKey }) {
  const q = useQuery({ queryKey: ['key-usage', k.id], queryFn: () => api<KeyUsage>(`/api-keys/${encodeURIComponent(k.id)}/usage`) })
  const d = q.data
  const granted = d?.creditsGranted ?? k.granted
  const used = d?.creditsUsed ?? k.used
  const balance = d?.balance ?? k.balance
  const hit = typeof d?.cacheHitRate === 'number' ? d.cacheHitRate : null
  const items: [string, string, string?][] = [
    ['余额', granted > 0 ? formatNumber(Math.round(balance * 100) / 100) : '∞', balance < 0 ? 't-bad' : 't-ok'],
    ['额度', granted > 0 ? formatNumber(granted) : '不限额'],
    ['已用积分', formatCredits(used)],
    ['Tokens', formatCompact(d?.tokensUsed ?? k.tokensUsed)],
    ['请求', formatNumber(d?.requestsCount ?? k.requestsCount)],
    ['缓存命中率', formatPercent(hit), hitTone(hit)],
  ]
  const byModel = d?.byModel || []
  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="summary-grid">
        {items.map(([l, v, tone]) => (
          <div key={l} className="summary-item">
            <div className="l">{l}</div>
            <div className={`v ${tone || ''}`}>{q.isLoading ? <Skeleton w={60} h={18} /> : v}</div>
          </div>
        ))}
      </div>
      <div>
        <div className="section-title">按模型</div>
        {q.isLoading ? (
          <TableSkeleton rows={3} cols={4} />
        ) : !byModel.length ? (
          <div className="xs muted">暂无按模型统计</div>
        ) : (
          <div className="card card-flush" style={{ boxShadow: 'none' }}>
            <div className="table-wrap">
              <table className="table table-compact">
                <thead>
                  <tr>
                    <th>模型</th>
                    <th className="right">积分</th>
                    <th className="right">请求</th>
                    <th className="right">输入</th>
                    <th className="right">输出</th>
                    <th className="right">缓存读取</th>
                  </tr>
                </thead>
                <tbody>
                  {byModel.map((m) => (
                    <tr key={m.model}>
                      <td className="mono xs text-2">{m.model}</td>
                      <td className="right num strong">{formatCredits(m.credits)}</td>
                      <td className="right num">{formatNumber(m.requests)}</td>
                      <td className="right num">{formatCompact(m.inputTokens || 0)}</td>
                      <td className="right num">{formatCompact(m.outputTokens || 0)}</td>
                      <td className="right num">{formatCompact(m.cacheReadInputTokens || 0)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

function usePaged<T>(key: string, id: string, path: string, page: number) {
  return useQuery({
    queryKey: [key, id, page],
    queryFn: () => api<Paged<T>>(`/api-keys/${encodeURIComponent(id)}${path}?page=${page}&pageSize=${PAGE}`),
    placeholderData: (prev) => prev,
  })
}

function UsageRecords({ id }: { id: string }) {
  const [page, setPage] = useState(1)
  const q = usePaged<UsageRecord>('key-records', id, '/usage/records', page)
  const rows = q.data?.records || []
  if (q.isLoading) return <TableSkeleton rows={6} cols={6} />
  if (q.isError) return <Empty title="明细加载失败">{(q.error as Error).message}</Empty>
  if (!rows.length) return <Empty title="暂无消费记录">明细需要数据库模式，JSON 模式下不记录</Empty>
  return (
    <div className="card card-flush" style={{ boxShadow: 'none', opacity: q.isPlaceholderData ? 0.6 : 1, transition: 'opacity .15s' }}>
      <div className="table-wrap">
        <table className="table table-compact">
          <thead>
            <tr>
              <th>时间</th>
              <th>模型</th>
              <th className="right">输入</th>
              <th className="right">输出</th>
              <th className="right">缓存读取</th>
              <th className="right">积分</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={`${r.createdAt}-${i}`}>
                <td className="num muted nowrap">{formatDateTime(r.createdAt, true)}</td>
                <td className="mono xs text-2">{r.model || '—'}</td>
                <td className="right num">{formatNumber(r.inputTokens)}</td>
                <td className="right num">{formatNumber(r.outputTokens)}</td>
                <td className="right num">{r.cacheReadInputTokens ? formatNumber(r.cacheReadInputTokens) : <span className="faint">0</span>}</td>
                <td className="right num strong">{formatCredits(r.credits)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <Pager page={page} pageSize={PAGE} total={q.data?.total || 0} onChange={setPage} />
    </div>
  )
}

function Recharges({ id }: { id: string }) {
  const [page, setPage] = useState(1)
  const q = usePaged<RechargeRecord>('key-recharges', id, '/recharges', page)
  const rows = q.data?.records || []
  if (q.isLoading) return <TableSkeleton rows={4} cols={5} />
  if (q.isError) return <Empty title="充值记录加载失败">{(q.error as Error).message}</Empty>
  if (!rows.length) return <Empty title="暂无充值记录" />
  return (
    <div className="card card-flush" style={{ boxShadow: 'none', opacity: q.isPlaceholderData ? 0.6 : 1, transition: 'opacity .15s' }}>
      <div className="table-wrap">
        <table className="table table-compact">
          <thead>
            <tr>
              <th>时间</th>
              <th className="right">充值</th>
              <th className="right">充值后总额度</th>
              <th>操作人</th>
              <th>备注</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={`${r.createdAt}-${i}`}>
                <td className="num muted nowrap">{formatDateTime(r.createdAt)}</td>
                <td className="right num t-ok strong">+{formatNumber(r.amount)}</td>
                <td className="right num">{formatNumber(r.balanceAfter)}</td>
                <td className="xs">{r.operator === 'admin' ? '管理员' : r.operator?.startsWith('reseller:') ? `父卡 ${r.operator.slice(9)}` : r.operator || '—'}</td>
                <td className="xs muted">{r.note || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <Pager page={page} pageSize={PAGE} total={q.data?.total || 0} onChange={setPage} />
    </div>
  )
}

function Children({ k, onCreate }: { k: NKey; onCreate: () => void }) {
  const q = useQuery({
    queryKey: ['key-children', k.id],
    queryFn: () => api<{ children: KeyChild[] }>(`/api-keys/${encodeURIComponent(k.id)}/children`).then((d) => d.children || []),
  })
  const rows = q.data || []
  const allocated = rows.reduce((s, c) => s + (c.creditsGranted || 0), 0)
  return (
    <div className="stack" style={{ gap: 12 }}>
      <div className="row row-between">
        <span className="xs muted">
          已分配 <b className="num text-2">{formatNumber(allocated)}</b>
          {k.granted > 0 && (
            <>
              {' '}
              / 父卡额度 <b className="num text-2">{formatNumber(k.granted)}</b>
            </>
          )}
        </span>
        <Button size="sm" variant="primary" icon={<GitBranch />} onClick={onCreate}>
          开一张子卡
        </Button>
      </div>
      {q.isLoading ? (
        <TableSkeleton rows={3} cols={5} />
      ) : !rows.length ? (
        <Empty title="还没有子卡">子卡从这张卡的额度池里划拨积分，适合代理商分销</Empty>
      ) : (
        <div className="card card-flush" style={{ boxShadow: 'none' }}>
          <div className="table-wrap">
            <table className="table table-compact">
              <thead>
                <tr>
                  <th>名称</th>
                  <th className="right">额度</th>
                  <th className="right">已用</th>
                  <th className="right">余额</th>
                  <th className="right">占父卡</th>
                  <th className="right">请求</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((c) => (
                  <tr key={c.id} data-dim={!c.enabled}>
                    <td>
                      <span className="row" style={{ gap: 6 }}>
                        {c.name || '未命名'}
                        {!c.enabled && <Badge tone="bad">已禁用</Badge>}
                      </span>
                    </td>
                    <td className="right num">{c.creditsGranted > 0 ? formatNumber(c.creditsGranted) : '不限额'}</td>
                    <td className="right num">{formatCredits(c.creditsUsed)}</td>
                    <td className={`right num ${c.balance < 0 ? 't-bad' : ''}`}>{c.creditsGranted > 0 ? formatNumber(Math.round(c.balance * 10) / 10) : '∞'}</td>
                    <td className="right num">{k.granted > 0 ? formatPercent(c.creditsGranted / k.granted) : '—'}</td>
                    <td className="right num">{formatNumber(c.requestsCount)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  )
}
