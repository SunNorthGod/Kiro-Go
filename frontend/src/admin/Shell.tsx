import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { LayoutGroup, motion } from 'motion/react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { ArrowUpRight, Gauge, KeyRound, LogOut, Menu as MenuIcon, ScrollText, Search, SlidersHorizontal, Users } from 'lucide-react'
import { useAccounts, useApiKeys, useStatus, type Tab } from './queries'
import { Button } from '@/ui/Button'
import { Dot, Tip } from '@/ui/controls'
import { ThemeButton } from '@/ui/ThemeButton'
import { PageFade } from '@/ui/misc'
import { Ambient, TopProgress, useCardSpotlight } from '@/ui/ambient'
import { formatDuration } from '@/lib/format'
import { confirm } from '@/lib/confirm'
import { useIsMobile } from '@/lib/media'
import { CommandPalette } from './CommandPalette'
import { OverviewPage } from './pages/Overview'
import { AccountsPage } from './pages/Accounts'
import { KeysPage } from './pages/Keys'
import { LogsPage } from './pages/Logs'
import { SettingsPage } from './pages/Settings'

const logo = import.meta.env.BASE_URL + 'logo.png'
const TABS: Tab[] = ['overview', 'accounts', 'keys', 'logs', 'settings']
const TITLES: Record<Tab, string> = {
  overview: '概览',
  accounts: '账号',
  keys: '卡密',
  logs: '请求日志',
  settings: '设置',
}
const ICONS: Record<Tab, ReactNode> = {
  overview: <Gauge />,
  accounts: <Users />,
  keys: <KeyRound />,
  logs: <ScrollText />,
  settings: <SlidersHorizontal />,
}
const SHORT: Record<Tab, string> = { overview: '概览', accounts: '账号', keys: '卡密', logs: '日志', settings: '设置' }
const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)

function readTab(): Tab {
  const h = location.hash.replace(/^#\/?/, '') as Tab
  if (TABS.includes(h)) return h
  try {
    const s = localStorage.getItem('kiro_tab') as Tab
    if (TABS.includes(s)) return s
  } catch {
    /* ignore */
  }
  return 'overview'
}

/* Pages portal their actions into the top bar. */
const TopbarSlot = createContext<HTMLElement | null>(null)
export function TopbarActions({ children }: { children: ReactNode }) {
  const el = useContext(TopbarSlot)
  return el ? createPortal(children, el) : null
}

const TabContext = createContext<{ tab: Tab; go: (t: Tab) => void }>({ tab: 'overview', go: () => {} })
export const useTab = () => useContext(TabContext)

const typing = (el: EventTarget | null) => {
  const t = el as HTMLElement | null
  return !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)
}

