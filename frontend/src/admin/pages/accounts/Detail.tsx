import { useEffect, useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Wand2 } from 'lucide-react'
import { toast } from 'sonner'
import { api, type Account, type CachedModel } from '../../api'
import { qk } from '../../queries'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Field, Input } from '@/ui/Field'
import { Badge, Bar, Switch, usageTone } from '@/ui/controls'
import { CopyButton, Skeleton } from '@/ui/misc'
import { formatCompact, formatDateTime, formatNumber } from '@/lib/format'
import { authLabel, displayName, isBanned, subscription } from './util'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const HEX_RE = /^[0-9a-f]{32}$|^[0-9a-f]{64}$/i

export function AccountDetailDialog({ id, onClose, onSaved }: { id: string | null; onClose: () => void; onSaved: () => void }) {
  const qc = useQueryClient()
  const open = !!id
  const acc = useQuery({
    queryKey: ['account', id],
    queryFn: () => api<Account>(`/accounts/${encodeURIComponent(id!)}`),
    enabled: open,
    staleTime: 0,
  })
  const models = useQuery({
    queryKey: ['account-models', id],
    queryFn: () => api<{ models: CachedModel[] }>(`/accounts/${encodeURIComponent(id!)}/models/cached`).then((d) => d.models || []),
    enabled: open,
  })
  const fallback = qc.getQueryData<Account[]>(qk.accounts)?.find((a) => a.id === id)
  const a = acc.data || fallback

  const [machineId, setMachineId] = useState('')
  const [weight, setWeight] = useState('0')
  const [proxyURL, setProxyURL] = useState('')
  const [saving, setSaving] = useState(false)
  const [formFor, setFormFor] = useState<string | null>(null)

  // Seed the form once per opened account (later polls must not clobber edits).
  useEffect(() => {
    if (a && formFor !== a.id && acc.isFetched) {
      setMachineId(a.machineId || '')
      setWeight(String(a.weight || 0))
      setProxyURL(a.proxyURL || '')
      setFormFor(a.id)
    }
  }, [a, formFor, acc.isFetched])
  useEffect(() => {
    if (!open) setFormFor(null)
  }, [open])

  const genMachineId = async () => {
    try {
      const d = await api<{ machineId: string }>('/generate-machine-id')
      if (d.machineId) setMachineId(d.machineId)
    } catch (e) {
      toast.error('生成失败：' + (e as Error).message)
    }
  }

  const save = async () => {
    if (!a) return
    const m = machineId.trim()
    if (m && !UUID_RE.test(m) && !HEX_RE.test(m)) return toast.warning('机器码格式不对', { description: '需要 UUID 或 32/64 位十六进制' })
    const p = proxyURL.trim()
    if (p && !/^(socks5|socks5h|http|https):\/\//i.test(p)) return toast.warning('代理地址格式不对', { description: '例如 socks5://host:port 或 http://host:port' })
    setSaving(true)
    try {
      await api(`/accounts/${encodeURIComponent(a.id)}`, {
        method: 'PUT',
        body: { machineId: m, weight: Math.max(0, parseInt(weight, 10) || 0), proxyURL: p },
      })
      toast.success('已保存')
      onSaved()
      onClose()
    } catch (e) {
      toast.error('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title={a ? displayName(a) : '账号详情'}
      description={a ? `${authLabel(a)} · ${a.region || 'us-east-1'}` : undefined}
      width={720}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            取消
          </Button>
          <Button variant="primary" loading={saving} onClick={save} disabled={!a}>
            保存
          </Button>
        </>
      }
    >
      {!a ? (
        <div className="stack">
          <Skeleton h={16} w="40%" />
          <Skeleton h={80} />
          <Skeleton h={80} />
        </div>
      ) : (
        <div>
          <Block title="基本信息">
            <div className="detail-grid">
              <KV label="邮箱 / 标识">
                <span className="row" style={{ gap: 4, justifyContent: 'flex-end', minWidth: 0 }}>
                  <span className="ellipsis" style={{ maxWidth: 220 }} title={a.email}>
                    {displayName(a)}
                  </span>
                  {a.email && <CopyButton value={a.email} label="复制" done="已复制" />}
                </span>
              </KV>
              <KV label="用户 ID">
                <span className="mono xs ellipsis" style={{ maxWidth: 220, display: 'inline-block' }} title={a.userId}>
                  {a.userId || '—'}
                </span>
              </KV>
              <KV label="状态">
                <StateBadges a={a} />
              </KV>
              <KV label="Token 到期">{a.expiresAt ? formatDateTime(a.expiresAt) : '长期有效'}</KV>
            </div>
          </Block>

          <Block title="调度设置">
            <div className="stack" style={{ gap: 12 }}>
              <div className="grid grid-2" style={{ gap: 12 }}>
                <Field label="优先级" hint="数值越大越优先调度，0 最低">
                  <Input value={weight} onChange={(e) => setWeight(e.target.value.replace(/\D/g, ''))} inputMode="numeric" className="num" />
                </Field>
                <Field label="独立代理" hint="留空使用全局代理">
                  <Input value={proxyURL} onChange={(e) => setProxyURL(e.target.value)} placeholder="socks5://host:port" mono />
                </Field>
              </div>
              <Field label="机器码" hint="UUID 格式；修改后下次请求生效">
                <div className="row" style={{ gap: 8 }}>
                  <Input value={machineId} onChange={(e) => setMachineId(e.target.value)} mono placeholder="UUID" />
                  <Button icon={<Wand2 />} onClick={genMachineId} className="none">
                    生成
                  </Button>
                </div>
              </Field>
            </div>
          </Block>

          <Block title="超额调用（Overages）">
            <OverageBlock key={a.id + (acc.data ? '-fresh' : '')} a={a} />
          </Block>

          <Block title="订阅与额度">
            <div className="detail-grid">
              <KV label="订阅">{a.subscriptionTitle || subscription(a.subscriptionType).label}</KV>
              <KV label="重置日期">{a.nextResetDate || '—'}</KV>
            </div>
            <QuotaLine label="主额度" cur={a.usageCurrent} limit={a.usageLimit} />
            {a.trialUsageLimit > 0 && (
              <>
                <QuotaLine label={`试用额度 · ${a.trialStatus || '—'}`} cur={a.trialUsageCurrent} limit={a.trialUsageLimit} />
                <div className="xs muted" style={{ marginTop: 6 }}>
                  试用到期 {formatDateTime(a.trialExpiresAt)}
                </div>
              </>
            )}
          </Block>

          <Block title="累计统计">
            <div className="summary-grid" style={{ gridTemplateColumns: 'repeat(4, minmax(0,1fr))' }}>
              <Summary label="请求" value={formatNumber(a.requestCount || 0)} />
              <Summary label="错误" value={formatNumber(a.errorCount || 0)} tone={a.errorCount ? 't-bad' : undefined} />
              <Summary label="Tokens" value={formatCompact(a.totalTokens || 0)} />
              <Summary label="积分" value={(a.totalCredits || 0).toFixed(2)} />
            </div>
          </Block>

          <Block title={`可用模型${models.data ? ` · ${models.data.length}` : ''}`}>
            {models.isLoading ? (
              <Skeleton h={60} />
            ) : models.isError ? (
              <div className="xs t-bad">模型列表加载失败</div>
            ) : !models.data?.length ? (
              <div className="xs muted">暂无缓存的模型，后台每 30 分钟同步一次。</div>
            ) : (
              <div className="model-list">
                {[...models.data]
                  .sort((x, y) => (x.modelId === 'auto' ? -1 : y.modelId === 'auto' ? 1 : (x.rateMultiplier || 1) - (y.rateMultiplier || 1)))
                  .map((m) => (
                    <div key={m.modelId} className="model-card" title={m.description}>
                      <div className="row row-between" style={{ gap: 6 }}>
                        <span className="mid">{m.modelId}</span>
                        <Badge tone="outline">{m.rateMultiplier || 1}×</Badge>
                      </div>
                      {m.description && <div className="desc">{m.description}</div>}
                    </div>
                  ))}
              </div>
            )}
          </Block>
        </div>
      )}
    </Dialog>
  )
}

function Block({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="detail-section">
      <div className="section-title">{title}</div>
      {children}
    </section>
  )
}

function KV({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="kv">
      <span className="muted">{label}</span>
      <b style={{ fontWeight: 520 }}>{children}</b>
    </div>
  )
}

function Summary({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="summary-item">
      <div className="l">{label}</div>
      <div className={`v ${tone || ''}`}>{value}</div>
    </div>
  )
}

function QuotaLine({ label, cur, limit }: { label: string; cur: number; limit: number }) {
  const ratio = limit > 0 ? Math.min(1, cur / limit) : 0
  return (
    <div style={{ marginTop: 12 }}>
      <div className="quota-top">
        <span>{label}</span>
        <span>
          <b>{cur.toFixed(1)}</b> / {limit.toFixed(0)}
          {limit > 0 && <span className="faint"> · {(ratio * 100).toFixed(0)}%</span>}
        </span>
      </div>
      <Bar value={ratio} tone={usageTone(ratio)} />
    </div>
  )
}

function StateBadges({ a }: { a: Account }) {
  if (a.banStatus === 'BANNED') return <Badge tone="bad">已封禁</Badge>
  if (a.banStatus === 'SUSPENDED') return <Badge tone="bad">已暂停</Badge>
  if (isBanned(a)) return <Badge tone="bad">{a.banStatus}</Badge>
  if (!a.hasToken) return <Badge tone="bad">无 Token</Badge>
  if (!a.enabled) return <Badge tone="bad">已禁用</Badge>
  return <Badge tone="ok">正常</Badge>
}

function OverageBlock({ a }: { a: Account }) {
  const qc = useQueryClient()
  const [status, setStatus] = useState((a.overageStatus || '').toUpperCase())
  const [busy, setBusy] = useState(false)
  const [info, setInfo] = useState({ cap: a.overageCap, rate: a.overageRate, cur: a.currentOverages, at: a.overageCheckedAt })
  const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE'

  const toggle = async (desired: boolean) => {
    setBusy(true)
    const prev = status
    setStatus(desired ? 'ENABLED' : 'DISABLED')
    try {
      const d = await api<{ overageStatus: string; overageCap: number; overageRate: number; currentOverages: number; overageCheckedAt: number }>(
        `/accounts/${encodeURIComponent(a.id)}/overage`,
        { method: 'POST', body: { enabled: desired } },
      )
      setStatus((d.overageStatus || '').toUpperCase())
      setInfo({ cap: d.overageCap, rate: d.overageRate, cur: d.currentOverages, at: d.overageCheckedAt })
      toast.success(desired ? 'Overages 已开启' : 'Overages 已关闭')
      qc.invalidateQueries({ queryKey: qk.accounts })
    } catch (e) {
      setStatus(prev)
      toast.error('切换失败：' + (e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const text =
    status === 'ENABLED' ? '已开启，额度用完后按 Overage 单价继续计费' : status === 'DISABLED' ? '已关闭，额度用完即停止' : '状态未知，等待后台同步'

  return (
    <div className="stack" style={{ gap: 12 }}>
      <label className="switch-row">
        <Switch checked={status === 'ENABLED'} onChange={toggle} disabled={busy || !capable} accent label="Overages" />
        <span className="stack" style={{ gap: 2 }}>
          <span className="strong" style={{ fontSize: 13.5, fontWeight: 560 }}>
            {busy ? '正在调用 AWS…' : text}
          </span>
          <span className="field-hint">{capable ? '开关会直接调用 AWS 修改该账号在 Kiro 后台的设置，立即生效。' : '当前订阅不支持 Overages。'}</span>
        </span>
      </label>
      <div className="summary-grid" style={{ gridTemplateColumns: 'repeat(4, minmax(0,1fr))' }}>
        <Summary label="上限" value={info.cap ? `$${Number(info.cap).toFixed(2)}` : '—'} />
        <Summary label="单价" value={info.rate ? `$${Number(info.rate).toFixed(2)}` : '—'} />
        <Summary label="已产生" value={`$${Number(info.cur || 0).toFixed(2)}`} tone={info.cur ? 't-warn' : undefined} />
        <Summary label="上次同步" value={info.at ? formatDateTime(info.at).slice(5) : '—'} />
      </div>
    </div>
  )
}
