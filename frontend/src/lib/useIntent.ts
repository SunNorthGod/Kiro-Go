import { useEffect, useRef } from 'react'
import { onIntent, takeIntent, type Intent } from './intent'

/** Run `handle` for every palette intent addressed to this tab. */
export function useIntent(tab: Intent['tab'], handle: (i: Intent) => void) {
  const ref = useRef(handle)
  ref.current = handle
  useEffect(() => {
    const run = () => {
      const i = takeIntent(tab)
      if (i) ref.current(i)
    }
    run()
    return onIntent(run)
  }, [tab])
}