export function Shell({ onSignOut }: { onSignOut: () => void }) {
  const [tab, setTab] = useState<Tab>(readTab)
  const [slot, setSlot] = useState<HTMLElement | null>(null)
  const [drawer, setDrawer] = useState(false)
  const [palette, setPalette] = useState(false)
  const mobile = useIsMobile()
  useCardSpotlight()

  const go = useCallback((t: Tab) => {
    setTab(t)
    setDrawer(false)
    if (location.hash !== '#' + t) history.pushState(null, '', '#' + t)
    try {
      localStorage.setItem('kiro_tab', t)
    } catch {
      /* ignore */
    }
    window.scrollTo({ top: 0 })
  }, [])

  useEffect(() => {
    const on = () => setTab(readTab())
    window.addEventListener('popstate', on)
    window.addEventListener('hashchange', on)
    if (!location.hash) history.replaceState(null, '', '#' + readTab())
    return () => {
      window.removeEventListener('popstate', on)
      window.removeEventListener('hashchange', on)
    }
  }, [])

  // Ctrl/⌘+K opens the palette; "/" focuses the page's search box.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPalette((p) => !p)
        return
      }
      if (e.key === '/' && !typing(e.target) && !document.querySelector('[role="dialog"]')) {
        const input = document.querySelector<HTMLInputElement>('.content .search input')
        if (input) {
          e.preventDefault()
          input.focus()
          input.select()
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  useEffect(() => {
    document.title = `${TITLES[tab]} · Kiro-Go`
  }, [tab])

  const signOut = useCallback(async () => {
    const ok = await confirm({ title: '退出登录？', description: '退出后需要重新输入管理员密码。', confirmText: '退出' })
    if (ok) onSignOut()
  }, [onSignOut])

  return (
    <TabContext.Provider value={{ tab, go }}>
      <TopbarSlot.Provider value={slot}>
        <Ambient />
        <TopProgress />
        <div className="app">
          <aside className="sidebar-desktop">
            <LayoutGroup id="sidebar-desktop">
              <Sidebar tab={tab} go={go} onSignOut={signOut} />
            </LayoutGroup>
          </aside>
          <div className="main">
            <header className="topbar">
              <Button variant="ghost" iconOnly icon={<MenuIcon />} className="mobile-only" onClick={() => setDrawer(true)}>
                菜单
              </Button>
              <motion.h1 key={tab} className="topbar-title" initial={{ opacity: 0, y: 4 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
                {TITLES[tab]}
              </motion.h1>
              {mobile ? (
                <Button variant="ghost" iconOnly icon={<Search />} onClick={() => setPalette(true)}>
                  搜索
                </Button>
              ) : (
                <button type="button" className="palette-trigger" onClick={() => setPalette(true)}>
                  <Search />
                  <span>搜索或跳转</span>
                  <span className="kbd">{isMac ? '⌘' : 'Ctrl'} K</span>
                </button>
              )}
              <div className="topbar-actions" ref={setSlot} />
            </header>
            <main className="content">
              <PageFade k={tab}>
                <Page tab={tab} />
              </PageFade>
            </main>
          </div>
        </div>

        {mobile && (
          <nav className="bottom-nav" aria-label="主导航">
            <LayoutGroup id="bottom-nav">
              {TABS.map((t) => (
                <button key={t} type="button" className="bottom-nav-item" data-active={tab === t} onClick={() => go(t)}>
                  {tab === t && <motion.span layoutId="bn-pill" className="bottom-nav-pill" transition={{ type: 'spring', stiffness: 520, damping: 38 }} />}
                  {ICONS[t]}
                  <span>{SHORT[t]}</span>
                </button>
              ))}
            </LayoutGroup>
          </nav>
        )}

        <DialogPrimitive.Root open={drawer} onOpenChange={setDrawer}>
          <DialogPrimitive.Portal>
            <DialogPrimitive.Overlay className="dialog-overlay" />
            <DialogPrimitive.Content className="drawer" aria-describedby={undefined}>
              <DialogPrimitive.Title className="sr-only">导航</DialogPrimitive.Title>
              <LayoutGroup id="sidebar-drawer">
                <Sidebar tab={tab} go={go} onSignOut={signOut} />
              </LayoutGroup>
            </DialogPrimitive.Content>
          </DialogPrimitive.Portal>
        </DialogPrimitive.Root>
        <CommandPalette open={palette} onOpenChange={setPalette} go={go} onSignOut={signOut} />
      </TopbarSlot.Provider>
    </TabContext.Provider>
  )
}

function Page({ tab }: { tab: Tab }) {
  switch (tab) {
    case 'overview':
      return <OverviewPage />
    case 'accounts':
      return <AccountsPage />
    case 'keys':
      return <KeysPage />
    case 'logs':
      return <LogsPage />
    case 'settings':
      return <SettingsPage />
  }
}

function Sidebar({ tab, go, onSignOut }: { tab: Tab; go: (t: Tab) => void; onSignOut: () => void }) {
  const accounts = useAccounts(tab === 'accounts')
  const keys = useApiKeys(tab === 'keys')
  const status = useStatus()

  const items: { id: Tab; count?: number }[] = [
    { id: 'overview' },
    { id: 'accounts', count: accounts.data?.length },
    { id: 'keys', count: keys.data?.length },
    { id: 'logs' },
  ]
  const online = status.isSuccess && !status.isError

  return (
    <div className="sidebar">
      <div className="brand">
        <span className="brand-mark">
          <img src={logo} alt="" />
        </span>
        <span className="brand-text">
          <div className="brand-name">Kiro-Go</div>
          <div className="brand-sub">管理后台</div>
        </span>
      </div>

      <div className="nav-group-label">工作台</div>
      <nav className="nav">
        {items.map((it) => (
          <NavItem key={it.id} active={tab === it.id} onClick={() => go(it.id)} icon={ICONS[it.id]} label={TITLES[it.id]} count={it.count} />
        ))}
      </nav>
      <div className="nav-group-label">系统</div>
      <nav className="nav">
        <NavItem active={tab === 'settings'} onClick={() => go('settings')} icon={ICONS.settings} label="设置" />
        <a className="nav-item" href="/user" target="_blank" rel="noopener">
          <ArrowUpRight />
          用户中心
        </a>
      </nav>

      <div className="sidebar-foot">
        <div className="status-card">
          <Dot tone={status.isLoading ? 'off' : online ? 'ok' : 'bad'} live={online} />
          <span>
            <b>{status.isLoading ? '连接中' : online ? '运行中' : '无法连接'}</b>
            {online && status.data?.uptime != null && <span className="xs"> · {formatDuration(status.data.uptime)}</span>}
          </span>
          {status.data?.version && <span className="ver">v{status.data.version}</span>}
        </div>
        <div className="sidebar-actions">
          <ThemeButton />
          <span className="grow" />
          <Tip content="退出登录">
            <Button variant="ghost" iconOnly icon={<LogOut />} onClick={onSignOut}>
              退出登录
            </Button>
          </Tip>
        </div>
      </div>
    </div>
  )
}

function NavItem({ active, onClick, icon, label, count }: { active: boolean; onClick: () => void; icon: ReactNode; label: string; count?: number }) {
  return (
    <button type="button" className="nav-item" data-active={active} onClick={onClick}>
      {active && <motion.span layoutId="nav-pill" className="nav-pill" transition={{ type: 'spring', stiffness: 480, damping: 38 }} />}
      {icon}
      {label}
      {count != null && <span className="nav-count">{count}</span>}
    </button>
  )
}
