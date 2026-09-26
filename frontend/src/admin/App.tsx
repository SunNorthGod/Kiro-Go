import { useCallback, useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, motion } from 'motion/react'
import { api, setAdminPassword, setUnauthorizedHandler } from './api'
import { clearPassword, initialPassword } from './auth'
import { Login } from './Login'
import { Shell } from './Shell'
import { toast } from 'sonner'

type Phase = 'checking' | 'out' | 'in'

export function App() {
  const qc = useQueryClient()
  const [phase, setPhase] = useState<Phase>(() => (initialPassword() ? 'checking' : 'out'))

  const signOut = useCallback(
    (expired?: boolean) => {
      clearPassword()
      setAdminPassword('')
      qc.clear()
      setPhase('out')
      if (expired) toast.warning('登录已失效，请重新输入密码')
    },
    [qc],
  )

  useEffect(() => {
    setUnauthorizedHandler(() => signOut(true))
    return () => setUnauthorizedHandler(null)
  }, [signOut])

  // Verify a stored password once on load.
  useEffect(() => {
    if (phase !== 'checking') return
    const p = initialPassword()
    api('/status', { auth: p })
      .then(() => {
        setAdminPassword(p)
        setPhase('in')
      })
      .catch(() => {
        clearPassword()
        setPhase('out')
      })
  }, [phase])

  return (
    <AnimatePresence mode="wait" initial={false}>
      {phase === 'checking' ? (
        <motion.div key="boot" style={{ minHeight: '100vh', display: 'grid', placeItems: 'center' }} exit={{ opacity: 0 }}>
          <span className="spinner" style={{ width: 18, height: 18, color: 'var(--faint)' }} />
        </motion.div>
      ) : phase === 'out' ? (
        <motion.div key="login" initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0, transition: { duration: 0.18 } }}>
          <Login
            onSuccess={(p) => {
              setAdminPassword(p)
              setPhase('in')
            }}
          />
        </motion.div>
      ) : (
        <motion.div key="app" initial={{ opacity: 0 }} animate={{ opacity: 1 }} transition={{ duration: 0.3 }}>
          <Shell onSignOut={() => signOut(false)} />
        </motion.div>
      )}
    </AnimatePresence>
  )
}
