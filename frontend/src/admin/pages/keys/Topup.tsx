import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { api } from '../../api'
import { Dialog } from '@/ui/Dialog'
import { Button } from '@/ui/Button'
import { Field, Input } from '@/ui/Field'
import { formatNumber } from '@/lib/format'
import { keyName, type NKey } from './util'

const QUICK = [100, 500, 1000, 5000]

export function TopupDialog({ k, onClose, onDone }: { k: NKey | null; onClose: () => void; onDone: () => void }) {
  const [amount, setAmount] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (k) {
      setAmount('')
      setNote('')
    }
  }, [k])

  const n = parseFloat(amount)
  const valid = Number.isFinite(n) && n > 0

  const submit = async () => {
    if (!k) return
    if (!valid) return toast.warning('请输入大于 0 的充值额度')
    setBusy(true)
    try {
      const d = await api<{ balance: number }>(`/api-keys/${encodeURIComponent(k.id)}/topup`, {
        method: 'POST',
        body: { amount: n, note: note.trim() },
      })
      toast.success(`已充值 ${formatNumber(n)} 积分`, { description: `当前余额 ${formatNumber(Math.round(d.balance * 10) / 10)}` })
      onDone()
      onClose()
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={!!k}
      onOpenChange={(o) => !o && onClose()}
      title="充值"
      description={k ? `给「${keyName(k)}」增加积分，会记入充值记录。` : undefined}
      width={440}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            取消
          </Button>
          <Button variant="primary" loading={busy} onClick={submit} disabled={!valid}>
            确认充值
          </Button>
        </>
      }
    >
      {k && (
        <form
          className="stack"
          style={{ gap: 14 }}
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
        >
          <div className="summary-grid" style={{ gridTemplateColumns: 'repeat(2, minmax(0,1fr))' }}>
            <div className="summary-item">
              <div className="l">当前余额</div>
              <div className="v">{k.granted > 0 ? formatNumber(Math.round(k.balance * 10) / 10) : '不限额'}</div>
            </div>
            <div className="summary-item">
              <div className="l">充值后</div>
              <div className="v t-ok">{k.granted > 0 && valid ? formatNumber(Math.round((k.balance + n) * 10) / 10) : '—'}</div>
            </div>
          </div>
          {k.granted <= 0 && (
            <div className="callout callout-warn">
              <span>这是一张不限额的卡。充值后会变成限额卡，额度等于充值数，已用 {formatNumber(Math.round(k.used * 10) / 10)} 会从中扣除。</span>
            </div>
          )}
          <Field label="充值积分">
            <Input data-autofocus value={amount} onChange={(e) => setAmount(e.target.value.replace(/[^\d.]/g, ''))} inputMode="decimal" placeholder="例如 1000" className="num" />
          </Field>
          <div className="row row-wrap" style={{ gap: 6, marginTop: -6 }}>
            {QUICK.map((q) => (
              <Button key={q} size="sm" variant="secondary" onClick={() => setAmount(String(q))}>
                {formatNumber(q)}
              </Button>
            ))}
          </div>
          <Field label="备注" hint="可选，例如订单号">
            <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder="备注" />
          </Field>
        </form>
      )}
    </Dialog>
  )
}
