import { useEffect, useMemo, useState } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import { Search } from 'lucide-react'
import { toast } from 'sonner'
import { api, type Account } from '../../api'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Field, Input } from '@/ui/Field'
import { Checkbox, Select, SwitchRow } from '@/ui/controls'
import { copyText } from '@/lib/clipboard'
import { formatNumber, fromDatetimeLocal, toDatetimeLocal } from '@/lib/format'
import { displayName } from '../accounts/util'
import { keyName, type NKey } from './util'

type LimitMode = 'default' | 'unlimited' | 'custom'
const NONE = '__none__'

function limitToForm(v: number | null | undefined, fallback: number): [LimitMode, string] {
  if (v == null) return ['default', String(fallback)]
  if (v === 0) return ['unlimited', String(fallback)]
  return ['custom', String(v)]
}
function formToLimit(mode: LimitMode, value: string): number | null {
  if (mode === 'default') return null
  if (mode === 'unlimited') return 0
  const n = parseInt(value, 10)
  return Number.isFinite(n) && n >= 1 ? n : null
}

const DAY = 86400
const EXPIRY_PRESETS: { label: string; days: number }[] = [
  { label: '永久', days: 0 },
  { label: '7 天', days: 7 },
  { label: '30 天', days: 30 },
  { label: '90 天', days: 90 },
  { label: '365 天', days: 365 },
]

