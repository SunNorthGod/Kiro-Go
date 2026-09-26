import { useRef, useState } from 'react'
import { motion, useAnimationControls } from 'motion/react'
import { ArrowRight, Lock } from 'lucide-react'
import { api } from './api'
import { isRemembered, rememberedPassword, storePassword } from './auth'
import { Button } from '@/ui/Button'
import { Field, PasswordInput } from '@/ui/Field'
import { Checkbox } from '@/ui/controls'
import { ThemeButton } from '@/ui/ThemeButton'
import { Ambient } from '@/ui/ambient'

const logo = import.meta.env.BASE_URL + 'logo.png'

export function Login({ onSuccess }: { onSuccess: (password: string) => void }) {
  const [pwd, setPwd] = useState(() => rememberedPassword())
  const [remember, setRemember] = useState(() => isRemembered())
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const shake = useAnimationControls()
  const inputRef = useRef<HTMLInputElement>(null)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    if (!pwd) {
      setError('请输入管理员密码')
      inputRef.current?.focus()
      return
    }
    setBusy(true)
    setError('')
    try {
      await api('/status', { auth: pwd })
      storePassword(pwd, remember)
      onSuccess(pwd)
    } catch (err) {
      const status = (err as { status?: number }).status
      setError(status === 401 ? '密码不正确' : (err as Error).message)
      shake.start({ x: [0, -7, 7, -5, 5, -2, 0], transition: { duration: 0.42 } })
      inputRef.current?.select()
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
        style={{ width: 'min(380px, 100%)' }}
      >
        <motion.form animate={shake} className="card login-card stack" style={{ gap: 20 }} onSubmit={submit}>
          <div className="stack" style={{ gap: 14 }}>
            <div className="login-avatar">
              <img src={logo} alt="" />
            </div>
            <div>
              <h1 style={{ fontSize: 20 }}>登录 Kiro-Go</h1>
              <p className="muted" style={{ marginTop: 4, fontSize: 13 }}>
                管理后台，输入管理员密码继续
              </p>
            </div>
          </div>
          <Field label="管理员密码" error={error} htmlFor="admin-pwd">
            <PasswordInput
              ref={inputRef}
              id="admin-pwd"
              icon={<Lock />}
              value={pwd}
              onChange={(e) => {
                setPwd(e.target.value)
                if (error) setError('')
              }}
              invalid={!!error}
              autoComplete="current-password"
              autoFocus
              placeholder="••••••••"
            />
          </Field>
          <label className="remember">
            <Checkbox checked={remember} onChange={setRemember} label="记住密码" />
            在这台设备上记住密码
          </label>
          <Button type="submit" variant="primary" size="lg" block loading={busy} icon={<ArrowRight />} style={{ flexDirection: 'row-reverse' }}>
            登录
          </Button>
        </motion.form>
        <p className="xs faint" style={{ textAlign: 'center', marginTop: 16 }}>
          客户查询余额请前往{' '}
          <a href="/user" className="text-2" style={{ textDecoration: 'underline', textUnderlineOffset: 3 }}>
            用户中心
          </a>
        </p>
      </motion.div>
    </div>
  )
}
