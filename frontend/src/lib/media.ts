import { useSyncExternalStore } from 'react'

/** Subscribes to a CSS media query. */
export function useMedia(query: string): boolean {
  return useSyncExternalStore(
    (cb) => {
      const mq = window.matchMedia(query)
      mq.addEventListener('change', cb)
      return () => mq.removeEventListener('change', cb)
    },
    () => window.matchMedia(query).matches,
  )
}

/** Phone layout: card lists, bottom tab bar, sheets. */
export const MOBILE_QUERY = '(max-width: 760px)'
export const useIsMobile = () => useMedia(MOBILE_QUERY)
