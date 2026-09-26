// One-shot navigation intents from the command palette to a page, e.g. "open the
// detail of key X" or "open the create form". A page consumes the intent for its
// tab on mount (or when a new one arrives) and clears it.

export type Intent =
  | { tab: 'accounts'; action: 'add' }
  | { tab: 'accounts'; action: 'detail'; id: string }
  | { tab: 'keys'; action: 'create' }
  | { tab: 'keys'; action: 'detail'; id: string }

let pending: Intent | null = null
const listeners = new Set<() => void>()

export function pushIntent(i: Intent) {
  pending = i
  listeners.forEach((l) => l())
}

export function takeIntent(tab: Intent['tab']): Intent | null {
  if (pending && pending.tab === tab) {
    const i = pending
    pending = null
    return i
  }
  return null
}

export function onIntent(fn: () => void) {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}
