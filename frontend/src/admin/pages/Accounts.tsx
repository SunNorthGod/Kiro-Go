import { useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, motion } from 'motion/react'
import {
  ArrowDownWideNarrow,
  Clock3,
  Copy,
  Download,
  FlaskConical,
  Hourglass,
  MoreHorizontal,
  Plus,
  Search,
  ShieldCheck,
  Trash2,
  UserRound,
  Users,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import { api, type Account, type ExportData } from '../api'
import { qk, useAccounts } from '../queries'
import { TopbarActions } from '../Shell'
import { Button } from '@/ui/Button'
import { Input } from '@/ui/Field'
import { Badge, Bar, Checkbox, Dot, Segmented, Select, Switch, Tip, usageTone } from '@/ui/controls'
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from '@/ui/Dialog'
import { Empty, TableSkeleton } from '@/ui/misc'
import { confirm } from '@/lib/confirm'
import { copyText } from '@/lib/clipboard'
import { formatCompact, formatDateTime, formatDuration, formatNumber, formatRelTime, formatTimeLeft } from '@/lib/format'
import { authLabel, displayName, dotTone, importPayloadFromExport, isBanned, remainingQuota, subscription, tokenExpired, trialSuffix } from './accounts/util'
import { useIsMobile } from '@/lib/media'
import { useIntent } from '@/lib/useIntent'
import { AddAccountDialog } from './accounts/AddAccount'
import { AccountDetailDialog } from './accounts/Detail'
import { TestDialog } from './accounts/TestDialog'
import { ExportDialog } from './accounts/ExportDialog'

type Filter = 'all' | 'enabled' | 'disabled' | 'banned'
type Sort = 'priority' | 'rpm' | 'usage'

export function AccountsPage() {
  const qc = useQueryClient()
  const { data, isLoading, isError, error } = useAccounts(true)
  const accounts = useMemo(() => data || [], [data])
  const [kw, setKw] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [sort, setSort] = useState<Sort>('priority')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [adding, setAdding] = useState(false)
  const [exporting, setExporting] = useState(false)
  const [detailId, setDetailId] = useState<string | null>(null)
  const [testId, setTestId] = useState<string | null>(null)
  const [busyBatch, setBusyBatch] = useState(false)

  const refresh = () => qc.invalidateQueries({ queryKey: qk.accounts })

  const counts = useMemo(
    () => ({
      all: accounts.length,
      enabled: accounts.filter((a) => a.enabled).length,
      disabled: accounts.filter((a) => !a.enabled && !isBanned(a)).length,
      banned: accounts.filter(isBanned).length,
    }),
    [accounts],
  )

  const rows = useMemo(() => {
    const q = kw.trim().toLowerCase()
    const list = accounts.filter((a) => {
      if (filter === 'enabled' && !a.enabled) return false
      if (filter === 'disabled' && (a.enabled || isBanned(a))) return false
      if (filter === 'banned' && !isBanned(a)) return false
      if (q && !`${a.email} ${a.nickname} ${a.userId} ${a.id}`.toLowerCase().includes(q)) return false
      return true
    })
    list.sort((a, b) => {
      if (sort === 'rpm') return (b.rpm || 0) - (a.rpm || 0)
      if (sort === 'usage') return (b.usagePercent || 0) - (a.usagePercent || 0)
      return (b.weight || 0) - (a.weight || 0)
    })
    return list
  }, [accounts, filter, kw, sort])

  // Drop selections for accounts that no longer exist.
  const selectedIds = useMemo(() => [...selected].filter((id) => accounts.some((a) => a.id === id)), [selected, accounts])
  const visibleSelected = rows.filter((a) => selected.has(a.id)).length
  const allState: boolean | 'indeterminate' = rows.length > 0 && visibleSelected === rows.length ? true : visibleSelected > 0 ? 'indeterminate' : false

  const toggleOne = (id: string, on: boolean) =>
    setSelected((s) => {
      const n = new Set(s)
      if (on) n.add(id)
      else n.delete(id)
      return n
    })
  const toggleAll = (on: boolean) =>
    setSelected((s) => {
      const n = new Set(s)
      rows.forEach((a) => (on ? n.add(a.id) : n.delete(a.id)))
      return n
    })

  const setEnabled = async (a: Account, enabled: boolean) => {
    qc.setQueryData<Account[]>(qk.accounts, (old) => old?.map((x) => (x.id === a.id ? { ...x, enabled } : x)))
    try {
      await api(`/accounts/${encodeURIComponent(a.id)}`, { method: 'PUT', body: { enabled } })
      toast.success(enabled ? '账号已启用' : '账号已禁用')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      refresh()
    }
  }

  const remove = async (a: Account) => {
    const ok = await confirm({ title: '删除这个账号？', description: `${displayName(a)} 会从账号池移除，已记录的统计一并删除。`, confirmText: '删除', danger: true })
    if (!ok) return
    try {
      await api(`/accounts/${encodeURIComponent(a.id)}`, { method: 'DELETE' })
      toast.success('账号已删除')
      toggleOne(a.id, false)
      refresh()
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  const copyJson = async (a: Account) => {
    try {
      const d = await api<ExportData>('/export', { method: 'POST', body: { ids: [a.id] } })
      const acc = d.accounts?.[0]
      if (!acc) throw new Error('没有找到这个账号的凭证')
      await copyText(JSON.stringify(importPayloadFromExport(acc), null, 2))
      toast.success('凭证 JSON 已复制', { description: '可直接粘贴到「添加账号 · 凭证 JSON」导入' })
    } catch (e) {
      toast.error('复制失败：' + (e as Error).message)
    }
  }

  const batch = async (action: 'enable' | 'disable' | 'delete') => {
    const ids = selectedIds
    if (!ids.length) return
    if (action === 'delete') {
      const ok = await confirm({ title: `删除 ${ids.length} 个账号？`, description: '删除后无法恢复。', confirmText: '删除', danger: true })
      if (!ok) return
      setBusyBatch(true)
      let okN = 0
      let fail = 0
      for (const id of ids) {
        try {
          await api(`/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' })
          okN++
        } catch {
          fail++
        }
      }
      setBusyBatch(false)
      setSelected(new Set())
      refresh()
      if (fail) toast.warning(`已删除 ${okN} 个，${fail} 个失败`)
      else toast.success(`已删除 ${okN} 个账号`)
      return
    }
    const ok = await confirm({
      title: action === 'enable' ? `启用 ${ids.length} 个账号？` : `禁用 ${ids.length} 个账号？`,
      description: action === 'enable' ? '被封禁的账号也会解除封禁标记并重新参与调度。' : '禁用的账号不再接收新请求。',
      confirmText: action === 'enable' ? '启用' : '禁用',
      danger: action === 'disable',
    })
    if (!ok) return
    setBusyBatch(true)
    try {
      const d = await api<{ count: number }>('/accounts/batch', { method: 'POST', body: { ids, action } })
      toast.success(`${action === 'enable' ? '已启用' : '已禁用'} ${d.count || ids.length} 个账号`)
      setSelected(new Set())
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusyBatch(false)
      refresh()
    }
  }

  const totalRpm = accounts.reduce((s, a) => s + (a.rpm || 0), 0)
  const remain = accounts.reduce((s, a) => s + remainingQuota(a), 0)
  const mobile = useIsMobile()

  useIntent('accounts', (i) => {
    if (i.action === 'add') setAdding(true)
    else if (i.action === 'detail') setDetailId(i.id)
  })

  return (
    <>
      <TopbarActions>
        <Button size="sm" icon={<Download />} className="btn-collapse" onClick={() => (accounts.length ? setExporting(true) : toast.warning('还没有账号'))}>
          导出
        </Button>
        <Button size="sm" variant="primary" icon={<Plus />} onClick={() => setAdding(true)}>
          添加账号
        </Button>
      </TopbarActions>

      <div className="stack" style={{ gap: 16 }}>
        <div className="card mini-stats" style={{ ['--cols' as string]: 5 }}>
          <MiniStat label="账号" value={formatNumber(accounts.length)} />
          <MiniStat label="已启用" value={formatNumber(counts.enabled)} dot="ok" />
          <MiniStat label="已封禁" value={formatNumber(counts.banned)} dot={counts.banned ? 'bad' : 'off'} tone={counts.banned ? 't-bad' : undefined} />
          <MiniStat label="剩余额度" value={formatNumber(Math.round(remain))} tone="t-ok" />
          <MiniStat label="实时 RPM" value={formatNumber(totalRpm)} />
        </div>

        <div className="card card-flush">
          <div className="toolbar">
            <div className="search">
              <Input icon={<Search />} placeholder="搜索邮箱、昵称或用户 ID" value={kw} onChange={(e) => setKw(e.target.value)} />
            </div>
            <Segmented
              id="acc-filter"
              value={filter}
              onChange={setFilter}
              items={[
                { value: 'all', label: '全部', count: counts.all },
                { value: 'enabled', label: '已启用', count: counts.enabled },
                { value: 'disabled', label: '已禁用', count: counts.disabled },
                { value: 'banned', label: '已封禁', count: counts.banned },
              ]}
            />
            <Select
              value={sort}
              onChange={setSort}
              prefix={<ArrowDownWideNarrow />}
              style={{ width: 150 }}
              ariaLabel="排序"
              options={[
                { value: 'priority', label: '按优先级' },
                { value: 'rpm', label: '按实时 RPM' },
                { value: 'usage', label: '按额度用量' },
              ]}
            />
          </div>

          {isLoading ? (
            <TableSkeleton rows={4} cols={6} />
          ) : isError ? (
            <Empty title="账号列表加载失败">{(error as Error)?.message}</Empty>
          ) : !rows.length ? (
            <Empty icon={<Users />} title={accounts.length ? '没有匹配的账号' : '还没有账号'}>
              {accounts.length ? (
                '换个关键词或筛选条件'
              ) : (
                <Button variant="primary" size="sm" icon={<Plus />} onClick={() => setAdding(true)} style={{ marginTop: 6 }}>
                  添加第一个账号
                </Button>
              )}
            </Empty>
          ) : mobile ? (
            <>
              <div className="row" style={{ padding: '10px 14px', borderBottom: '1px solid var(--divider)', gap: 10 }}>
                <Checkbox checked={allState} onChange={toggleAll} label="全选" />
                <span className="xs muted">全选当前 {rows.length} 个</span>
              </div>
              <div className="mlist">
                {rows.map((a, i) => (
                  <AccountCard
                    key={a.id}
                    a={a}
                    i={i}
                    selected={selected.has(a.id)}
                    onSelect={(on) => toggleOne(a.id, on)}
                    onToggle={(v) => setEnabled(a, v)}
                    onDetail={() => setDetailId(a.id)}
                    onTest={() => setTestId(a.id)}
                    onCopy={() => copyJson(a)}
                    onDelete={() => remove(a)}
                  />
                ))}
              </div>
            </>
          ) : (
            <div className="table-wrap">
              <table className="table" style={{ minWidth: 1060 }}>
                <thead>
                  <tr>
                    <th className="cell-check">
                      <Checkbox checked={allState} onChange={toggleAll} label="全选" />
                    </th>
                    <th>账号</th>
                    <th>额度</th>
                    <th className="right">积分</th>
                    <th className="right">RPM</th>
                    <th className="right">请求</th>
                    <th className="right">Tokens</th>
                    <th style={{ width: 64 }}>启用</th>
                    <th className="right" style={{ width: 118 }}>
                      操作
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((a, i) => (
                    <AccountRow
                      key={a.id}
                      i={i}
                      a={a}
                      selected={selected.has(a.id)}
                      onSelect={(on) => toggleOne(a.id, on)}
                      onToggle={(v) => setEnabled(a, v)}
                      onDetail={() => setDetailId(a.id)}
                      onTest={() => setTestId(a.id)}
                      onCopy={() => copyJson(a)}
                      onDelete={() => remove(a)}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      <AnimatePresence>
        {selectedIds.length > 0 && (
          <motion.div
            className="batch-bar"
            initial={{ opacity: 0, y: 24, x: '-50%', scale: 0.96 }}
            animate={{ opacity: 1, y: 0, x: '-50%', scale: 1 }}
            exit={{ opacity: 0, y: 24, x: '-50%', scale: 0.96 }}
            transition={{ type: 'spring', stiffness: 420, damping: 34 }}
          >
            <b>已选 {selectedIds.length} 个账号</b>
            <Button size="sm" onClick={() => batch('enable')} disabled={busyBatch}>
              启用
            </Button>
            <Button size="sm" onClick={() => batch('disable')} disabled={busyBatch}>
              禁用
            </Button>
            <Button size="sm" variant="danger" onClick={() => batch('delete')} loading={busyBatch}>
              删除
            </Button>
            <Button size="sm" variant="ghost" iconOnly icon={<X />} onClick={() => setSelected(new Set())} style={{ border: 0 }}>
              取消选择
            </Button>
          </motion.div>
        )}
      </AnimatePresence>

      <AddAccountDialog open={adding} onOpenChange={setAdding} onDone={refresh} />
      <ExportDialog open={exporting} onOpenChange={setExporting} accounts={accounts} />
      <AccountDetailDialog id={detailId} onClose={() => setDetailId(null)} onSaved={refresh} />
      <TestDialog account={accounts.find((a) => a.id === testId) || null} onClose={() => setTestId(null)} />
    </>
  )
}

function MiniStat({ label, value, dot, tone }: { label: string; value: string; dot?: 'ok' | 'bad' | 'off'; tone?: string }) {
  return (
    <div className="mini-stat">
      <div className="mini-stat-label">
        {dot && <Dot tone={dot} />}
        {label}
      </div>
      <div className={`mini-stat-value ${tone || ''}`}>{value}</div>
    </div>
  )
}

interface RowProps {
  a: Account
  i: number
  selected: boolean
  onSelect: (v: boolean) => void
  onToggle: (v: boolean) => void
  onDetail: () => void
  onTest: () => void
  onCopy: () => void
  onDelete: () => void
}

function StateBadges({ a }: { a: Account }) {
  const banned = isBanned(a)
  const sub = subscription(a.subscriptionType)
  const overage = (a.overageStatus || '').toUpperCase()
  return (
    <>
      <Badge tone={sub.tone}>{sub.label}</Badge>
      {a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0 && <Badge tone="teal">试用中</Badge>}
      {overage === 'ENABLED' && <Badge tone="warn">超额开</Badge>}
      {a.banStatus === 'BANNED' && <Badge tone="bad">已封禁</Badge>}
      {a.banStatus === 'SUSPENDED' && <Badge tone="bad">已暂停</Badge>}
      {!a.hasToken ? <Badge tone="bad">无 Token</Badge> : tokenExpired(a) ? <Badge tone="warn">Token 过期</Badge> : null}
      {!a.enabled && !banned && <Badge tone="bad">已禁用</Badge>}
    </>
  )
}

function RowMenu({ onDetail, onCopy, onDelete }: { onDetail: () => void; onCopy: () => void; onDelete: () => void }) {
  return (
    <Menu>
      <MenuTrigger asChild>
        <Button size="sm" variant="ghost" iconOnly icon={<MoreHorizontal />}>
          更多操作
        </Button>
      </MenuTrigger>
      <MenuContent>
        <MenuItem icon={<UserRound />} onSelect={onDetail}>
          详情与设置
        </MenuItem>
        <MenuItem icon={<Copy />} onSelect={onCopy}>
          复制凭证 JSON
        </MenuItem>
        <MenuSeparator />
        <MenuItem icon={<Trash2 />} danger onSelect={onDelete}>
          删除账号
        </MenuItem>
      </MenuContent>
    </Menu>
  )
}

function EnableSwitch({ a, onToggle }: { a: Account; onToggle: (v: boolean) => void }) {
  if (isBanned(a)) {
    return (
      <Tip content="封禁账号需要勾选后用批量「启用」解除">
        <span>
          <Switch checked={false} onChange={() => {}} disabled label="启用" />
        </span>
      </Tip>
    )
  }
  return <Switch checked={a.enabled} onChange={onToggle} label={a.enabled ? '禁用账号' : '启用账号'} />
}

/** Phone layout: one card per account. */
function AccountCard({ a, i, selected, onSelect, onToggle, onDetail, onTest, onCopy, onDelete }: RowProps) {
  const usage = a.usageLimit > 0 ? a.usagePercent || 0 : null
  return (
    <div className="mcard" data-selected={selected} data-dim={!a.enabled || isBanned(a)} style={{ ['--i' as string]: i }}>
      <div className="mcard-head">
        <Checkbox checked={selected} onChange={onSelect} label={`选择 ${displayName(a)}`} />
        <div className="grow">
          <div className="mcard-title">
            <Dot tone={dotTone(a)} />
            <button type="button" className="name" onClick={onDetail}>
              {displayName(a)}
            </button>
          </div>
          <div className="mcard-title" style={{ marginTop: 6 }}>
            <StateBadges a={a} />
          </div>
          <div className="mcard-sub">
            <span>{authLabel(a)}</span>
            <span>{a.expiresAt ? `Token ${formatTimeLeft(a.expiresAt)}` : '长期有效'}</span>
            <span>优先级 {a.weight || 0}</span>
          </div>
        </div>
        <EnableSwitch a={a} onToggle={onToggle} />
      </div>
      {usage != null && (
        <div>
          <div className="quota-top">
            <span>主额度</span>
            <span>
              <b className={usage > 0.9 ? 't-bad' : undefined}>{(usage * 100).toFixed(0)}%</b> · {a.usageCurrent.toFixed(1)} / {a.usageLimit.toFixed(0)}
            </span>
          </div>
          <Bar value={usage} tone={usageTone(usage)} />
        </div>
      )}
      <div className="mcard-stats" style={{ ['--c' as string]: 4 }}>
        <div className="mcard-stat">
          <div className="l">积分</div>
          <div className="v">{formatCompact(a.totalCredits || 0)}</div>
        </div>
        <div className="mcard-stat">
          <div className="l">RPM</div>
          <div className="v">{a.rpm || 0}</div>
        </div>
        <div className="mcard-stat">
          <div className="l">请求</div>
          <div className="v">{formatCompact(a.requestCount || 0)}</div>
        </div>
        <div className="mcard-stat">
          <div className="l">Tokens</div>
          <div className="v">{formatCompact(a.totalTokens || 0)}</div>
        </div>
      </div>
      <div className="mcard-foot">
        <Button size="sm" icon={<FlaskConical />} onClick={onTest}>
          测试
        </Button>
        <Button size="sm" variant="ghost" icon={<UserRound />} onClick={onDetail}>
          详情
        </Button>
        <span className="grow" />
        <RowMenu onDetail={onDetail} onCopy={onCopy} onDelete={onDelete} />
      </div>
    </div>
  )
}

function AccountRow({ a, i, selected, onSelect, onToggle, onDetail, onTest, onCopy, onDelete }: RowProps) {
  const banned = isBanned(a)
  const usage = a.usageLimit > 0 ? a.usagePercent || 0 : null
  const trial = a.trialUsageLimit > 0
  const trialRatio = trial ? Math.min(1, a.trialUsageCurrent / a.trialUsageLimit) : 0

  return (
    <tr data-selected={selected} data-dim={!a.enabled || banned} style={{ ['--i' as string]: i }}>
      <td className="cell-check">
        <Checkbox checked={selected} onChange={onSelect} label={`选择 ${displayName(a)}`} />
      </td>
      <td style={{ minWidth: 300 }}>
        <div className="ident">
          <Dot tone={dotTone(a)} />
          <div className="ident-main">
            <div className="ident-title">
              <button type="button" className="name" onClick={onDetail} style={{ textAlign: 'left' }} title={a.email}>
                {displayName(a)}
              </button>
              <StateBadges a={a} />
            </div>
            <div className="ident-sub">
              <span>
                <ShieldCheck />
                {authLabel(a)}
              </span>
              <Tip content="访问令牌剩余有效期，可刷新的账号到期后会自动续期">
                <span>
                  <Hourglass />
                  {a.expiresAt ? formatTimeLeft(a.expiresAt) : '长期有效'}
                </span>
              </Tip>
              {a.createdAt > 0 && (
                <Tip content={`添加于 ${formatDateTime(a.createdAt)}，已运行 ${formatDuration(Date.now() / 1000 - a.createdAt)}`}>
                  <span>
                    <Clock3 />
                    {formatRelTime(a.createdAt)}添加
                  </span>
                </Tip>
              )}
              <span>优先级 {a.weight || 0}</span>
            </div>
          </div>
        </div>
      </td>
      <td>
        <div className="quota-cell">
          {usage != null ? (
            <>
              <div className="quota-top">
                <b className={usage > 0.9 ? 't-bad' : undefined}>{(usage * 100).toFixed(0)}%</b>
                <span>
                  {a.usageCurrent.toFixed(1)} / {a.usageLimit.toFixed(0)}
                </span>
              </div>
              <Bar value={usage} tone={usageTone(usage)} />
            </>
          ) : (
            <span className="faint xs">未获取到主额度</span>
          )}
          {trial && (
            <Tip content={`试用额度 ${a.trialUsageCurrent.toFixed(1)} / ${a.trialUsageLimit.toFixed(0)} ${trialSuffix(a.trialExpiresAt)}`}>
              <div style={{ marginTop: usage != null ? 8 : 4 }}>
                <div className="quota-top" style={{ marginBottom: 4 }}>
                  <span>试用 {trialSuffix(a.trialExpiresAt)}</span>
                  <span>
                    {a.trialUsageCurrent.toFixed(1)} / {a.trialUsageLimit.toFixed(0)}
                  </span>
                </div>
                <Bar value={trialRatio} tone="accent" thin />
              </div>
            </Tip>
          )}
        </div>
      </td>
      <td className="right num strong">
        <Tip content={(a.totalCredits || 0).toFixed(2)}>
          <span>{formatCompact(a.totalCredits || 0)}</span>
        </Tip>
      </td>
      <td className="right num">{a.rpm || 0}</td>
      <td className="right num">
        <Tip content={formatNumber(a.requestCount || 0)}>
          <span>{formatCompact(a.requestCount || 0)}</span>
        </Tip>
      </td>
      <td className="right num">
        <Tip content={formatNumber(a.totalTokens || 0)}>
          <span>{formatCompact(a.totalTokens || 0)}</span>
        </Tip>
      </td>
      <td>
        <EnableSwitch a={a} onToggle={onToggle} />
      </td>
      <td>
        <div className="row-actions">
          <Button size="sm" icon={<FlaskConical />} onClick={onTest}>
            测试
          </Button>
          <RowMenu onDetail={onDetail} onCopy={onCopy} onDelete={onDelete} />
        </div>
      </td>
    </tr>
  )
}
