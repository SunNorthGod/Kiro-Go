import type { ApiKey } from '../../api'

export interface NKey {
  id: string
  name: string
  key: string
  keyMasked: string
  rpm: number
  enabled: boolean
  migrated: boolean
  granted: number
  used: number
  balance: number
  tokenLimit: number
  tokensUsed: number
  requestsCount: number
  expiresAt: number
  maxConcurrency: number | null
  maxRPM: number | null
  boundAccountIds: string[]
  parentKeyId: string
  createdAt: number
  lastUsedAt: number
}

export function normalizeKey(k: ApiKey): NKey {
  const granted = k.creditsGranted != null ? k.creditsGranted : k.creditLimit || 0
  const used = k.creditsUsed || 0
  return {
    id: k.id,
    name: k.name || '',
    key: k.key || '',
    keyMasked: k.keyMasked || '',
    rpm: k.rpm || 0,
    enabled: !!k.enabled,
    migrated: !!k.migrated,
    granted,
    used,
    balance: k.balance != null ? k.balance : granted - used,
    tokenLimit: k.tokenLimit || 0,
    tokensUsed: k.tokensUsed || 0,
    requestsCount: k.requestsCount || 0,
    expiresAt: k.expiresAt || 0,
    maxConcurrency: k.maxConcurrency ?? null,
    maxRPM: k.maxRPM ?? null,
    boundAccountIds: Array.isArray(k.boundAccountIds) ? k.boundAccountIds : [],
    parentKeyId: k.parentKeyId || '',
    createdAt: k.createdAt || 0,
    lastUsedAt: k.lastUsedAt || 0,
  }
}

export const keyExpired = (k: NKey) => k.expiresAt > 0 && k.expiresAt < Date.now() / 1000

export function keyDot(k: NKey): 'ok' | 'warn' | 'bad' {
  if (keyExpired(k)) return 'bad'
  if (!k.enabled) return 'warn'
  if (k.granted > 0 && k.balance < 0) return 'warn'
  return 'ok'
}

/** Tri-state limit: null = system default, 0 = unlimited, N. */
export const limitLabel = (v: number | null) => (v == null ? '默认' : v === 0 ? '不限' : String(v))

export const keyName = (k: { name?: string }) => k.name || '未命名'
