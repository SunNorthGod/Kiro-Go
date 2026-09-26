import { useSyncExternalStore } from 'react'

// Counts user-initiated requests (anything that is not a GET) so the top progress
// bar can show that a save/delete is in flight. Background polling never touches it.
let inflight = 0
const listeners = new Set<() => void>()
const emit = () => listeners.forEach((l) => l())

export function beginActivity(): () => void {
  inflight++
  emit()
  let done = false
  return () => {
    if (done) return
    done = true
    inflight = Math.max(0, inflight - 1)
    emit()
  }
}

export function useActivity(): number {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l)
      return () => listeners.delete(l)
    },
    () => inflight,
  )
}
