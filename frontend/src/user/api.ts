// Customer portal API: every call authenticates with the customer's own API key.
import { beginActivity } from '@/lib/activity'

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

let apiKey = ''
let onUnauthorized: (() => void) | null = null
export const setUserKey = (k: string) => {
  apiKey = k
}
export const getUserKey = () => apiKey
export const setUserUnauthorized = (fn: (() => void) | null) => {
  onUnauthorized = fn
}

export async function uapi<T>(path: string, opts: { method?: string; body?: unknown; key?: string } = {}): Promise<T> {
  const headers: Record<string, string> = { Authorization: 'Bearer ' + (opts.key ?? apiKey) }
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  const method = opts.method || 'GET'
  const end = method !== 'GET' ? beginActivity() : null
  let res: Response
  try {
    res = await fetch('/user/api' + path, {
      method,
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      cache: 'no-store',
    })
  } catch {
    end?.()
    throw new ApiError('网络连接失败，请重试', 0)
  }
  end?.()
  let data: unknown = null
  try {
    data = await res.json()
  } catch {
    /* empty body */
  }
  const err = data && typeof data === 'object' && 'error' in data ? String((data as { error?: unknown }).error || '') : ''
  if (res.status === 401) {
    if (opts.key === undefined) onUnauthorized?.()
    throw new ApiError('API Key 无效或已停用', 401)
  }
  if (res.status === 403) throw new ApiError(err === 'API key has expired' ? '这张卡密已过期' : err || '没有权限', 403)
  if (!res.ok) throw new ApiError(err || `请求失败（HTTP ${res.status}）`, res.status)
  return data as T
}

export interface Me {
  name: string
  enabled: boolean
  expired: boolean
  creditsGranted: number
  creditsUsed: number
  balance: number
  tokensUsed: number
  requestsCount: number
  tokenLimit: number
  creditLimit: number
  expiresAt: number
  maxConcurrency: number | null
  maxRPM: number | null
  createdAt: number
  lastUsedAt: number
  isParent: boolean
  canManageSubKeys: boolean
  allocatable: number
}

export interface Usage {
  creditsGranted: number
  creditsUsed: number
  balance: number
  tokensUsed: number
  requestsCount: number
  byModel: {
    model: string
    credits: number
    requests: number
    inputTokens: number
    outputTokens: number
    cacheReadInputTokens: number
    cacheCreationInputTokens: number
  }[]
  totalInputTokens?: number
  totalCacheReadTokens?: number
  cacheHitRate: number | null
}

export interface Paged<T> {
  records: T[]
  total: number
  page: number
  pageSize: number
}

export interface UsageRecord {
  model: string
  inputTokens: number
  outputTokens: number
  credits: number
  createdAt: number
  cacheReadInputTokens: number
}

export interface RechargeRecord {
  amount: number
  note: string
  balanceAfter: number
  createdAt: number
}

export interface Child {
  id: string
  name: string
  key: string
  enabled: boolean
  creditsGranted: number
  creditsUsed: number
  balance: number
  requestsCount: number
  tokensUsed: number
  createdAt: number
  expiresAt: number
  status: 'active' | 'disabled' | 'expired'
}

export interface Reseller {
  budget: number
  ownUsed: number
  allocated: number
  allocatable: number
  subKeyCount: number
  children: Child[]
}

/* key persistence, compatible with the previous portal */
const STORE = 'user_api_key'
const REMEMBER = 'user_key_remember'
export function storedKey(): string {
  try {
    return sessionStorage.getItem(STORE) || (localStorage.getItem(REMEMBER) === '1' ? localStorage.getItem(STORE) || '' : '')
  } catch {
    return ''
  }
}
export function isKeyRemembered() {
  try {
    return localStorage.getItem(REMEMBER) === '1'
  } catch {
    return false
  }
}
export function persistKey(k: string, remember: boolean) {
  try {
    sessionStorage.setItem(STORE, k)
    if (remember) {
      localStorage.setItem(STORE, k)
      localStorage.setItem(REMEMBER, '1')
    } else {
      localStorage.removeItem(STORE)
      localStorage.removeItem(REMEMBER)
    }
  } catch {
    /* ignore */
  }
}
export function clearKey() {
  try {
    sessionStorage.removeItem(STORE)
    localStorage.removeItem(STORE)
    localStorage.removeItem(REMEMBER)
  } catch {
    /* ignore */
  }
}

export function maskKey(k: string) {
  if (!k) return ''
  if (k.length <= 12) return k.slice(0, 3) + '••••'
  return `${k.slice(0, 7)}••••••••${k.slice(-4)}`
}
