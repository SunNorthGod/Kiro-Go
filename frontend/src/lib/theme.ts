import { useCallback, useEffect, useSyncExternalStore } from 'react'

// Shared with the old panel and the user portal, so a user's choice survives the rewrite.
const STORAGE_KEY = 'kiro_theme'
export type ThemePref = 'system' | 'light' | 'dark'
const ORDER: ThemePref[] = ['system', 'light', 'dark']

const media = typeof window !== 'undefined' ? window.matchMedia('(prefers-color-scheme: dark)') : null
const listeners = new Set<() => void>()

function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return ORDER.includes(v as ThemePref) ? (v as ThemePref) : 'system'
  } catch {
    return 'system'
  }
}

let pref: ThemePref = readPref()

function resolve(p: ThemePref): 'light' | 'dark' {
  if (p === 'system') return media?.matches ? 'dark' : 'light'
  return p
}

function apply() {
  document.documentElement.classList.toggle('dark', resolve(pref) === 'dark')
}

function emit() {
  listeners.forEach((l) => l())
}

media?.addEventListener('change', () => {
  if (pref === 'system') {
    apply()
    emit()
  }
})

function subscribe(l: () => void) {
  listeners.add(l)
  return () => listeners.delete(l)
}

type ViewTransitionDoc = Document & {
  startViewTransition?: (cb: () => void) => { ready: Promise<void> }
}

/**
 * Switch the theme. When the browser supports view transitions, the new theme is
 * revealed as a circle growing out of the element that was clicked.
 */
export function setThemePref(next: ThemePref, origin?: { x: number; y: number }) {
  const before = resolve(pref)
  pref = next
  try {
    localStorage.setItem(STORAGE_KEY, next)
  } catch {
    /* private mode */
  }
  const after = resolve(next)
  const doc = document as ViewTransitionDoc
  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches
  if (before !== after && doc.startViewTransition && origin && !reduce) {
    const r = Math.hypot(Math.max(origin.x, innerWidth - origin.x), Math.max(origin.y, innerHeight - origin.y))
    document.documentElement.classList.add('vt-theme')
    const t = doc.startViewTransition(() => {
      apply()
    })
    t.ready
      .then(() => {
        document.documentElement.animate(
          { clipPath: [`circle(0px at ${origin.x}px ${origin.y}px)`, `circle(${r}px at ${origin.x}px ${origin.y}px)`] },
          { duration: 520, easing: 'cubic-bezier(0.16, 1, 0.3, 1)', pseudoElement: '::view-transition-new(root)' },
        ).finished.finally(() => document.documentElement.classList.remove('vt-theme'))
      })
      .catch(() => document.documentElement.classList.remove('vt-theme'))
  } else {
    apply()
  }
  emit()
}

export function useTheme() {
  const current = useSyncExternalStore(subscribe, () => pref)
  const resolved = resolve(current)
  const cycle = useCallback((origin?: { x: number; y: number }) => {
    const next = ORDER[(ORDER.indexOf(pref) + 1) % ORDER.length]
    setThemePref(next, origin)
  }, [])
  useEffect(() => {
    apply()
    const id = requestAnimationFrame(() => document.documentElement.classList.add('theme-ready'))
    return () => cancelAnimationFrame(id)
  }, [])
  return { pref: current, resolved, cycle, set: setThemePref }
}

export const THEME_LABEL: Record<ThemePref, string> = {
  system: '跟随系统',
  light: '浅色',
  dark: '深色',
}
