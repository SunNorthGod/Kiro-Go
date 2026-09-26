import { useEffect, useRef, useState } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import { Building2, ExternalLink, FileJson, FileUp, KeyRound, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '../../api'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Field, Input, Textarea } from '@/ui/Field'
import { CopyButton } from '@/ui/misc'
import { credentialPayload, normalizeCredentialRecord } from './util'

type Method = 'json' | 'apikey' | 'sso'

const METHODS: { id: Method; title: string; desc: string; icon: React.ReactNode }[] = [
  { id: 'json', title: '凭证 JSON', desc: '粘贴或拖入导出的凭证文件', icon: <FileJson /> },
  { id: 'apikey', title: 'Kiro API Key', desc: '批量导入 ksk_ 开头的 Key', icon: <KeyRound /> },
  { id: 'sso', title: '企业 SSO', desc: 'IAM Identity Center 授权', icon: <Building2 /> },
]

export function AddAccountDialog({ open, onOpenChange, onDone }: { open: boolean; onOpenChange: (o: boolean) => void; onDone: () => void }) {
  const [method, setMethod] = useState<Method>('json')
  const [busy, setBusy] = useState(false)
  const submitRef = useRef<() => void>(() => {})
  const [footer, setFooter] = useState<{ label: string; disabled?: boolean }>({ label: '导入' })

  useEffect(() => {
    if (!open) setBusy(false)
  }, [open])

  const finish = () => {
    onDone()
    onOpenChange(false)
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !busy && onOpenChange(o)}
      title="添加账号"
      description="账号加入后会立即同步一次额度和订阅信息。"
      width={600}
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={busy}>
            取消
          </Button>
          <Button variant="primary" loading={busy} disabled={footer.disabled} onClick={() => submitRef.current()}>
            {footer.label}
          </Button>
        </>
      }
    >
      <div className="method-tabs">
        {METHODS.map((m) => (
          <button key={m.id} type="button" className="method-tab" data-active={method === m.id} onClick={() => !busy && setMethod(m.id)}>
            {m.icon}
            <span className="t">{m.title}</span>
            <span className="d">{m.desc}</span>
          </button>
        ))}
      </div>
      <AnimatePresence mode="wait" initial={false}>
        <motion.div
          key={method}
          initial={{ opacity: 0, x: 10 }}
          animate={{ opacity: 1, x: 0 }}
          exit={{ opacity: 0, x: -10 }}
          transition={{ duration: 0.18, ease: [0.16, 1, 0.3, 1] }}
        >
          {method === 'json' && <JsonForm setBusy={setBusy} submitRef={submitRef} setFooter={setFooter} onFinish={finish} onPartial={onDone} />}
          {method === 'apikey' && <ApiKeyForm setBusy={setBusy} submitRef={submitRef} setFooter={setFooter} onFinish={finish} />}
          {method === 'sso' && <SsoForm setBusy={setBusy} submitRef={submitRef} setFooter={setFooter} onFinish={finish} />}
        </motion.div>
      </AnimatePresence>
    </Dialog>
  )
}

interface FormProps {
  setBusy: (b: boolean) => void
  submitRef: React.MutableRefObject<() => void>
  setFooter: (f: { label: string; disabled?: boolean }) => void
  onFinish: () => void
}

