import { useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  ArrowDownWideNarrow,
  CalendarClock,
  CornerDownRight,
  Eraser,
  GitBranch,
  Globe,
  KeyRound,
  Link2,
  MoreHorizontal,
  PencilLine,
  Plus,
  ReceiptText,
  Search,
  Trash2,
  Wallet,
} from 'lucide-react'
import { toast } from 'sonner'
import { api, type ApiKey } from '../api'
import { qk, useAccounts, useApiKeys } from '../queries'
import { TopbarActions } from '../Shell'
import { Button } from '@/ui/Button'
import { Input } from '@/ui/Field'
import { Badge, Bar, Dot, Segmented, Select, Switch, Tip, usageTone } from '@/ui/controls'
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from '@/ui/Dialog'
import { CopyButton, Empty, Pager, TableSkeleton } from '@/ui/misc'
import { confirm } from '@/lib/confirm'
import { copyText } from '@/lib/clipboard'
import { formatCompact, formatDate, formatDateTime, formatNumber, formatRelTime } from '@/lib/format'
import { keyDot, keyExpired, keyName, limitLabel, normalizeKey, type NKey } from './keys/util'
import { useIsMobile } from '@/lib/media'
import { useIntent } from '@/lib/useIntent'
import { KeyFormDialog } from './keys/KeyForm'
import { TopupDialog } from './keys/Topup'
import { KeyDetailDialog } from './keys/KeyDetail'
import { displayName } from './accounts/util'

type Filter = 'all' | 'active' | 'disabled' | 'expired' | 'sub'
type Sort = 'balance' | 'rpm' | 'createdDesc'
const PAGE = 50

