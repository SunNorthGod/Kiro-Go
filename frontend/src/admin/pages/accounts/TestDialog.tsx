import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { AnimatePresence, motion } from 'motion/react'
import { Play } from 'lucide-react'
import { api, type Account, type CachedModel } from '../../api'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Field, Input } from '@/ui/Field'
import { Select } from '@/ui/controls'
import { Skeleton } from '@/ui/misc'
import { authLabel, displayName } from './util'

interface Line {
  t: string
  msg: string
  kind: 'info' | 'ok' | 'err'
}

export function TestDialog({ account, onClose }: { account: Account | null; onClose: () => void }) {
  const open = !!account
  const id = account?.id
  const models = useQuery({
    queryKey: ['account-models', id],
    queryFn: () => api<{ models: CachedModel[] }>(`/accounts/${encodeURIComponent(id!)}/models/cached`).then((d) => d.models || []),
    enabled: open,
  })
  const ids = (models.data || []).map((m) => m.modelId).filter(Boolean).sort()
  const [model, setModel] = useState('')
  const [lines, setLines] = useState<Line[]>([])
  const [running, setRunning] = useState(false)
  const logRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) {
      setLines([])
      setModel('')
    }
  }, [open])
  useEffect(() => {
    if (!model && ids.length) setModel(ids.includes('claude-sonnet-4') ? 'claude-sonnet-4' : ids[0])
  }, [ids, model])
  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight, behavior: 'smooth' })
  }, [lines])

  const log = (msg: string, kind: Line['kind']) =>
    setLines((l) => [...l, { t: new Date().toLocaleTimeString('zh-CN', { hour12: false }), msg, kind }].slice(-100))

  const run = async () => {
    if (!account || running) return
    const m = model.trim() || 'claude-sonnet-4'
    setRunning(true)
    log(`发送测试请求 · 模型 ${m} · ${account.proxyURL ? '独立代理' : '全局代理'}`, 'info')
    const start = performance.now()
    try {
      const d = await api<{ reply: string }>(`/accounts/${encodeURIComponent(account.id)}/test`, { method: 'POST', body: { model: m } })
      const s = ((performance.now() - start) / 1000).toFixed(1)
      log(`成功 · ${s}s · 回复：${d.reply || '（空）'}`, 'ok')
    } catch (e) {
      const s = ((performance.now() - start) / 1000).toFixed(1)
      log(`失败 · ${s}s · ${(e as Error).message}`, 'err')
    } finally {
      setRunning(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title="测试账号"
      description={account ? `${displayName(account)} · ${authLabel(account)}` : undefined}
      width={560}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            关闭
          </Button>
          <Button variant="primary" icon={<Play />} loading={running} onClick={run} disabled={models.isLoading}>
            发送测试
          </Button>
        </>
      }
    >
      <div className="stack" style={{ gap: 14 }}>
        <Field label="测试模型" hint={models.isError ? '模型列表不可用，可以手动填写' : `已缓存 ${ids.length} 个模型；会发送一句 "say ok"，最多 5 个 token`}>
          {models.isLoading ? (
            <Skeleton h={36} />
          ) : ids.length ? (
            <Select value={model || ids[0]} onChange={setModel} options={ids.map((m) => ({ value: m, label: <span className="mono xs">{m}</span> }))} />
          ) : (
            <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="claude-sonnet-4" mono onKeyDown={(e) => e.key === 'Enter' && run()} />
          )}
        </Field>
        <div className="log-console" ref={logRef}>
          {!lines.length ? (
            <span className="faint">还没有运行测试</span>
          ) : (
            <AnimatePresence initial={false}>
              {lines.map((l, i) => (
                <motion.div key={i} className={`log-line ${l.kind}`} initial={{ opacity: 0, x: -6 }} animate={{ opacity: 1, x: 0 }} transition={{ duration: 0.2 }}>
                  <time>{l.t}</time>
                  <span style={{ wordBreak: 'break-word' }}>{l.msg}</span>
                </motion.div>
              ))}
            </AnimatePresence>
          )}
        </div>
      </div>
    </Dialog>
  )
}
