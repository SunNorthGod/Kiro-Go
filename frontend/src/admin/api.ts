// Admin API client + response types. Every call carries X-Admin-Password; a 401
// anywhere signs the operator out.
import { beginActivity } from '@/lib/activity'

export class ApiError extends Error {
  status: number
  data: unknown
  constructor(message: string, status: number, data?: unknown) {
    super(message)
    this.status = status
    this.data = data
  }
}

let password = ''
let onUnauthorized: (() => void) | null = null

export function setAdminPassword(p: string) {
  password = p
}
export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn
}

export async function api<T = unknown>(
  path: string,
  opts: { method?: string; body?: unknown; signal?: AbortSignal; auth?: string } = {},
): Promise<T> {
  const headers: Record<string, string> = { 'X-Admin-Password': opts.auth ?? password }
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  const method = opts.method || 'GET'
  const end = method !== 'GET' ? beginActivity() : null
  let res: Response
  let text: string
  try {
    res = await fetch('/admin/api' + path, {
      method,
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      cache: 'no-store',
      signal: opts.signal,
    })
    text = await res.text()
  } catch (e) {
    if ((e as Error).name === 'AbortError') throw e
    throw new ApiError('无法连接到服务，请检查网络', 0)
  } finally {
    end?.()
  }
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  const errMsg: string =
    data && typeof data === 'object' && 'error' in data ? String((data as { error?: unknown }).error || '') : ''
  if (res.status === 401) {
    if (!opts.auth) onUnauthorized?.()
    throw new ApiError('密码错误或登录已失效', 401, data)
  }
  if (res.status === 429) throw new ApiError('尝试次数过多，请稍后再试', 429, data)
  if (!res.ok) throw new ApiError(errMsg || `请求失败（HTTP ${res.status}）`, res.status, data)
  if (data && typeof data === 'object' && (data as { success?: boolean }).success === false) {
    throw new ApiError(errMsg || '操作失败', res.status, data)
  }
  return data as T
}

/* ---------------- types ---------------- */

export interface Overview {
  totalRequests: number
  successRequests: number
  failedRequests: number
  totalTokens: number
  totalCredits: number
  uptime: number
  totalRPM: number
  totalTPM: number
  accounts: { total: number; available: number; enabled: number; disabled: number }
  keys: { total: number; active: number }
  concurrency: { inflight: number; activeAccounts: number; activeKeys: number }
  cache: { stickyHits: number; stickyMisses: number; stickySessions: number }
  promptCache: { hitRate: number | null; readTokens: number; creationTokens: number; inputTokens: number; windowDays: number }
  daily: { date: string; requests: number; tokens: number; credits: number }[]
}

export interface Account {
  id: string
  email: string
  userId: string
  nickname: string
  authMethod: string
  provider: string
  region: string
  createdAt: number
  enabled: boolean
  banStatus: string
  banReason: string
  banTime: number
  expiresAt: number
  hasToken: boolean
  canRefresh: boolean
  machineId: string
  weight: number
  overageStatus: string
  overageCapability: string
  overageCap: number
  overageRate: number
  currentOverages: number
  overageCheckedAt: number
  proxyURL: string
  subscriptionType: string
  subscriptionTitle: string
  daysRemaining: number
  usageCurrent: number
  usageLimit: number
  usagePercent: number
  nextResetDate: string
  lastRefresh: number
  trialUsageCurrent: number
  trialUsageLimit: number
  trialUsagePercent: number
  trialStatus: string
  trialExpiresAt: number
  requestCount: number
  errorCount: number
  totalTokens: number
  totalCredits: number
  lastUsed: number
  rpm: number
}

export interface CachedModel {
  modelId: string
  modelName?: string
  description?: string
  rateMultiplier?: number
}

export interface ApiKey {
  id: string
  name?: string
  key: string
  keyMasked: string
  rpm: number
  enabled: boolean
  migrated?: boolean
  createdAt: number
  lastUsedAt?: number
  tokenLimit?: number
  creditLimit?: number
  tokensUsed: number
  creditsUsed: number
  requestsCount: number
  creditsGranted: number
  balance: number
  expiresAt?: number
  expired: boolean
  maxConcurrency?: number | null
  maxRPM?: number | null
  boundAccountIds?: string[]
  parentKeyId?: string
}

export interface KeyUsage {
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
  totalOutputTokens?: number
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
  cacheCreationInputTokens: number
}

export interface RechargeRecord {
  amount: number
  operator: string
  note: string
  balanceAfter: number
  createdAt: number
}

export interface KeyChild {
  id: string
  name: string
  enabled: boolean
  creditsGranted: number
  creditsUsed: number
  balance: number
  requestsCount: number
}

export interface RequestLog {
  time: number
  endpoint: string
  model: string
  accountId: string
  status: 'success' | 'error'
  error: string
  errorType: string
  tokens: number
  credits: number
  duration: number
}

export interface PromptRule {
  id: string
  name: string
  type: 'regex' | 'lines-containing'
  match: string
  replace?: string
  enabled: boolean
}

export interface PromptFilter {
  filterClaudeCode: boolean
  filterEnvNoise: boolean
  filterStripBoundaries: boolean
  rules: PromptRule[] | null
}

export interface ExportData {
  version: string
  exportedAt: number
  accounts: ExportAccount[]
  groups: unknown[]
  tags: unknown[]
}

export interface ExportAccount {
  id: string
  email: string
  nickname?: string
  idp: string
  userId?: string
  profileArn?: string
  machineId?: string
  credentials: {
    accessToken: string
    refreshToken: string
    clientId?: string
    clientSecret?: string
    region?: string
    authMethod?: string
    provider?: string
    tokenEndpoint?: string
    issuerUrl?: string
    scopes?: string
  }
}