export function KeysPage() {
  const qc = useQueryClient()
  const { data, isLoading, isError, error } = useApiKeys(true)
  const accounts = useAccounts()
  const keys = useMemo(() => (data || []).map(normalizeKey), [data])
  const [kw, setKw] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [sort, setSort] = useState<Sort>('balance')
  const [page, setPage] = useState(1)
  const [form, setForm] = useState<{ open: boolean; edit?: NKey; parentId?: string }>({ open: false })
  const [topup, setTopup] = useState<NKey | null>(null)
  const [detail, setDetail] = useState<NKey | null>(null)

  const refresh = () => qc.invalidateQueries({ queryKey: qk.keys })

  const childCount = useMemo(() => {
    const m = new Map<string, { n: number; granted: number }>()
    for (const k of keys)
      if (k.parentKeyId) {
        const c = m.get(k.parentKeyId) || { n: 0, granted: 0 }
        c.n++
        c.granted += k.granted
        m.set(k.parentKeyId, c)
      }
    return m
  }, [keys])
  const byId = useMemo(() => new Map(keys.map((k) => [k.id, k])), [keys])

  const counts = useMemo(
    () => ({
      all: keys.length,
      active: keys.filter((k) => k.enabled && !keyExpired(k)).length,
      disabled: keys.filter((k) => !k.enabled).length,
      expired: keys.filter(keyExpired).length,
      sub: keys.filter((k) => k.parentKeyId).length,
    }),
    [keys],
  )

  const rows = useMemo(() => {
    const q = kw.trim().toLowerCase()
    const pass = (k: NKey) => {
      if (filter === 'active' && (!k.enabled || keyExpired(k))) return false
      if (filter === 'disabled' && k.enabled) return false
      if (filter === 'expired' && !keyExpired(k)) return false
      if (filter === 'sub' && !k.parentKeyId) return false
      if (q && !`${k.name} ${k.key} ${k.keyMasked}`.toLowerCase().includes(q)) return false
      return true
    }
    // Budgeted cards by balance, high to low; unlimited cards have no balance and go last.
    const bal = (k: NKey) => (k.granted > 0 ? k.balance : Number.NEGATIVE_INFINITY)
    const cmp = (a: NKey, b: NKey) => {
      if (sort === 'rpm') return b.rpm - a.rpm
      if (sort === 'createdDesc') return b.createdAt - a.createdAt
      const x = bal(b)
      const y = bal(a)
      return x === y ? 0 : x > y ? 1 : -1
    }
    const matched = keys.filter(pass).sort(cmp)
    if (filter === 'sub' || q) return matched.map((k) => ({ k, child: !!k.parentKeyId }))
    // Nest sub-cards directly under their parent card.
    const matchedIds = new Set(matched.map((k) => k.id))
    const out: { k: NKey; child: boolean }[] = []
    for (const k of matched) {
      if (k.parentKeyId && matchedIds.has(k.parentKeyId)) continue
      out.push({ k, child: !!k.parentKeyId })
      if (!k.parentKeyId) for (const c of matched) if (c.parentKeyId === k.id) out.push({ k: c, child: true })
    }
    return out
  }, [keys, filter, kw, sort])

  const pageRows = rows.slice((page - 1) * PAGE, page * PAGE)

  const setEnabled = async (k: NKey, enabled: boolean) => {
    qc.setQueryData<ApiKey[]>(qk.keys, (old) => old?.map((x) => (x.id === k.id ? { ...x, enabled } : x)))
    try {
      await api(`/api-keys/${encodeURIComponent(k.id)}`, { method: 'PUT', body: { enabled } })
      toast.success(enabled ? '卡密已启用' : '卡密已禁用')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      refresh()
    }
  }

  const reset = async (k: NKey) => {
    const ok = await confirm({
      title: `清零「${keyName(k)}」的用量？`,
      description: '已用积分、Token 和请求计数会归零，余额恢复为额度。充值记录保留。',
      confirmText: '清零',
      danger: true,
    })
    if (!ok) return
    try {
      await api(`/api-keys/${encodeURIComponent(k.id)}/reset-usage`, { method: 'POST' })
      toast.success('用量已清零')
      refresh()
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  const remove = async (k: NKey) => {
    const kids = childCount.get(k.id)?.n || 0
    const ok = await confirm({
      title: `删除卡密「${keyName(k)}」？`,
      description: k.parentKeyId
        ? '子卡已消耗的积分会结算到父卡，未用完的部分退回父卡额度池。'
        : kids
          ? `这张卡下有 ${kids} 张子卡，删除后客户将无法继续使用这张卡。`
          : '删除后使用这张卡的客户会立即无法调用。',
      confirmText: '删除',
      danger: true,
    })
    if (!ok) return
    try {
      await api(`/api-keys/${encodeURIComponent(k.id)}`, { method: 'DELETE' })
      toast.success('卡密已删除')
      refresh()
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  const accountLabel = (id: string) => {
    const a = accounts.data?.find((x) => x.id === id)
    return a ? displayName(a) : id.slice(0, 8)
  }

  const totalUsed = keys.reduce((s, k) => s + k.used, 0)
  const totalBalance = keys.reduce((s, k) => s + (k.granted > 0 ? k.balance : 0), 0)
  const mobile = useIsMobile()

  useIntent('keys', (i) => {
    if (i.action === 'create') setForm({ open: true })
    else if (i.action === 'detail') {
      const k = keys.find((x) => x.id === i.id)
      if (k) setDetail(k)
    }
  })

  const keyMenu = (k: NKey) => (
    <Menu>
      <MenuTrigger asChild>
        <Button size="sm" variant="ghost" iconOnly icon={<MoreHorizontal />}>
          更多操作
        </Button>
      </MenuTrigger>
      <MenuContent>
        <MenuItem icon={<ReceiptText />} onSelect={() => setDetail(k)}>
          用量与明细
        </MenuItem>
        <MenuItem icon={<PencilLine />} onSelect={() => setForm({ open: true, edit: k })}>
          编辑
        </MenuItem>
        {!k.parentKeyId && (
          <MenuItem icon={<GitBranch />} onSelect={() => setForm({ open: true, parentId: k.id })}>
            开一张子卡
          </MenuItem>
        )}
        <MenuItem
          icon={<KeyRound />}
          onSelect={() =>
            copyText(k.key || k.keyMasked).then(
              () => toast.success('Key 已复制'),
              () => toast.error('复制失败'),
            )
          }
        >
          复制 Key
        </MenuItem>
        <MenuSeparator />
        <MenuItem icon={<Eraser />} onSelect={() => reset(k)}>
          清零用量
        </MenuItem>
        <MenuItem icon={<Trash2 />} danger onSelect={() => remove(k)}>
          删除卡密
        </MenuItem>
      </MenuContent>
    </Menu>
  )

  const keyTags = (k: NKey) => {
    const kids = childCount.get(k.id)
    const parent = k.parentKeyId ? byId.get(k.parentKeyId) : undefined
    return (
      <>
        {kids && <Badge tone="accent">父卡 · {kids.n} 张子卡</Badge>}
        {k.parentKeyId && (
          <Tip content={`父卡：${parent ? keyName(parent) : k.parentKeyId.slice(0, 8)}`}>
            <span>
              <Badge tone="teal">子卡</Badge>
            </span>
          </Tip>
        )}
        {k.migrated && <Badge tone="outline">已迁移</Badge>}
        {!k.enabled && <Badge tone="bad">已禁用</Badge>}
        {keyExpired(k) && <Badge tone="bad">已过期</Badge>}
      </>
    )
  }

  return (
    <>
      <TopbarActions>
        <Button size="sm" variant="primary" icon={<Plus />} onClick={() => setForm({ open: true })}>
          新建卡密
        </Button>
      </TopbarActions>

      <div className="stack" style={{ gap: 16 }}>
        <div className="card mini-stats" style={{ ['--cols' as string]: 4 }}>
          <div className="mini-stat">
            <div className="mini-stat-label">卡密总数</div>
            <div className="mini-stat-value">{formatNumber(keys.length)}</div>
          </div>
          <div className="mini-stat">
            <div className="mini-stat-label">
              <Dot tone="ok" />
              生效中
            </div>
            <div className="mini-stat-value">{formatNumber(counts.active)}</div>
          </div>
          <div className="mini-stat">
            <div className="mini-stat-label">累计消耗积分</div>
            <div className="mini-stat-value">{formatNumber(Math.round(totalUsed))}</div>
          </div>
          <div className="mini-stat">
            <div className="mini-stat-label">剩余余额（限额卡）</div>
            <div className="mini-stat-value t-ok">{formatNumber(Math.round(totalBalance))}</div>
          </div>
        </div>

        <div className="card card-flush">
          <div className="toolbar">
            <div className="search">
              <Input
                icon={<Search />}
                placeholder="搜索名称或 Key"
                value={kw}
                onChange={(e) => {
                  setKw(e.target.value)
                  setPage(1)
                }}
              />
            </div>
            <Segmented
              id="key-filter"
              value={filter}
              onChange={(v) => {
                setFilter(v)
                setPage(1)
              }}
              items={[
                { value: 'all', label: '全部', count: counts.all },
                { value: 'active', label: '生效', count: counts.active },
                { value: 'disabled', label: '禁用', count: counts.disabled },
                { value: 'expired', label: '过期', count: counts.expired },
                { value: 'sub', label: '子卡', count: counts.sub },
              ]}
            />
            <Select
              value={sort}
              onChange={setSort}
              prefix={<ArrowDownWideNarrow />}
              style={{ width: 150 }}
              ariaLabel="排序"
              options={[
                { value: 'balance', label: '按余额' },
                { value: 'rpm', label: '按实时 RPM' },
                { value: 'createdDesc', label: '最新创建' },
              ]}
            />
          </div>

          {isLoading ? (
            <TableSkeleton rows={6} cols={6} />
          ) : isError ? (
            <Empty title="卡密加载失败">{(error as Error)?.message}</Empty>
          ) : !rows.length ? (
            <Empty icon={<KeyRound />} title={keys.length ? '没有匹配的卡密' : '还没有卡密'}>
              {keys.length ? (
                '换个关键词或筛选条件'
              ) : (
                <Button size="sm" variant="primary" icon={<Plus />} onClick={() => setForm({ open: true })} style={{ marginTop: 6 }}>
                  新建第一张卡密
                </Button>
              )}
            </Empty>
          ) : mobile ? (
            <>
              <div className="mlist">
                {pageRows.map(({ k, child }, idx) => {
                  const ratio = k.granted > 0 ? Math.min(1, k.used / k.granted) : 0
                  return (
                    <div key={k.id} className="mcard" data-dim={!k.enabled || keyExpired(k)} style={{ ['--i' as string]: idx, paddingLeft: child ? 30 : undefined }}>
                      {child && <CornerDownRight size={13} className="faint" style={{ position: 'absolute', left: 12, top: 18 }} />}
                      <div className="mcard-head">
                        <Dot tone={keyDot(k)} />
                        <div className="grow">
                          <div className="mcard-title">
                            <button type="button" className="name" onClick={() => setDetail(k)}>
                              {keyName(k)}
                            </button>
                            {keyTags(k)}
                          </div>
                          <div className="mcard-sub" style={{ alignItems: 'center' }}>
                            <span className="key-code">{k.keyMasked || k.key}</span>
                            <CopyButton value={k.key || k.keyMasked} label="复制 Key" done="Key 已复制" />
                            <CopyButton value={() => location.origin} label="复制接口地址" done="接口地址已复制" glyph={<Globe size={14} />} />
                          </div>
                        </div>
                        <Switch checked={k.enabled} onChange={(v) => setEnabled(k, v)} label={k.enabled ? '禁用卡密' : '启用卡密'} />
                      </div>
                      <div>
                        <div className="quota-top">
                          <span>{k.granted > 0 ? '余额' : '不限额'}</span>
                          <span>
                            {k.granted > 0 ? (
                              <>
                                <b className={k.balance < 0 ? 't-bad' : undefined}>{formatNumber(Math.round(k.balance * 10) / 10)}</b> / {formatNumber(k.granted)}
                              </>
                            ) : (
                              <>已用 {formatNumber(Math.round(k.used * 10) / 10)}</>
                            )}
                          </span>
                        </div>
                        {k.granted > 0 && <Bar value={ratio} tone={usageTone(ratio)} />}
                      </div>
                      <div className="mcard-stats" style={{ ['--c' as string]: 3 }}>
                        <div className="mcard-stat">
                          <div className="l">请求</div>
                          <div className="v">{formatCompact(k.requestsCount)}</div>
                        </div>
                        <div className="mcard-stat">
                          <div className="l">RPM</div>
                          <div className="v">{k.rpm}</div>
                        </div>
                        <div className="mcard-stat">
                          <div className="l">到期</div>
                          <div className={`v ${keyExpired(k) ? 't-bad' : ''}`} style={{ fontSize: 12.5 }}>
                            {k.expiresAt ? formatDate(k.expiresAt).slice(2) : '永久'}
                          </div>
                        </div>
                      </div>
                      <div className="mcard-foot">
                        <Button size="sm" icon={<Wallet />} onClick={() => setTopup(k)}>
                          充值
                        </Button>
                        <Button size="sm" variant="ghost" icon={<ReceiptText />} onClick={() => setDetail(k)}>
                          明细
                        </Button>
                        <span className="grow" />
                        {keyMenu(k)}
                      </div>
                    </div>
                  )
                })}
              </div>
              <Pager page={page} pageSize={PAGE} total={rows.length} onChange={setPage} />
            </>
          ) : (
            <>
              <div className="table-wrap">
                <table className="table" style={{ minWidth: 1120 }}>
                  <thead>
                    <tr>
                      <th>卡密</th>
                      <th>余额</th>
                      <th className="right">请求</th>
                      <th className="right">Tokens</th>
                      <th className="right">RPM</th>
                      <th>限制</th>
                      <th>到期</th>
                      <th style={{ width: 64 }}>启用</th>
                      <th className="right" style={{ width: 118 }}>
                        操作
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {pageRows.map(({ k, child }, idx) => {
                      const kids = childCount.get(k.id)
                      const expired = keyExpired(k)
                      const ratio = k.granted > 0 ? Math.min(1, k.used / k.granted) : 0
                      return (
                        <tr key={k.id} data-dim={!k.enabled || expired} style={{ ['--i' as string]: idx }}>
                          <td style={{ minWidth: 320 }}>
                            <div className="ident" style={child ? { paddingLeft: 18 } : undefined}>
                              {child ? <CornerDownRight size={14} className="faint none" style={{ marginTop: 3 }} /> : <Dot tone={keyDot(k)} />}
                              <div className="ident-main">
                                <div className="ident-title">
                                  {child && <Dot tone={keyDot(k)} />}
                                  <button type="button" className="name" onClick={() => setDetail(k)} title={k.name}>
                                    {keyName(k)}
                                  </button>
                                  {keyTags(k)}
                                </div>
                                <div className="ident-sub" style={{ alignItems: 'center' }}>
                                  <span className="key-code" title={k.keyMasked}>
                                    {k.keyMasked || k.key}
                                  </span>
                                  <span style={{ gap: 0 }}>
                                    <CopyButton value={k.key || k.keyMasked} label="复制 Key" done="Key 已复制" />
                                    <CopyButton value={() => location.origin} label="复制接口地址" done="接口地址已复制" glyph={<Globe size={14} />} />
                                  </span>
                                  {k.boundAccountIds.length > 0 && (
                                    <Tip content={k.boundAccountIds.map(accountLabel).join('、')}>
                                      <span>
                                        <Link2 />
                                        绑定 {k.boundAccountIds.length} 个账号
                                      </span>
                                    </Tip>
                                  )}
                                  {k.lastUsedAt > 0 && <span>{formatRelTime(k.lastUsedAt)}使用</span>}
                                </div>
                              </div>
                            </div>
                          </td>
                          <td>
                            <div className="quota-cell">
                              {k.granted > 0 ? (
                                <>
                                  <div className="quota-top">
                                    <b className={k.balance < 0 ? 't-bad' : undefined}>{formatNumber(Math.round(k.balance * 10) / 10)}</b>
                                    <span>/ {formatNumber(k.granted)}</span>
                                  </div>
                                  <Bar value={ratio} tone={usageTone(ratio)} />
                                  {kids && (
                                    <div className="xs faint" style={{ marginTop: 5 }}>
                                      已分给子卡 {formatNumber(kids.granted)}
                                    </div>
                                  )}
                                </>
                              ) : (
                                <>
                                  <div className="quota-top">
                                    <b>不限额</b>
                                  </div>
                                  <span className="xs muted">已用 {formatNumber(Math.round(k.used * 10) / 10)}</span>
                                </>
                              )}
                            </div>
                          </td>
                          <td className="right num">
                            <Tip content={formatNumber(k.requestsCount)}>
                              <span>{formatCompact(k.requestsCount)}</span>
                            </Tip>
                          </td>
                          <td className="right num">
                            <Tip content={formatNumber(k.tokensUsed)}>
                              <span>{formatCompact(k.tokensUsed)}</span>
                            </Tip>
                          </td>
                          <td className="right num">{k.rpm}</td>
                          <td className="xs">
                            <div>并发 {limitLabel(k.maxConcurrency)}</div>
                            <div className="muted">RPM {limitLabel(k.maxRPM)}</div>
                          </td>
                          <td className="xs nowrap">
                            {k.expiresAt ? (
                              <Tip content={formatDateTime(k.expiresAt)}>
                                <span className={expired ? 't-bad' : undefined}>
                                  <CalendarClock size={12} style={{ display: 'inline', verticalAlign: -2, marginRight: 4 }} />
                                  {formatDate(k.expiresAt)}
                                </span>
                              </Tip>
                            ) : (
                              <span className="muted">永久</span>
                            )}
                          </td>
                          <td>
                            <Switch checked={k.enabled} onChange={(v) => setEnabled(k, v)} label={k.enabled ? '禁用卡密' : '启用卡密'} />
                          </td>
                          <td>
                            <div className="row-actions">
                              <Button size="sm" icon={<Wallet />} onClick={() => setTopup(k)}>
                                充值
                              </Button>
                              {keyMenu(k)}
                            </div>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
              <Pager page={page} pageSize={PAGE} total={rows.length} onChange={setPage} />
            </>
          )}
        </div>
      </div>

      <KeyFormDialog
        open={form.open}
        edit={form.edit}
        parentId={form.parentId}
        keys={keys}
        accounts={accounts.data || []}
        onOpenChange={(o) => setForm((f) => ({ ...f, open: o }))}
        onSaved={refresh}
      />
      <TopupDialog k={topup} onClose={() => setTopup(null)} onDone={refresh} />
      <KeyDetailDialog
        k={detail}
        keys={keys}
        onClose={() => setDetail(null)}
        onCreateChild={(pid) => {
          setDetail(null)
          setForm({ open: true, parentId: pid })
        }}
      />
    </>
  )
}
