import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, LayoutGroup, motion, useAnimationControls } from 'motion/react'
import { GitBranch, Gauge, KeyRound, LogOut, Plug, ReceiptText, RefreshCw, ShieldCheck, Wallet } from 'lucide-react'
import { Ambient, TopProgress, useCardSpotlight } from '@/ui/ambient'
import { useIsMobile } from '@/lib/media'
import { toast } from 'sonner'
import { clearKey, isKeyRemembered, persistKey, setUserKey, setUserUnauthorized, storedKey, uapi, type Me, type Usage } from './api'
import { Button } from '@/ui/Button'
import { Field, PasswordInput } from '@/ui/Field'
import { Checkbox, Tip } from '@/ui/controls'
import { ThemeButton } from '@/ui/ThemeButton'
import { PageFade } from '@/ui/misc'
import { OverviewTab } from './pages/Overview'
import { RecordsTab, RechargesTab } from './pages/Records'
import { ChildrenTab } from './pages/Children'
import { UsageTab } from './pages/Usage'

const logo = import.meta.env.BASE_URL + 'logo.png'
export type UTab = 'overview' | 'records' | 'recharges' | 'children' | 'usage'
const TAB_ICON: Record<UTab, React.ReactNode> = {
  overview: <Gauge />,
  records: <ReceiptText />,
  recharges: <Wallet />,
  children: <GitBranch />,
  usage: <Plug />,
}

export function UserApp() {
  const qc = useQueryClient()
  const [key, setKey] = useState(() => storedKey())
  const [phase, setPhase] = useState<'checking' | 'out' | 'in'>(() => (storedKey() ? 'checking' : 'out'))

  const signOut = useCallback(
    (expired?: boolean) => {
      clearKey()
      setUserKey('')
      setKey('')
      qc.clear()
      setPhase('out')
      if (expired) toast.warning('登录已失效，请重新输入 API Key')
    },
    [qc],
  )

  useEffect(() => {
    setUserUnauthorized(() => signOut(true))
    return () => setUserUnauthorized(null)
  }, [signOut])

  useEffect(() => {
    if (phase !== 'checking') return
    uapi<Me>('/me', { key })
      .then((me) => {
        qc.setQueryData(['me'], me)
        setUserKey(key)
        setPhase('in')
      })
      .catch((e) => {
        if ((e as { status?: number }).status === 401) clearKey()
        setPhase('out')
      })
  }, [phase, key, qc])

  return (
    <AnimatePresence mode="wait" initial={false}>
      {phase === 'checking' ? (
        <motion.div key="boot" style={{ minHeight: '100vh', display: 'grid', placeItems: 'center' }} exit={{ opacity: 0 }}>
          <span className="spinner" style={{ width: 18, height: 18, color: 'var(--faint)' }} />
        </motion.div>
      ) : phase === 'out' ? (
        <motion.div key="login" initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0, transition: { duration: 0.18 } }}>
          <UserLogin
            initial={key}
            onSuccess={(k, me) => {
              qc.setQueryData(['me'], me)
              setUserKey(k)
              setKey(k)
              setPhase('in')
            }}
          />
        </motion.div>
      ) : (
        <motion.div key="portal" initial={{ opacity: 0 }} animate={{ opacity: 1 }} transition={{ duration: 0.3 }}>
          <Portal apiKey={key} onSignOut={() => signOut(false)} />
        </motion.div>
      )}
    </AnimatePresence>
  )
}

function UserLogin({ initial, onSuccess }: { initial: string; onSuccess: (key: string, me: Me) => void }) {
  const [key, setKey] = useState(initial)
  const [remember, setRemember] = useState(() => isKeyRemembered())
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const shake = useAnimationControls()
  const ref = useRef<HTMLInputElement>(null)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    const k = key.trim()
    if (!k) {
      setError('请输入 API Key')
      ref.current?.focus()
      return
    }
    setBusy(true)
    setError('')
    try {
      const me = await uapi<Me>('/me', { key: k })
      persistKey(k, remember)
      onSuccess(k, me)
    } catch (err) {
      setError((err as Error).message)
      shake.start({ x: [0, -7, 7, -5, 5, -2, 0], transition: { duration: 0.42 } })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login">
      <Ambient variant="hero" />
      <div className="login-corner">
        <ThemeButton />
      </div>
      <motion.div
        initial={{ opacity: 0, y: 14, scale: 0.985 }}
        animate={{ opacity: 1, y: 0, scale: 1 }}
        transition={{ duration: 0.5, ease: [0.16, 1, 0.3, 1] }}
        style={{ width: 'min(400px, 100%)' }}
      >
        <motion.form animate={shake} className="card login-card stack" style={{ gap: 20, width: '100%' }} onSubmit={submit}>
          <div className="stack" style={{ gap: 14 }}>
            <div className="login-avatar">
              <img src={logo} alt="" />
            </div>
            <div>
              <h1 style={{ fontSize: 20 }}>查询余额与用量</h1>
              <p className="muted" style={{ marginTop: 4, fontSize: 13 }}>
                输入你的 API Key，查看余额、消费明细和接入方式
              </p>
            </div>
          </div>
          <Field label="API Key" error={error} htmlFor="user-key">
            <PasswordInput
              ref={ref}
              id="user-key"
              icon={<KeyRound />}
              value={key}
              onChange={(e) => {
                setKey(e.target.value)
                if (error) setError('')
              }}
              invalid={!!error}
              autoFocus
              autoComplete="off"
              spellCheck={false}
              placeholder="sk-..."
              mono
            />
          </Field>
          <label className="remember">
            <Checkbox checked={remember} onChange={setRemember} label="记住" />
            在这台设备上记住
          </label>
          <Button type="submit" variant="primary" size="lg" block loading={busy}>
            查询
          </Button>
          <p className="xs faint row" style={{ gap: 6, alignItems: 'flex-start', lineHeight: 1.55 }}>
            <ShieldCheck size={13} style={{ flex: 'none', marginTop: 2 }} />
            Key 只保存在这个浏览器里。公用电脑请不要勾选「记住」。
          </p>
        </motion.form>
      </motion.div>
    </div>
  )
}

