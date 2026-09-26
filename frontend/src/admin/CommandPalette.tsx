import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { AnimatePresence, motion } from 'motion/react'
import {
  ArrowUpRight,
  CornerDownLeft,
  Gauge,
  KeyRound,
  LogOut,
  Moon,
  Plus,
  ScrollText,
  Search,
  SlidersHorizontal,
  UserRound,
  Users,
} from 'lucide-react'
import { useAccounts, useApiKeys, type Tab } from './queries'
import { pushIntent } from '@/lib/intent'
import { useTheme } from '@/lib/theme'
import { displayName } from './pages/accounts/util'

interface Item {
  id: string
  group: string
  label: string
  hint?: string
  icon: ReactNode
  keywords?: string
  run: () => void
}

export function CommandPalette({
  open,
  onOpenChange,
  go,
  onSignOut,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  go: (t: Tab) => void
  onSignOut: () => void
}) {
  const [q, setQ] = useState('')
  const [active, setActive] = useState(0)
  const listRef = useRef<HTMLDivElement>(null)
  const accounts = useAccounts()
  const keys = useApiKeys()
  const { cycle } = useTheme()

  useEffect(() => {
    if (open) {
      setQ('')
      setActive(0)
    }
  }, [open])

  const items = useMemo<Item[]>(() => {
    const close = (fn: () => void) => () => {
      onOpenChange(false)
      // Let the palette's exit animation start before the page reacts.
      requestAnimationFrame(fn)
    }
    const nav: Item[] = [
      { id: 'p-ov', group: '页面', label: '概览', icon: <Gauge />, keywords: 'overview gailan', run: close(() => go('overview')) },
      { id: 'p-acc', group: '页面', label: '账号', icon: <Users />, keywords: 'accounts zhanghao', run: close(() => go('accounts')) },
      { id: 'p-key', group: '页面', label: '卡密', icon: <KeyRound />, keywords: 'keys kami api', run: close(() => go('keys')) },
      { id: 'p-log', group: '页面', label: '请求日志', icon: <ScrollText />, keywords: 'logs rizhi', run: close(() => go('logs')) },
      { id: 'p-set', group: '页面', label: '设置', icon: <SlidersHorizontal />, keywords: 'settings shezhi proxy', run: close(() => go('settings')) },
    ]
    const actions: Item[] = [
      {
        id: 'a-key',
        group: '操作',
        label: '新建卡密',
        icon: <Plus />,
        keywords: 'create key xinjian',
        run: close(() => {
          pushIntent({ tab: 'keys', action: 'create' })
          go('keys')
        }),
      },
      {
        id: 'a-acc',
        group: '操作',
        label: '添加账号',
        icon: <Plus />,
        keywords: 'add account tianjia',
        run: close(() => {
          pushIntent({ tab: 'accounts', action: 'add' })
          go('accounts')
        }),
      },
      { id: 'a-theme', group: '操作', label: '切换主题', icon: <Moon />, keywords: 'theme dark light zhuti', run: close(() => cycle()) },
      { id: 'a-user', group: '操作', label: '打开用户中心', icon: <ArrowUpRight />, keywords: 'user portal', run: close(() => window.open('/user', '_blank', 'noopener')) },
      { id: 'a-out', group: '操作', label: '退出登录', icon: <LogOut />, keywords: 'logout tuichu', run: close(onSignOut) },
    ]
    const accs: Item[] = (accounts.data || []).map((a) => ({
      id: 'acc-' + a.id,
      group: '账号',
      label: displayName(a),
      hint: a.enabled ? undefined : '已禁用',
      icon: <UserRound />,
      keywords: `${a.email} ${a.nickname} ${a.userId}`,
      run: close(() => {
        pushIntent({ tab: 'accounts', action: 'detail', id: a.id })
        go('accounts')
      }),
    }))
    const ks: Item[] = (keys.data || []).map((k) => ({
      id: 'key-' + k.id,
      group: '卡密',
      label: k.name || '未命名',
      hint: k.keyMasked,
      icon: <KeyRound />,
      keywords: `${k.name} ${k.key} ${k.keyMasked}`,
      run: close(() => {
        pushIntent({ tab: 'keys', action: 'detail', id: k.id })
        go('keys')
      }),
    }))
    return [...nav, ...actions, ...accs, ...ks]
  }, [accounts.data, keys.data, go, onOpenChange, onSignOut, cycle])

  const filtered = useMemo(() => {
    const s = q.trim().toLowerCase()
    if (!s) return items.filter((i) => i.group === '页面' || i.group === '操作')
    return items
      .filter((i) => `${i.label} ${i.keywords || ''} ${i.hint || ''}`.toLowerCase().includes(s))
      .slice(0, 40)
  }, [items, q])

  useEffect(() => setActive(0), [q])
  useEffect(() => {
    listRef.current?.querySelector<HTMLElement>(`[data-idx="${active}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [active])

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActive((a) => Math.min(filtered.length - 1, a + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((a) => Math.max(0, a - 1))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      filtered[active]?.run()
    }
  }

  let lastGroup = ''
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="dialog-overlay palette-overlay" />
        <DialogPrimitive.Content className="palette" aria-describedby={undefined} onKeyDown={onKey}>
          <DialogPrimitive.Title className="sr-only">命令面板</DialogPrimitive.Title>
          <div className="palette-input">
            <Search />
            <input autoFocus value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索页面、操作、账号或卡密…" spellCheck={false} />
            <span className="kbd">Esc</span>
          </div>
          <div className="palette-list" ref={listRef}>
            {!filtered.length ? (
              <div className="palette-empty">没有找到「{q}」</div>
            ) : (
              filtered.map((it, idx) => {
                const header = it.group !== lastGroup ? it.group : null
                lastGroup = it.group
                return (
                  <div key={it.id}>
                    {header && <div className="palette-group">{header}</div>}
                    <button
                      type="button"
                      data-idx={idx}
                      className="palette-item"
                      data-active={idx === active}
                      onMouseMove={() => idx !== active && setActive(idx)}
                      onClick={it.run}
                    >
                      <AnimatePresence>
                        {idx === active && (
                          <motion.span
                            layoutId="palette-hl"
                            className="palette-hl"
                            transition={{ type: 'spring', stiffness: 700, damping: 45 }}
                          />
                        )}
                      </AnimatePresence>
                      {it.icon}
                      <span className="ellipsis">{it.label}</span>
                      {it.hint && <span className="palette-hint ellipsis">{it.hint}</span>}
                      {idx === active && <CornerDownLeft className="palette-enter" />}
                    </button>
                  </div>
                )
              })
            )}
          </div>
          <div className="palette-foot">
            <span>
              <span className="kbd">↑</span> <span className="kbd">↓</span> 选择
            </span>
            <span>
              <span className="kbd">Enter</span> 打开
            </span>
            <span style={{ marginLeft: 'auto' }}>
              <span className="kbd">Ctrl</span> <span className="kbd">K</span> 随时唤出
            </span>
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
