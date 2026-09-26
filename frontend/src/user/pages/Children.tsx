import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, motion } from 'motion/react'
import { GitBranch, Globe, Link2, PencilLine, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { uapi, type Child, type Reseller } from '../api'
import { Button } from '@/ui/Button'
import { Field, Input } from '@/ui/Field'
import { Badge, Bar, Dot, Switch, usageTone } from '@/ui/controls'
import { Dialog } from '@/ui/Dialog'
import { CopyButton, Empty, Skeleton } from '@/ui/misc'
import { confirm } from '@/lib/confirm'
import { formatDateTime, formatNumber } from '@/lib/format'
import { useIsMobile } from '@/lib/media'

const fmt = (v: number) => formatNumber(Math.round(v * 100) / 100)

export function ChildrenTab() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['children'], queryFn: () => uapi<Reseller>('/children') })
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<Child | null>(null)
  const d = q.data
  const mobile = useIsMobile()

  const reload = () => {
    qc.invalidateQueries({ queryKey: ['children'] })
    qc.invalidateQueries({ queryKey: ['me'] })
    qc.invalidateQueries({ queryKey: ['usage'] })
  }

  const toggle = async (c: Child, enabled: boolean) => {
    qc.setQueryData<Reseller>(['children'], (old) => (old ? { ...old, children: old.children.map((x) => (x.id === c.id ? { ...x, enabled } : x)) } : old))
    try {
      await uapi(`/children/${encodeURIComponent(c.id)}`, { method: 'PUT', body: { enabled } })
      toast.success(enabled ? '子卡已启用' : '子卡已停用')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      reload()
    }
  }

  const remove = async (c: Child) => {
    const ok = await confirm({
      title: `删除子卡「${c.name || '未命名'}」？`,
      description: '已消耗的额度不退回，未用完的部分回到你的可分配额度。此操作不可撤销。',
      confirmText: '删除',
      danger: true,
    })
    if (!ok) return
    try {
      await uapi(`/children/${encodeURIComponent(c.id)}`, { method: 'DELETE' })
      toast.success('子卡已删除')
      reload()
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  if (q.isLoading) {
    return (
      <div className="card" style={{ padding: 20 }}>
        <Skeleton h={60} />
      </div>
    )
  }
  if (q.isError || !d) return <Empty title="加载失败">{(q.error as Error)?.message}</Empty>

  const budget = d.budget || 0
  const usedPart = budget > 0 ? Math.min(1, (d.ownUsed || 0) / budget) : 0
  const allocPart = budget > 0 ? Math.min(1 - usedPart, (d.allocated || 0) / budget) : 0

  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="card" style={{ padding: 20 }}>
        <div className="row row-between row-wrap" style={{ gap: 12 }}>
          <div>
            <div className="kpi-label">可分配额度</div>
            <div className="chart-value t-ok">
              {budget > 0 ? fmt(d.allocatable || 0) : '不限'}
              {budget > 0 && <small>积分</small>}
            </div>
          </div>
          <Button variant="primary" icon={<Plus />} onClick={() => setCreating(true)} disabled={budget > 0 && (d.allocatable || 0) <= 0}>
            开设子卡
          </Button>
        </div>
        {budget > 0 && (
          <div style={{ marginTop: 18 }}>
            <div style={{ display: 'flex', height: 8, borderRadius: 999, overflow: 'hidden', background: 'var(--surface-3)' }}>
              <motion.span initial={{ width: 0 }} animate={{ width: `${usedPart * 100}%` }} transition={{ duration: 0.6 }} style={{ background: 'var(--accent)' }} />
              <motion.span initial={{ width: 0 }} animate={{ width: `${allocPart * 100}%` }} transition={{ duration: 0.6, delay: 0.05 }} style={{ background: 'var(--teal)' }} />
            </div>
            <div className="row row-wrap xs muted" style={{ gap: 18, marginTop: 10 }}>
              <Legend color="var(--accent)" label="自己已用" value={fmt(d.ownUsed || 0)} />
              <Legend color="var(--teal)" label="已分给子卡" value={fmt(d.allocated || 0)} />
              <Legend color="var(--surface-3)" label="总预算" value={fmt(budget)} />
            </div>
          </div>
        )}
        <p className="xs muted" style={{ marginTop: 14 }}>
          子卡从你的额度里划拨，你自己的消耗和分给子卡的额度共用同一份预算。
        </p>
      </div>

      <div className="card card-flush">
        <div className="card-header">
          <span className="card-title">
            <GitBranch />
            子卡 · {d.children.length}
          </span>
        </div>
        {!d.children.length ? (
          <Empty icon={<GitBranch />} title="还没有子卡">
            <Button size="sm" variant="primary" icon={<Plus />} onClick={() => setCreating(true)} style={{ marginTop: 6 }}>
              开第一张
            </Button>
          </Empty>
        ) : mobile ? (
          <div className="mlist">
            {d.children.map((c, i) => {
              const g = c.creditsGranted || 0
              const bal = c.balance ?? g - (c.creditsUsed || 0)
              const ratio = g > 0 ? Math.min(1, (c.creditsUsed || 0) / g) : 0
              return (
                <div key={c.id} className="mcard" data-dim={!c.enabled || c.status === 'expired'} style={{ ['--i' as string]: i }}>
                  <div className="mcard-head">
                    <Dot tone={c.status === 'expired' ? 'bad' : !c.enabled ? 'warn' : 'ok'} />
                    <div className="grow">
                      <div className="mcard-title">
                        <span className="name">{c.name || '未命名'}</span>
                        {c.status === 'expired' && <Badge tone="bad">已过期</Badge>}
                        {c.status === 'disabled' && <Badge tone="bad">已停用</Badge>}
                      </div>
                      <div className="mcard-sub" style={{ alignItems: 'center' }}>
                        <span className="key-code" style={{ maxWidth: 170 }}>
                          {c.key}
                        </span>
                        <CopyButton value={c.key} label="复制 Key" done="Key 已复制" />
                        <CopyButton value={() => location.origin} label="复制接口地址" done="接口地址已复制" glyph={<Globe size={14} />} />
                      </div>
                    </div>
                    <Switch checked={c.enabled} onChange={(v) => toggle(c, v)} label={c.enabled ? '停用' : '启用'} />
                  </div>
                  <div>
                    <div className="quota-top">
                      <span>{c.expiresAt ? `到期 ${formatDateTime(c.expiresAt).slice(0, 10)}` : '跟随本卡到期'}</span>
                      <span>
                        <b className={bal < 0 ? 't-bad' : undefined}>{g > 0 ? fmt(bal) : '不限'}</b> / {g > 0 ? fmt(g) : '∞'}
                      </span>
                    </div>
                    <Bar value={ratio} tone={usageTone(ratio)} />
                  </div>
                  <div className="mcard-foot">
                    <Button size="sm" icon={<PencilLine />} onClick={() => setEditing(c)}>
                      编辑
                    </Button>
                    <span className="grow" />
                    <Button size="sm" variant="danger-soft" icon={<Trash2 />} onClick={() => remove(c)}>
                      删除
                    </Button>
                  </div>
                </div>
              )
            })}
          </div>
        ) : (
          <div className="table-wrap">
            <table className="table" style={{ minWidth: 820 }}>
              <thead>
                <tr>
                  <th>子卡</th>
                  <th>余额</th>
                  <th>到期</th>
                  <th style={{ width: 64 }}>启用</th>
                  <th className="right" style={{ width: 110 }}>
                    操作
                  </th>
                </tr>
              </thead>
              <tbody>
                <AnimatePresence initial={false}>
                  {d.children.map((c) => {
                    const g = c.creditsGranted || 0
                    const bal = c.balance ?? g - (c.creditsUsed || 0)
                    const ratio = g > 0 ? Math.min(1, (c.creditsUsed || 0) / g) : 0
                    return (
                      <motion.tr key={c.id} data-dim={!c.enabled || c.status === 'expired'} initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}>
                        <td>
                          <div className="ident">
                            <Dot tone={c.status === 'expired' ? 'bad' : !c.enabled ? 'warn' : 'ok'} />
                            <div className="ident-main">
                              <div className="ident-title">
                                <span className="name">{c.name || '未命名'}</span>
                                {c.status === 'expired' && <Badge tone="bad">已过期</Badge>}
                                {c.status === 'disabled' && <Badge tone="bad">已停用</Badge>}
                              </div>
                              <div className="ident-sub" style={{ alignItems: 'center' }}>
                                <span className="key-code">{c.key}</span>
                                <span>
                                  <CopyButton value={c.key} label="复制 Key" done="Key 已复制" />
                                  <CopyButton value={() => location.origin} label="复制接口地址" done="接口地址已复制" glyph={<Globe size={14} />} />
                                </span>
                              </div>
                            </div>
                          </div>
                        </td>
                        <td>
                          <div className="quota-cell">
                            <div className="quota-top">
                              <b className={bal < 0 ? 't-bad' : undefined}>{g > 0 ? fmt(bal) : '不限'}</b>
                              <span>/ {g > 0 ? fmt(g) : '∞'}</span>
                            </div>
                            <Bar value={ratio} tone={usageTone(ratio)} />
                          </div>
                        </td>
                        <td className="xs nowrap">{c.expiresAt ? formatDateTime(c.expiresAt) : <span className="muted">跟随本卡</span>}</td>
                        <td>
                          <Switch checked={c.enabled} onChange={(v) => toggle(c, v)} label={c.enabled ? '停用' : '启用'} />
                        </td>
                        <td>
                          <div className="row-actions">
                            <Button size="sm" variant="ghost" iconOnly icon={<PencilLine />} onClick={() => setEditing(c)}>
                              编辑
                            </Button>
                            <Button size="sm" variant="danger-soft" iconOnly icon={<Trash2 />} onClick={() => remove(c)}>
                              删除
                            </Button>
                          </div>
                        </td>
                      </motion.tr>
                    )
                  })}
                </AnimatePresence>
              </tbody>
            </table>
          </div>
        )}
      </div>

      <ChildDialog open={creating} onOpenChange={setCreating} allocatable={budget > 0 ? d.allocatable || 0 : null} onDone={reload} />
      <ChildDialog open={!!editing} onOpenChange={(o) => !o && setEditing(null)} edit={editing || undefined} allocatable={budget > 0 ? d.allocatable || 0 : null} onDone={reload} />
    </div>
  )
}

