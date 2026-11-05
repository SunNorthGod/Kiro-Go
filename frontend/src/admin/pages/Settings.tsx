import { useEffect, useState, type ReactNode } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { Lock } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '../api'
import { isRemembered, storePassword } from '../auth'
import { setAdminPassword } from '../api'
import { Button } from '@/ui/Button'
import { Field, Input, PasswordInput } from '@/ui/Field'
import { Segmented } from '@/ui/controls'
import { Skeleton } from '@/ui/misc'

export function SettingsPage() {
  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="card" style={{ padding: '6px 24px' }}>
        <Section title="管理员密码" desc="登录后台用的密码。修改后当前会话会自动切换到新密码。">
          <PasswordForm />
        </Section>
        <Section title="出站代理" desc="网关访问 Kiro 上游时使用的代理，保存后立即生效。账号可以在详情里单独设置代理，优先级更高。">
          <ProxyForm />
        </Section>
        <Section title="卡密默认限制" desc="没有单独设置的卡密会使用这里的值。填 0 表示不限制。">
          <LimitsForm />
        </Section>
      </div>

    </div>
  )
}

function Section({ title, desc, children }: { title: string; desc: string; children: ReactNode }) {
  return (
    <section className="settings-grid">
      <div>
        <h3>{title}</h3>
        <p>{desc}</p>
      </div>
      <div className="settings-form">{children}</div>
    </section>
  )
}

function SaveRow({ busy, onClick, label = '保存', disabled }: { busy: boolean; onClick: () => void; label?: string; disabled?: boolean }) {
  return (
    <div>
      <Button variant="primary" loading={busy} onClick={onClick} disabled={disabled}>
        {label}
      </Button>
    </div>
  )
}

/* ---------------- password ---------------- */
function PasswordForm() {
  const [pwd, setPwd] = useState('')
  const [again, setAgain] = useState('')
  const [busy, setBusy] = useState(false)
  const mismatch = again.length > 0 && again !== pwd
  const save = async () => {
    if (!pwd) return toast.warning('请输入新密码')
    if (pwd !== again) return toast.warning('两次输入的密码不一致')
    setBusy(true)
    try {
      await api('/settings', { method: 'POST', body: { password: pwd } })
      storePassword(pwd, isRemembered())
      setAdminPassword(pwd)
      setPwd('')
      setAgain('')
      toast.success('密码已修改')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <div className="grid grid-2" style={{ gap: 12 }}>
        <Field label="新密码">
          <PasswordInput icon={<Lock />} value={pwd} onChange={(e) => setPwd(e.target.value)} autoComplete="new-password" placeholder="输入新密码" />
        </Field>
        <Field label="确认新密码" error={mismatch ? '两次输入不一致' : undefined}>
          <PasswordInput icon={<Lock />} value={again} onChange={(e) => setAgain(e.target.value)} autoComplete="new-password" invalid={mismatch} placeholder="再输入一次" />
        </Field>
      </div>
      <SaveRow busy={busy} onClick={save} label="修改密码" disabled={!pwd || !again} />
    </>
  )
}

/* ---------------- proxy ---------------- */
type ProxyType = 'none' | 'socks5' | 'http'
function ProxyForm() {
  const [loaded, setLoaded] = useState(false)
  const [type, setType] = useState<ProxyType>('none')
  const [host, setHost] = useState('')
  const [port, setPort] = useState('')
  const [user, setUser] = useState('')
  const [pass, setPass] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api<{ proxyURL: string }>('/proxy')
      .then((d) => {
        const url = d.proxyURL || ''
        if (url) {
          try {
            const u = new URL(url)
            const scheme = u.protocol.replace(':', '')
            setType(scheme.startsWith('socks5') ? 'socks5' : 'http')
            setHost(u.hostname)
            setPort(u.port)
            setUser(decodeURIComponent(u.username))
            setPass(decodeURIComponent(u.password))
          } catch {
            setType('none')
          }
        }
      })
      .catch((e) => toast.error('读取代理设置失败：' + (e as Error).message))
      .finally(() => setLoaded(true))
  }, [])

  const save = async () => {
    let url = ''
    if (type !== 'none') {
      if (!host.trim() || !port.trim()) return toast.warning('请填写代理地址和端口')
      const u = user.trim()
      const p = pass.trim()
      const auth = u ? (p ? `${encodeURIComponent(u)}:${encodeURIComponent(p)}@` : `${encodeURIComponent(u)}@`) : ''
      url = `${type}://${auth}${host.trim()}:${port.trim()}`
    }
    setBusy(true)
    try {
      await api('/proxy', { method: 'POST', body: { proxyURL: url } })
      toast.success(url ? '代理已保存并生效' : '已切换为直连')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!loaded) return <Skeleton h={36} w={320} />
  return (
    <>
      <Field label="代理类型">
        <div>
          <Segmented
            id="proxy-type"
            value={type}
            onChange={setType}
            items={[
              { value: 'none', label: '直连' },
              { value: 'socks5', label: 'SOCKS5' },
              { value: 'http', label: 'HTTP' },
            ]}
          />
        </div>
      </Field>
      <AnimatePresence initial={false}>
        {type !== 'none' && (
          <motion.div
            className="stack"
            style={{ gap: 12, overflow: 'hidden' }}
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.28, ease: [0.16, 1, 0.3, 1] }}
          >
            <div className="row" style={{ gap: 10, alignItems: 'flex-start' }}>
              <Field label="地址" className="grow">
                <Input value={host} onChange={(e) => setHost(e.target.value)} placeholder="127.0.0.1" mono />
              </Field>
              <Field label="端口" className="none">
                <Input value={port} onChange={(e) => setPort(e.target.value.replace(/\D/g, ''))} placeholder="1080" inputMode="numeric" mono style={{ width: 104 }} />
              </Field>
            </div>
            <div className="grid grid-2" style={{ gap: 12 }}>
              <Field label="用户名" hint="可选">
                <Input value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
              </Field>
              <Field label="密码" hint="可选">
                <PasswordInput value={pass} onChange={(e) => setPass(e.target.value)} autoComplete="new-password" />
              </Field>
            </div>
          </motion.div>
        )}
      </AnimatePresence>
      <SaveRow busy={busy} onClick={save} />
    </>
  )
}

