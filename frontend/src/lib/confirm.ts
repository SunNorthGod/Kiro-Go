import { useSyncExternalStore } from 'react'

// Promise-based confirm dialog: `if (!(await confirm({...}))) return`.
export interface ConfirmOptions {
  title: string
  description?: string
  confirmText?: string
  cancelText?: string
  danger?: boolean
}

interface ConfirmState extends ConfirmOptions {
  open: boolean
  resolve?: (ok: boolean) => void
}

let state: ConfirmState = { open: false, title: '' }
const listeners = new Set<() => void>()
const emit = () => listeners.forEach((l) => l())

export function confirm(options: ConfirmOptions): Promise<boolean> {
  state.resolve?.(false)
  return new Promise((resolve) => {
    state = { ...options, open: true, resolve }
    emit()
  })
}

export function settleConfirm(ok: boolean) {
  state.resolve?.(ok)
  state = { ...state, open: false, resolve: undefined }
  emit()
}

export function useConfirmState() {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l)
      return () => listeners.delete(l)
    },
    () => state,
  )
}