export function KeyFormDialog({
  open,
  edit,
  parentId,
  keys,
  accounts,
  onOpenChange,
  onSaved,
}: {
  open: boolean
  edit?: NKey
  parentId?: string
  keys: NKey[]
  accounts: Account[]
  onOpenChange: (o: boolean) => void
  onSaved: () => void
}) {
  const [name, setName] = useState('')
  const [keyValue, setKeyValue] = useState('')
  const [enabled, setEnabled] = useState(true)
  const [credit, setCredit] = useState('0')
  const [tokenLimit, setTokenLimit] = useState('0')
  const [concMode, setConcMode] = useState<LimitMode>('default')
  const [conc, setConc] = useState('1')
  const [rpmMode, setRpmMode] = useState<LimitMode>('default')
  const [rpm, setRpm] = useState('20')
  const [expires, setExpires] = useState('')
  const [parent, setParent] = useState(NONE)
  const [bound, setBound] = useState<Set<string>>(new Set())
  const [accKw, setAccKw] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(edit?.name || '')
    setKeyValue('')
    setEnabled(edit ? edit.enabled : true)
    setCredit(String(edit ? edit.granted || 0 : 0))
    setTokenLimit(String(edit?.tokenLimit || 0))
    const [cm, cv] = limitToForm(edit ? edit.maxConcurrency : undefined, 1)
    setConcMode(cm)
    setConc(cv)
    const [rm, rv] = limitToForm(edit ? edit.maxRPM : undefined, 20)
    setRpmMode(rm)
    setRpm(rv)
    setExpires(edit ? toDatetimeLocal(edit.expiresAt) : '')
    const pid = edit ? edit.parentKeyId : parentId || ''
    setParent(pid || NONE)
    setBound(new Set(edit?.boundAccountIds || (pid ? keys.find((k) => k.id === pid)?.boundAccountIds : []) || []))
    setAccKw('')
    setBusy(false)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, edit, parentId])

  const parents = useMemo(() => keys.filter((k) => !k.parentKeyId && k.id !== edit?.id), [keys, edit])
  const parentKey = parent !== NONE ? keys.find((k) => k.id === parent) : undefined
  const visibleAccounts = accounts.filter((a) => !accKw || `${a.email} ${a.nickname}`.toLowerCase().includes(accKw.toLowerCase()))

  const setPreset = (days: number) => {
    if (!days) return setExpires('')
    setExpires(toDatetimeLocal(Math.floor(Date.now() / 1000) + days * DAY))
  }

  const submit = async () => {
    const c = parseFloat(credit)
    const quota = Number.isFinite(c) && c > 0 ? c : 0
    if (parentKey && quota <= 0) return toast.warning('子卡必须设置大于 0 的额度')
    const t = parseInt(tokenLimit, 10)
    const payload: Record<string, unknown> = {
      name: name.trim(),
      enabled,
      tokenLimit: Number.isFinite(t) && t > 0 ? t : 0,
      creditLimit: quota,
      creditsGranted: quota,
      maxConcurrency: formToLimit(concMode, conc),
      maxRPM: formToLimit(rpmMode, rpm),
      expiresAt: fromDatetimeLocal(expires),
      parentKeyId: parent === NONE ? '' : parent,
      boundAccountIds: [...bound],
    }
    setBusy(true)
    try {
      if (edit) {
        await api(`/api-keys/${encodeURIComponent(edit.id)}`, { method: 'PUT', body: payload })
        toast.success('卡密已更新')
      } else {
        if (keyValue.trim()) payload.key = keyValue.trim()
        const d = await api<{ key?: string; apiKey?: { key: string } }>('/api-keys', { method: 'POST', body: payload })
        const created = d.key || d.apiKey?.key || ''
        let copied = false
        if (created) {
          try {
            await copyText(created)
            copied = true
          } catch {
            /* fall through */
          }
        }
        toast.success('卡密已创建', { description: copied ? 'Key 已复制到剪贴板' : '可以在列表里复制 Key' })
      }
      onSaved()
      onOpenChange(false)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const title = edit ? `编辑「${keyName(edit)}」` : parentKey ? `给「${keyName(parentKey)}」开子卡` : '新建卡密'

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !busy && onOpenChange(o)}
      title={title}
      description={edit ? '额度是这张卡的总额度（含充值），修改会直接覆盖。' : '不填 Key 会自动生成；额度填 0 表示不限额。'}
      width={640}
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
      <div className="stack" style={{ gap: 20 }}>
        <section className="stack" style={{ gap: 12 }}>
          <div className="section-title" style={{ marginBottom: 0 }}>
            基本
          </div>
          <div className="grid grid-2" style={{ gap: 12 }}>
            <Field label="名称" hint="客户名、订单号等，方便查找">
              <Input data-autofocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：客户A" />
            </Field>
            <Field label="Key" hint={edit ? '创建后不能修改' : '留空自动生成 sk- 开头的 Key'}>
              {edit ? <Input value={edit.keyMasked} readOnly mono /> : <Input value={keyValue} onChange={(e) => setKeyValue(e.target.value)} placeholder="自动生成" mono />}
            </Field>
          </div>
          <SwitchRow checked={enabled} onChange={setEnabled} title="启用" hint="关闭后这张卡立即无法调用" />
        </section>

        <section className="stack" style={{ gap: 12 }}>
          <div className="section-title" style={{ marginBottom: 0 }}>
            额度与限制
          </div>
          <div className="grid grid-2" style={{ gap: 12 }}>
            <Field
              label="积分额度"
              hint={parentKey ? `从父卡额度池划拨，父卡当前余额 ${formatNumber(Math.round(parentKey.balance))}` : '0 表示不限额'}
            >
              <Input value={credit} onChange={(e) => setCredit(e.target.value.replace(/[^\d.]/g, ''))} inputMode="decimal" className="num" />
            </Field>
            <Field label="Token 上限" hint="0 表示不限制">
              <Input value={tokenLimit} onChange={(e) => setTokenLimit(e.target.value.replace(/\D/g, ''))} inputMode="numeric" className="num" />
            </Field>
            <LimitField label="最大并发" mode={concMode} setMode={setConcMode} value={conc} setValue={setConc} hint="默认跟随设置页的系统值" />
            <LimitField label="RPM 上限" mode={rpmMode} setMode={setRpmMode} value={rpm} setValue={setRpm} hint="每 60 秒最多请求数" />
          </div>
          <Field label="到期时间" hint="留空表示永久有效，按本机时区">
            <div className="stack" style={{ gap: 8 }}>
              <Input type="datetime-local" value={expires} onChange={(e) => setExpires(e.target.value)} />
              <div className="row row-wrap" style={{ gap: 6 }}>
                {EXPIRY_PRESETS.map((p) => (
                  <Button key={p.label} size="sm" variant="secondary" onClick={() => setPreset(p.days)}>
                    {p.label}
                  </Button>
                ))}
              </div>
            </div>
          </Field>
        </section>

        <section className="stack" style={{ gap: 12 }}>
          <div className="section-title" style={{ marginBottom: 0 }}>
            归属与路由
          </div>
          <Field label="父卡密" hint="设为子卡后，额度从父卡的额度池里划拨">
            <Select
              value={parent}
              onChange={setParent}
              options={[
                { value: NONE, label: '无（独立卡）' },
                ...parents.map((p) => ({ value: p.id, label: `${keyName(p)} · ${p.keyMasked}` })),
              ]}
            />
          </Field>
          <Field label="绑定账号" hint={bound.size ? `只会使用选中的 ${bound.size} 个账号` : '不选表示可以使用全部账号'}>
            <div className="export-list" style={{ maxHeight: 220 }}>
              {accounts.length > 6 && (
                <div style={{ padding: 8, borderBottom: '1px solid var(--divider)' }}>
                  <Input icon={<Search />} value={accKw} onChange={(e) => setAccKw(e.target.value)} placeholder="筛选账号" style={{ height: 30 }} />
                </div>
              )}
              {!accounts.length ? (
                <div className="xs muted" style={{ padding: 12 }}>
                  还没有账号
                </div>
              ) : (
                visibleAccounts.map((a) => (
                  <label key={a.id} className="export-row">
                    <Checkbox
                      checked={bound.has(a.id)}
                      onChange={(v) =>
                        setBound((s) => {
                          const n = new Set(s)
                          if (v) n.add(a.id)
                          else n.delete(a.id)
                          return n
                        })
                      }
                      label={displayName(a)}
                    />
                    <span className="ellipsis" style={{ fontSize: 13 }}>
                      {displayName(a)}
                    </span>
                    {!a.enabled && <span className="xs faint">已禁用</span>}
                  </label>
                ))
              )}
            </div>
          </Field>
        </section>
      </div>
    </Dialog>
  )
}

function LimitField({
  label,
  mode,
  setMode,
  value,
  setValue,
  hint,
}: {
  label: string
  mode: LimitMode
  setMode: (m: LimitMode) => void
  value: string
  setValue: (v: string) => void
  hint: string
}) {
  return (
    <Field label={label} hint={hint}>
      <div className="row" style={{ gap: 8 }}>
        <Select
          value={mode}
          onChange={setMode}
          style={{ flex: mode === 'custom' ? '0 0 128px' : '1 1 auto' }}
          options={[
            { value: 'default', label: '跟随默认' },
            { value: 'unlimited', label: '不限制' },
            { value: 'custom', label: '自定义' },
          ]}
        />
        <AnimatePresence initial={false}>
          {mode === 'custom' && (
            <motion.div
              initial={{ opacity: 0, width: 0 }}
              animate={{ opacity: 1, width: 'auto' }}
              exit={{ opacity: 0, width: 0 }}
              transition={{ duration: 0.22 }}
              style={{ overflow: 'hidden', flex: '1 1 auto' }}
            >
              <Input value={value} onChange={(e) => setValue(e.target.value.replace(/\D/g, ''))} inputMode="numeric" className="num" />
            </motion.div>
          )}
        </AnimatePresence>
      </div>
    </Field>
  )
}
