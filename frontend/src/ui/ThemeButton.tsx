import { Monitor, Moon, Sun } from 'lucide-react'
import { AnimatePresence, motion } from 'motion/react'
import { THEME_LABEL, useTheme } from '@/lib/theme'
import { Tip } from './controls'

export function ThemeButton({ size = 'md' }: { size?: 'sm' | 'md' }) {
  const { pref, cycle } = useTheme()
  const Icon = pref === 'system' ? Monitor : pref === 'light' ? Sun : Moon
  return (
    <Tip content={`主题：${THEME_LABEL[pref]}（点击切换）`}>
      <button
        type="button"
        className={`btn btn-ghost btn-icon${size === 'sm' ? ' btn-sm' : ''}`}
        aria-label="切换主题"
        onClick={(e) => {
          const r = e.currentTarget.getBoundingClientRect()
          cycle({ x: r.left + r.width / 2, y: r.top + r.height / 2 })
        }}
      >
        <AnimatePresence mode="wait" initial={false}>
          <motion.span
            key={pref}
            initial={{ rotate: -60, opacity: 0, scale: 0.6 }}
            animate={{ rotate: 0, opacity: 1, scale: 1 }}
            exit={{ rotate: 60, opacity: 0, scale: 0.6 }}
            transition={{ duration: 0.22, ease: [0.16, 1, 0.3, 1] }}
            style={{ display: 'grid', placeItems: 'center' }}
          >
            <Icon />
          </motion.span>
        </AnimatePresence>
      </button>
    </Tip>
  )
}
