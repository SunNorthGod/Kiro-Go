import { useEffect, useState } from 'react'
import { Copy, Download, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { api, type Account, type ExportData } from '../../api'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Checkbox } from '@/ui/controls'
import { copyText } from '@/lib/clipboard'
import { authLabel, displayName, subscription } from './util'

export function ExportDialog({ open, onOpenChange, accounts }: { open: boolean; onOpenChange: (o: boolean) => void; accounts: Account[] }) {
  const [sel, setSel] = useState<Set<string>>(new Set())
  const [busy, setBusy] = useState<'copy' | 'download' | null>(null)

  useEffect(() => {
    if (open) setSel(new Set(accounts.map((a) => a.id)))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const all = sel.size === accounts.length && accounts.length > 0
  const fetchData = async () => {
    if (!sel.size) {
      toast.warning('请至少选择一个账号')
      return null
    }
    return api<ExportData>('/export', { method: 'POST', body: { ids: [...sel] } })
  }

  const copy = async () => {
    setBusy('copy')
    try {
      const d = await fetchData()
      if (!d) return
      await copyText(JSON.stringify(d, null, 2))
      toast.success(`已复制 ${d.accounts.length} 个账号的凭证`)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(null)
    }
  }

  const download = async () => {
    setBusy('download')
    try {
      const d = await fetchData()
      if (!d) return
      const blob = new Blob([JSON.stringify(d, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      const now = new Date()
      link.href = url
      link.download = `kiro-accounts-${now.getFullYear()}${String(now.getMonth() + 1).padStart(2, '0')}${String(now.getDate()).padStart(2, '0')}.json`
      link.click()
      setTimeout(() => URL.revokeObjectURL(url), 1000)
      toast.success(`已导出 ${d.accounts.length} 个账号`)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(null)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="导出账号"
      description="导出为 Kiro Account Manager 格式，可以在其他实例或本页的「凭证 JSON」导入。"
      width={560}
      footer={
        <>
          <span className="xs muted" style={{ marginRight: 'auto' }}>
            已选 {sel.size} / {accounts.length}
          </span>
          <Button icon={<Copy />} onClick={copy} loading={busy === 'copy'} disabled={!!busy}>
            复制 JSON
          </Button>
          <Button variant="primary" icon={<Download />} onClick={download} loading={busy === 'download'} disabled={!!busy}>
            下载文件
          </Button>
        </>
      }
    >
      <div className="stack" style={{ gap: 12 }}>
        <div className="callout callout-warn">
          <TriangleAlert />
          导出文件包含 refreshToken、clientSecret 等完整凭证，请妥善保管。
        </div>
        <div className="export-list">
          <label className="export-row" style={{ background: 'var(--surface-2)' }}>
            <Checkbox checked={all ? true : sel.size ? 'indeterminate' : false} onChange={(v) => setSel(v ? new Set(accounts.map((a) => a.id)) : new Set())} label="全选" />
            <span className="strong xs">全选</span>
          </label>
          {accounts.map((a) => (
            <label key={a.id} className="export-row">
              <Checkbox
                checked={sel.has(a.id)}
                onChange={(v) =>
                  setSel((s) => {
                    const n = new Set(s)
                    if (v) n.add(a.id)
                    else n.delete(a.id)
                    return n
                  })
                }
                label={displayName(a)}
              />
              <span className="grow" style={{ minWidth: 0 }}>
                <div className="ellipsis" style={{ fontSize: 13, color: 'var(--text)' }}>
                  {displayName(a)}
                </div>
                <div className="xs muted">
                  {authLabel(a)} · {subscription(a.subscriptionType).label}
                </div>
              </span>
            </label>
          ))}
        </div>
      </div>
    </Dialog>
  )
}