function Portal({ apiKey, onSignOut }: { apiKey: string; onSignOut: () => void }) {
  const qc = useQueryClient()
  const [tab, setTab] = useState<UTab>('overview')
  const me = useQuery({ queryKey: ['me'], queryFn: () => uapi<Me>('/me'), refetchInterval: tab === 'overview' ? 30_000 : false })
  const usage = useQuery({ queryKey: ['usage'], queryFn: () => uapi<Usage>('/usage'), refetchInterval: tab === 'overview' ? 30_000 : false })
  const canSub = !!me.data?.canManageSubKeys

  const mobile = useIsMobile()
  useCardSpotlight()
  const tabs: { id: UTab; label: string; short?: string }[] = [
    { id: 'overview', label: '概览' },
    { id: 'records', label: '消费明细', short: '明细' },
    { id: 'recharges', label: '充值记录', short: '充值' },
    ...(canSub ? [{ id: 'children' as UTab, label: '子卡密', short: '子卡' }] : []),
    { id: 'usage', label: '接入方式', short: '接入' },
  ]

  useEffect(() => {
    if (tab === 'children' && me.data && !canSub) setTab('overview')
  }, [tab, canSub, me.data])

  useEffect(() => {
    document.title = me.data?.name ? `${me.data.name} · Kiro-Go 用户中心` : 'Kiro-Go 用户中心'
  }, [me.data?.name])

  const [spinning, setSpinning] = useState(false)
  const refresh = async () => {
    setSpinning(true)
    await qc.invalidateQueries()
    setTimeout(() => setSpinning(false), 400)
  }

  return (
    <>
      <Ambient />
      <TopProgress />
    <div className="portal-shell">
      <header className="portal-header">
        <div className="portal-header-inner">
          <div className="brand">
            <span className="brand-mark">
              <img src={logo} alt="" />
            </span>
            <span className="brand-text">
              <div className="brand-name">Kiro-Go</div>
              <div className="brand-sub">用户中心</div>
            </span>
          </div>
          {!mobile && (
            <nav className="portal-nav" aria-label="页面">
              {tabs.map((t) => (
                <button key={t.id} type="button" className="portal-tab" data-active={tab === t.id} onClick={() => setTab(t.id)}>
                  {tab === t.id && <motion.span layoutId="portal-pill" className="tab-pill" transition={{ type: 'spring', stiffness: 480, damping: 38 }} />}
                  {t.label}
                </button>
              ))}
            </nav>
          )}
          <div className="row" style={{ marginLeft: 'auto', gap: 4 }}>
            <Tip content="刷新数据">
              <Button variant="ghost" iconOnly onClick={refresh} icon={<RefreshCw style={{ transition: 'transform .5s var(--ease-out)', transform: spinning ? 'rotate(360deg)' : 'none' }} />}>
                刷新
              </Button>
            </Tip>
            <ThemeButton />
            <Tip content="退出">
              <Button variant="ghost" iconOnly icon={<LogOut />} onClick={onSignOut}>
                退出
              </Button>
            </Tip>
          </div>
        </div>
      </header>
      <main className="portal-main">
        <PageFade k={tab}>
          {tab === 'overview' && <OverviewTab me={me.data} usage={usage.data} loading={me.isLoading || usage.isLoading} />}
          {tab === 'records' && <RecordsTab />}
          {tab === 'recharges' && <RechargesTab />}
          {tab === 'children' && canSub && <ChildrenTab />}
          {tab === 'usage' && <UsageTab apiKey={apiKey} />}
        </PageFade>
      </main>
    </div>
      {mobile && (
        <nav className="bottom-nav" aria-label="页面">
          <LayoutGroup id="portal-bottom">
            {tabs.map((t) => (
              <button key={t.id} type="button" className="bottom-nav-item" data-active={tab === t.id} onClick={() => setTab(t.id)}>
                {tab === t.id && <motion.span layoutId="pbn-pill" className="bottom-nav-pill" transition={{ type: 'spring', stiffness: 520, damping: 38 }} />}
                {TAB_ICON[t.id]}
                <span>{t.short || t.label}</span>
              </button>
            ))}
          </LayoutGroup>
        </nav>
      )}
    </>
  )
}