/* ---------------- default limits ---------------- */
function LimitsForm() {
  const [conc, setConc] = useState<string | null>(null)
  const [rpm, setRpm] = useState('')
  const [busy, setBusy] = useState(false)
  const load = () =>
    api<{ defaultMaxConcurrency?: number; defaultMaxRPM?: number }>('/settings')
      .then((d) => {
        setConc(String(d.defaultMaxConcurrency ?? 5))
        setRpm(String(d.defaultMaxRPM ?? 20))
      })
      .catch((e) => toast.error('读取设置失败：' + (e as Error).message))
  useEffect(() => {
    load()
  }, [])
  const save = async () => {
    const c = parseInt(conc || '', 10)
    const r = parseInt(rpm, 10)
    setBusy(true)
    try {
      await api('/settings', {
        method: 'POST',
        body: { defaultMaxConcurrency: Number.isNaN(c) || c < 0 ? 5 : c, defaultMaxRPM: Number.isNaN(r) || r < 0 ? 20 : r },
      })
      toast.success('默认限制已保存')
      load()
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  if (conc == null) return <Skeleton h={36} w={320} />
  return (
    <>
      <div className="grid grid-2" style={{ gap: 12 }}>
        <Field label="默认并发数" hint="拥挤时每张卡密的并发基线，出厂值 5">
          <Input value={conc} onChange={(e) => setConc(e.target.value.replace(/\D/g, ''))} inputMode="numeric" className="num" />
        </Field>
        <Field label="默认 RPM 上限" hint="每 60 秒最多请求数，超出返回 429，出厂值 20">
          <Input value={rpm} onChange={(e) => setRpm(e.target.value.replace(/\D/g, ''))} inputMode="numeric" className="num" />
        </Field>
      </div>
      <SaveRow busy={busy} onClick={save} />
    </>
  )
}

/* ---------------- endpoint ---------------- */

/* ---------------- prompt filter ---------------- */