function Legend({ color, label, value }: { color: string; label: string; value: string }) {
  return (
    <span className="row" style={{ gap: 6 }}>
      <span style={{ width: 8, height: 8, borderRadius: 2, background: color, border: '1px solid var(--border)' }} />
      {label}
      <b className="num text-2">{value}</b>
    </span>
  )
}

function ChildDialog({
  open,
  onOpenChange,
  edit,
  allocatable,
  onDone,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  edit?: Child
  allocatable: number | null
  onDone: () => void
}) {
  const [name, setName] = useState('')
  const [credit, setCredit] = useState('')
  const [days, setDays] = useState('')
  const [busy, setBusy] = useState(false)
  const [seeded, setSeeded] = useState<string | null>(null)
  const marker = open ? edit?.id || 'new' : null
  if (marker !== seeded) {
    setSeeded(marker)
    if (open) {
      setName(edit?.name || '')
      setCredit(edit ? String(edit.creditsGranted || '') : '')
      setDays('')
    }
  }

  // When editing, the child's current grant is freed first, so it is available on top of the pool.
  const cap = allocatable == null ? null : allocatable + (edit ? Math.max(edit.creditsGranted || 0, edit.creditsUsed || 0) : 0)
  const n = parseFloat(credit)

  const submit = async () => {
    if (!name.trim()) return toast.warning('请填写子卡名称')
    if (!Number.isFinite(n) || n <= 0) return toast.warning('额度要大于 0')
    if (cap != null && n > cap + 1e-6) return toast.warning(`超出可分配额度，最多 ${fmt(cap)}`)
    setBusy(true)
    try {
      if (edit) {
        await uapi(`/children/${encodeURIComponent(edit.id)}`, { method: 'PUT', body: { name: name.trim(), creditLimit: n } })
        toast.success('已更新')
      } else {
        const body: Record<string, unknown> = { name: name.trim(), creditLimit: n }
        const dd = parseFloat(days)
        if (dd > 0) body.durationDays = dd
        await uapi('/children', { method: 'POST', body })
        toast.success('子卡已创建', { description: '在列表里复制 Key 发给你的客户' })
      }
      onDone()
      onOpenChange(false)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !busy && onOpenChange(o)}
      title={edit ? '编辑子卡' : '开设子卡'}
      description={edit ? '额度是这张子卡的总额度，不能低于它已经用掉的部分。' : '新子卡会继承本卡绑定的账号，Key 自动生成。'}
      width={460}
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={busy}>
            取消
          </Button>
          <Button variant="primary" loading={busy} onClick={submit}>
            {edit ? '保存' : '创建'}
          </Button>
        </>
      }
    >
      <form
        className="stack"
        style={{ gap: 14 }}
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <Field label="名称" hint="名称在全站唯一">
          <Input data-autofocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：客户A" />
        </Field>
        <Field label="额度（积分）" hint={cap != null ? `当前最多可分配 ${fmt(cap)}` : undefined}>
          <Input
            value={credit}
            onChange={(e) => setCredit(e.target.value.replace(/[^\d.]/g, ''))}
            inputMode="decimal"
            placeholder="例如 1000"
            className="num"
            invalid={cap != null && Number.isFinite(n) && n > cap + 1e-6}
            suffix={
              cap != null ? (
                <Button size="sm" variant="ghost" onClick={() => setCredit(String(Math.floor(cap * 100) / 100))}>
                  全部
                </Button>
              ) : undefined
            }
          />
        </Field>
        {!edit && (
          <Field label="有效期（天）" hint="留空表示跟随本卡到期">
            <Input value={days} onChange={(e) => setDays(e.target.value.replace(/[^\d.]/g, ''))} inputMode="decimal" placeholder="留空" className="num" />
          </Field>
        )}
        {edit && (
          <div className="callout">
            <Link2 />
            <span>
              已用 <b className="num">{fmt(edit.creditsUsed || 0)}</b> 积分
            </span>
          </div>
        )}
      </form>
    </Dialog>
  )
}