/* ---------------- credential JSON ---------------- */
function JsonForm({ setBusy, submitRef, setFooter, onFinish, onPartial }: FormProps & { onPartial: () => void }) {
  const [text, setText] = useState('')
  const [over, setOver] = useState(false)
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null)
  const [errors, setErrors] = useState<string[]>([])
  const fileRef = useRef<HTMLInputElement>(null)

  const readFile = (f: File | undefined) => {
    if (!f) return
    const r = new FileReader()
    r.onload = () => setText(String(r.result || ''))
    r.readAsText(f)
  }

  const submit = async () => {
    const raw = text.trim()
    if (!raw) return toast.warning('请先粘贴凭证 JSON')
    let json: unknown
    try {
      json = JSON.parse(raw)
    } catch {
      return toast.warning('JSON 格式不正确', { description: '支持单个对象、数组或 Kiro Account Manager 导出文件' })
    }
    const obj = json as { accounts?: unknown }
    const list: unknown[] = Array.isArray(obj?.accounts) ? obj.accounts : Array.isArray(json) ? json : [json]
    setBusy(true)
    setErrors([])
    let ok = 0
    const errs: string[] = []
    for (let i = 0; i < list.length; i++) {
      setProgress({ done: i, total: list.length })
      const item = normalizeCredentialRecord(list[i])
      const payload = credentialPayload(item)
      const label = item.email || item.id || `第 ${i + 1} 条`
      if (!payload) {
        errs.push(`${label}：缺少 refreshToken 或 kiroApiKey`)
        continue
      }
      try {
        await api('/auth/credentials', { method: 'POST', body: payload })
        ok++
      } catch (e) {
        errs.push(`${label}：${(e as Error).message}`)
      }
    }
    setProgress(null)
    setBusy(false)
    if (ok && !errs.length) {
      toast.success(`成功添加 ${ok} 个账号`)
      onFinish()
    } else if (ok) {
      toast.warning(`成功 ${ok} 个，失败 ${errs.length} 个`)
      setErrors(errs)
      onPartial()
    } else {
      setErrors(errs)
      toast.error('没有导入成功的账号')
    }
  }

  submitRef.current = submit
  useEffect(() => setFooter({ label: '导入' }), [setFooter])

  return (
    <div className="stack" style={{ gap: 12 }}>
      <Field
        label="凭证 JSON"
        aside={
          <Button size="sm" variant="ghost" icon={<FileUp />} onClick={() => fileRef.current?.click()}>
            选择文件
          </Button>
        }
        hint="支持单个对象、数组或 Kiro Account Manager 导出格式。OAuth 账号必须带 refreshToken，IdC 账号可附带 clientId / clientSecret。"
      >
        <div
          className="dropzone"
          data-over={over}
          onDragOver={(e) => {
            e.preventDefault()
            setOver(true)
          }}
          onDragLeave={() => setOver(false)}
          onDrop={(e) => {
            e.preventDefault()
            setOver(false)
            readFile(e.dataTransfer.files?.[0])
          }}
        >
          <Textarea
            data-autofocus
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={9}
            spellCheck={false}
            placeholder={'{"refreshToken":"...","clientId":"...","clientSecret":"...","region":"us-east-1"}\n\n也可以把 .json 文件直接拖到这里'}
          />
        </div>
        <input ref={fileRef} type="file" accept=".json,application/json,text/plain" hidden onChange={(e) => readFile(e.target.files?.[0])} />
      </Field>
      {progress && (
        <div className="xs muted num">
          正在导入 {progress.done + 1} / {progress.total}…
        </div>
      )}
      {errors.length > 0 && (
        <div className="callout callout-bad" style={{ maxHeight: 160, overflow: 'auto' }}>
          <TriangleAlert />
          <div className="stack" style={{ gap: 4 }}>
            {errors.map((e, i) => (
              <span key={i} style={{ wordBreak: 'break-all' }}>
                {e}
              </span>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

/* ---------------- Kiro API key ---------------- */
function ApiKeyForm({ setBusy, submitRef, setFooter, onFinish }: FormProps) {
  const [keys, setKeys] = useState('')
  const [region, setRegion] = useState('us-east-1')
  const [errors, setErrors] = useState<string[]>([])
  const count = keys.split('\n').filter((l) => l.trim()).length

  const submit = async () => {
    const apiKey = keys.trim()
    if (!apiKey) return toast.warning('请至少填写一个 API Key')
    setBusy(true)
    setErrors([])
    try {
      const d = await api<{ accounts: { id: string }[] | null; errors: string[] | null }>('/auth/api-key', {
        method: 'POST',
        body: { apiKey, region: region.trim() || 'us-east-1' },
      })
      const n = d.accounts?.length || 0
      const errs = d.errors || []
      if (errs.length) {
        setErrors(errs)
        toast.warning(`成功导入 ${n} 个，失败 ${errs.length} 个`)
        if (!n) return
      } else {
        toast.success(`成功导入 ${n} 个账号`)
      }
      onFinish()
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  submitRef.current = submit
  useEffect(() => setFooter({ label: count > 1 ? `导入 ${count} 个` : '导入' }), [setFooter, count])

  return (
    <div className="stack" style={{ gap: 14 }}>
      <Field label="API Key" hint="每行一个，以 ksk_ 开头。这类账号直接用 Key 调用，不需要刷新令牌。">
        <Textarea data-autofocus value={keys} onChange={(e) => setKeys(e.target.value)} rows={7} spellCheck={false} placeholder={'ksk_xxxxxxxx\nksk_yyyyyyyy'} />
      </Field>
      <Field label="区域">
        <Input value={region} onChange={(e) => setRegion(e.target.value)} mono style={{ maxWidth: 220 }} />
      </Field>
      {errors.length > 0 && (
        <div className="callout callout-bad">
          <TriangleAlert />
          <div className="stack" style={{ gap: 4 }}>
            {errors.map((e, i) => (
              <span key={i}>{e}</span>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

/* ---------------- IAM Identity Center ---------------- */
function SsoForm({ setBusy, submitRef, setFooter, onFinish }: FormProps) {
  const [name, setName] = useState('')
  const [startUrl, setStartUrl] = useState('')
  const [session, setSession] = useState<{ id: string; url: string } | null>(null)
  const [callback, setCallback] = useState('')

  const start = async () => {
    if (!name.trim()) return toast.warning('请填写备注名')
    if (!startUrl.trim()) return toast.warning('请填写门户地址（Start URL）')
    setBusy(true)
    try {
      const d = await api<{ sessionId: string; authorizeUrl: string }>('/auth/iam-sso/start', {
        method: 'POST',
        body: { startUrl: startUrl.trim(), name: name.trim() },
      })
      if (!d.sessionId) throw new Error('未拿到登录会话')
      setSession({ id: d.sessionId, url: d.authorizeUrl || '' })
      if (d.authorizeUrl) window.open(d.authorizeUrl, '_blank', 'noopener')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const complete = async () => {
    if (!session) return
    if (!callback.trim()) return toast.warning('请粘贴登录完成后跳转到的地址')
    setBusy(true)
    try {
      await api('/auth/iam-sso/complete', { method: 'POST', body: { sessionId: session.id, callbackUrl: callback.trim() } })
      toast.success('企业账号已添加')
      onFinish()
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  submitRef.current = session ? complete : start
  useEffect(() => setFooter({ label: session ? '完成登录' : '开始登录' }), [setFooter, session])

  return (
    <div className="steps">
      <div className="step" data-done={!!session} data-active={!session}>
        <span className="step-num">1</span>
        <div className="stack" style={{ gap: 12 }}>
          <div className="grid grid-2" style={{ gap: 12 }}>
            <Field label="备注名" hint="显示在账号列表里">
              <Input data-autofocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：团队A-张三" disabled={!!session} />
            </Field>
            <Field label="门户地址" hint="区域会自动探测">
              <Input value={startUrl} onChange={(e) => setStartUrl(e.target.value)} placeholder="https://d-xxxxxxxxxx.awsapps.com/start" mono disabled={!!session} />
            </Field>
          </div>
        </div>
      </div>
      <AnimatePresence initial={false}>
        {session && (
          <motion.div
            className="step"
            data-active
            initial={{ opacity: 0, height: 0 }}
            animate={{ opacity: 1, height: 'auto' }}
            transition={{ duration: 0.3, ease: [0.16, 1, 0.3, 1] }}
            style={{ overflow: 'hidden' }}
          >
            <span className="step-num">2</span>
            <div className="stack" style={{ gap: 12 }}>
              <Field label="登录链接" hint="已在新标签页打开。如果被浏览器拦截，点「打开」。">
                <div className="copy-line">
                  <code>{session.url}</code>
                  <CopyButton value={session.url} label="复制链接" done="链接已复制" />
                  <Button size="sm" variant="ghost" icon={<ExternalLink />} onClick={() => window.open(session.url, '_blank', 'noopener')}>
                    打开
                  </Button>
                </div>
              </Field>
              <Field label="回调地址" hint="登录完成后浏览器会跳到 127.0.0.1 并显示无法访问，这是正常的。把地址栏的完整地址粘贴到这里。会话 15 分钟内有效。">
                <Textarea value={callback} onChange={(e) => setCallback(e.target.value)} rows={3} spellCheck={false} placeholder="http://127.0.0.1/oauth/callback?code=..." autoFocus />
              </Field>
            </div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  )
}
