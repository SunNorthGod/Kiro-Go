import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ReceiptText, Wallet } from 'lucide-react'
import { uapi, type Paged, type RechargeRecord, type UsageRecord } from '../api'
import { Empty, Pager, TableSkeleton } from '@/ui/misc'
import { formatCredits, formatDateTime, formatNumber } from '@/lib/format'
import { useIsMobile } from '@/lib/media'

const PAGE = 20

function usePaged<T>(path: string, page: number) {
  return useQuery({
    queryKey: [path, page],
    queryFn: () => uapi<Paged<T>>(`${path}?page=${page}&pageSize=${PAGE}`),
    placeholderData: (prev) => prev,
  })
}

export function RecordsTab() {
  const [page, setPage] = useState(1)
  const q = usePaged<UsageRecord>('/usage/records', page)
  const rows = q.data?.records || []
  const mobile = useIsMobile()
  return (
    <div className="card card-flush">
      <div className="card-header">
        <span className="card-title">
          <ReceiptText />
          消费明细
        </span>
        <span className="card-actions xs muted">每次请求一条，新的在前</span>
      </div>
      {q.isLoading ? (
        <TableSkeleton rows={8} cols={6} />
      ) : q.isError ? (
        <Empty title="加载失败">{(q.error as Error).message}</Empty>
      ) : !rows.length ? (
        <Empty icon={<ReceiptText />} title="还没有消费记录">
          调用接口后，每次请求的消耗都会记录在这里
        </Empty>
      ) : mobile ? (
        <div style={{ opacity: q.isPlaceholderData ? 0.6 : 1, transition: 'opacity .15s' }}>
          <div className="mlist">
            {rows.map((r, i) => (
              <div key={`${r.createdAt}-${i}`} className="mcard" style={{ ['--i' as string]: i, gap: 6, padding: '11px 14px' }}>
                <div className="row row-between" style={{ gap: 10 }}>
                  <span className="mono xs text-2 ellipsis">{r.model || '—'}</span>
                  <span className="strong num nowrap">{formatCredits(r.credits)} 积分</span>
                </div>
                <div className="mcard-sub" style={{ marginTop: 0 }}>
                  <span className="num">{formatDateTime(r.createdAt, true).slice(5)}</span>
                  <span className="num">输入 {formatNumber(r.inputTokens || 0)}</span>
                  {!!r.cacheReadInputTokens && <span className="num">缓存 {formatNumber(r.cacheReadInputTokens)}</span>}
                  <span className="num">输出 {formatNumber(r.outputTokens || 0)}</span>
                </div>
              </div>
            ))}
          </div>
          <Pager page={page} pageSize={PAGE} total={q.data?.total || 0} onChange={setPage} />
        </div>
      ) : (
        <div style={{ opacity: q.isPlaceholderData ? 0.6 : 1, transition: 'opacity .15s' }}>
          <div className="table-wrap">
            <table className="table" style={{ minWidth: 680 }}>
              <thead>
                <tr>
                  <th>时间</th>
                  <th>模型</th>
                  <th className="right">输入</th>
                  <th className="right">缓存读取</th>
                  <th className="right">输出</th>
                  <th className="right">积分</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={`${r.createdAt}-${i}`} style={{ ['--i' as string]: i }}>
                    <td className="num muted nowrap">{formatDateTime(r.createdAt, true)}</td>
                    <td className="mono xs text-2">{r.model || '—'}</td>
                    <td className="right num">{formatNumber(r.inputTokens || 0)}</td>
                    <td className="right num">{r.cacheReadInputTokens ? formatNumber(r.cacheReadInputTokens) : <span className="faint">0</span>}</td>
                    <td className="right num">{formatNumber(r.outputTokens || 0)}</td>
                    <td className="right num strong">{formatCredits(r.credits)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pager page={page} pageSize={PAGE} total={q.data?.total || 0} onChange={setPage} />
        </div>
      )}
    </div>
  )
}

export function RechargesTab() {
  const [page, setPage] = useState(1)
  const q = usePaged<RechargeRecord>('/recharges', page)
  const rows = q.data?.records || []
  const mobile = useIsMobile()
  return (
    <div className="card card-flush">
      <div className="card-header">
        <span className="card-title">
          <Wallet />
          充值记录
        </span>
      </div>
      {q.isLoading ? (
        <TableSkeleton rows={5} cols={4} />
      ) : q.isError ? (
        <Empty title="加载失败">{(q.error as Error).message}</Empty>
      ) : !rows.length ? (
        <Empty icon={<Wallet />} title="还没有充值记录" />
      ) : mobile ? (
        <div>
          <div className="mlist">
            {rows.map((r, i) => (
              <div key={`${r.createdAt}-${i}`} className="mcard" style={{ ['--i' as string]: i, gap: 4, padding: '12px 14px' }}>
                <div className="row row-between">
                  <span className="strong num t-ok" style={{ fontSize: 15 }}>
                    +{formatNumber(r.amount || 0)}
                  </span>
                  <span className="xs muted num">{formatDateTime(r.createdAt)}</span>
                </div>
                <div className="mcard-sub" style={{ marginTop: 0 }}>
                  <span className="num">充值后总额度 {formatNumber(r.balanceAfter || 0)}</span>
                  {r.note && <span>{r.note}</span>}
                </div>
              </div>
            ))}
          </div>
          <Pager page={page} pageSize={PAGE} total={q.data?.total || 0} onChange={setPage} />
        </div>
      ) : (
        <div style={{ opacity: q.isPlaceholderData ? 0.6 : 1, transition: 'opacity .15s' }}>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>时间</th>
                  <th className="right">充值积分</th>
                  <th className="right">充值后总额度</th>
                  <th>备注</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={`${r.createdAt}-${i}`}>
                    <td className="num muted nowrap">{formatDateTime(r.createdAt)}</td>
                    <td className="right num strong t-ok">+{formatNumber(r.amount || 0)}</td>
                    <td className="right num">{formatNumber(r.balanceAfter || 0)}</td>
                    <td className="muted">{r.note || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pager page={page} pageSize={PAGE} total={q.data?.total || 0} onChange={setPage} />
        </div>
      )}
    </div>
  )
}
