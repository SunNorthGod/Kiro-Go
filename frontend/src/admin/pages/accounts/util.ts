import type { Account, ExportAccount } from '../../api'
import type { Tone } from '@/ui/controls'

export const isBanned = (a: Account) => !!a.banStatus && a.banStatus !== 'ACTIVE'

/** A short-lived access token past expiry is normal when the account can refresh it. */
export const tokenExpired = (a: Account) => !!a.expiresAt && a.expiresAt < Date.now() / 1000 && !a.canRefresh

export function dotTone(a: Account): 'ok' | 'warn' | 'bad' {
  if (isBanned(a) || !a.hasToken || tokenExpired(a)) return 'bad'
  if (!a.enabled) return 'warn'
  return 'ok'
}

/** Kiro API-key accounts store the full ksk_ key as their email; never print it whole. */
export function displayName(a: Pick<Account, 'email' | 'id'>): string {
  const e = a.email || ''
  if (e.startsWith('ksk_') && e.length > 18) return `${e.slice(0, 10)}…${e.slice(-4)}`
  return e || (a.id ? a.id.slice(0, 12) + '…' : '—')
}

export function subscription(type: string): { label: string; tone: Tone } {
  const s = (type || '').toUpperCase()
  if (s.includes('POWER')) return { label: 'Power', tone: 'pink' }
  if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return { label: 'Pro+', tone: 'accent' }
  if (s.includes('PRO')) return { label: 'Pro', tone: 'accent' }
  if (s.includes('FREE') || !type) return { label: 'Free', tone: 'outline' }
  return { label: type, tone: 'outline' }
}

export function authLabel(a: Pick<Account, 'provider' | 'authMethod'>): string {
  const method = a.provider || a.authMethod
  if (!method) return '—'
  const n = method.toLowerCase()
  if (n === 'external_idp' || n === 'azuread') return 'Microsoft SSO'
  if (n === 'idc' || n === 'enterprise') return '企业 SSO'
  if (n === 'social') return '社交登录'
  if (n === 'api_key' || n === 'apikey' || n === 'kiroapikey') return 'API Key'
  if (n === 'builderid') return 'Builder ID'
  if (n === 'github') return 'GitHub'
  if (n === 'google') return 'Google'
  return method
}

export function trialSuffix(ts: number): string {
  if (!ts) return ''
  const days = Math.ceil((ts * 1000 - Date.now()) / 86_400_000)
  if (days < 0) return '已过期'
  if (days === 0) return '今天到期'
  if (days <= 7) return `${days} 天后到期`
  return ''
}

export function remainingQuota(a: Account): number {
  let r = 0
  if (a.usageLimit > 0) r += Math.max(0, a.usageLimit - a.usageCurrent)
  if (a.trialUsageLimit > 0) r += Math.max(0, a.trialUsageLimit - a.trialUsageCurrent)
  return r
}

/* ---------- credential JSON import (same rules the previous panel used) ---------- */

type Rec = Record<string, unknown>
const str = (v: unknown) => (v == null ? '' : String(v))

/** `credentials.*` wins over top-level whenever the key exists in credentials. */
export function normalizeCredentialRecord(record: unknown) {
  const source = (record && typeof record === 'object' ? record : {}) as Rec
  const credentials = (source.credentials && typeof source.credentials === 'object' ? source.credentials : {}) as Rec
  const value = (key: string) => (Object.prototype.hasOwnProperty.call(credentials, key) ? credentials[key] : source[key])
  return {
    id: str(value('id')),
    email: str(value('email')),
    userId: str(value('userId')),
    nickname: str(value('nickname')),
    profileArn: str(value('profileArn')),
    accessToken: str(value('accessToken')),
    refreshToken: str(value('refreshToken')),
    kiroApiKey: str(value('kiroApiKey')),
    clientId: str(value('clientId')),
    clientSecret: str(value('clientSecret')),
    authMethod: str(value('authMethod')),
    provider: str(value('provider') || source.idp),
    region: str(value('region')),
    tokenEndpoint: str(value('tokenEndpoint')),
    issuerUrl: str(value('issuerUrl')),
    scopes: str(value('scopes')),
  }
}

const EXTERNAL_ALIASES = ['external_idp', 'external-idp', 'external', 'microsoft', 'm365', 'office365', 'azure', 'azuread', 'azure-ad', 'azure_ad', 'entra', 'entra-id']

/** Build the /auth/credentials body for one record, or null when it cannot be imported. */
export function credentialPayload(item: ReturnType<typeof normalizeCredentialRecord>): Rec | null {
  const rawMethod = item.authMethod.trim()
  const rawProvider = item.provider.trim()
  const methodKey = rawMethod.toLowerCase()
  const providerKey = rawProvider.toLowerCase()
  const kiroApiKey = item.kiroApiKey.trim()
  const isApiKey =
    Boolean(kiroApiKey) || methodKey === 'api_key' || methodKey === 'apikey' || (!item.refreshToken && item.accessToken.trim().startsWith('ksk_'))
  if (!isApiKey && !item.refreshToken) return null
  if (isApiKey) {
    return {
      id: item.id,
      email: item.email,
      userId: item.userId,
      nickname: item.nickname,
      kiroApiKey: kiroApiKey || item.accessToken,
      authMethod: 'api_key',
      provider: rawProvider || 'APIKey',
      region: item.region || 'us-east-1',
    }
  }
  const isExternalIdp = EXTERNAL_ALIASES.includes(methodKey) || EXTERNAL_ALIASES.includes(providerKey) || Boolean(item.tokenEndpoint || item.issuerUrl)
  let authMethod: string
  if (isExternalIdp) authMethod = 'external_idp'
  else if (item.clientId && item.clientSecret) authMethod = 'idc'
  else if (methodKey === 'idc') authMethod = 'idc'
  else if (methodKey === 'social' || methodKey === 'google' || methodKey === 'github') authMethod = 'social'
  else authMethod = methodKey ? 'social' : ''
  let provider = isExternalIdp ? 'AzureAD' : rawProvider
  if (!provider && authMethod === 'social') provider = 'Google'
  if (!provider && authMethod === 'idc') provider = 'BuilderId'
  return {
    id: item.id,
    email: item.email,
    userId: item.userId,
    nickname: item.nickname,
    profileArn: item.profileArn,
    refreshToken: item.refreshToken,
    accessToken: item.accessToken,
    clientId: item.clientId,
    clientSecret: item.clientSecret,
    authMethod,
    provider,
    region: item.region || (isExternalIdp ? '' : 'us-east-1'),
    tokenEndpoint: item.tokenEndpoint,
    issuerUrl: item.issuerUrl,
    scopes: item.scopes,
  }
}

/** Flat, re-importable credential JSON for one exported account ("复制凭证 JSON"). */
export function importPayloadFromExport(a: ExportAccount): Rec {
  const c = a.credentials || ({} as ExportAccount['credentials'])
  const payload: Rec = {
    clientId: c.clientId || '',
    clientSecret: c.clientSecret || '',
    accessToken: c.accessToken || '',
    refreshToken: c.refreshToken || '',
  }
  const extra: Rec = {
    authMethod: c.authMethod,
    provider: c.provider || a.idp,
    tokenEndpoint: c.tokenEndpoint,
    issuerUrl: c.issuerUrl,
    scopes: c.scopes,
    userId: a.userId,
    profileArn: a.profileArn,
    region: c.region,
  }
  for (const [k, v] of Object.entries(extra)) if (v) payload[k] = v
  return payload
}
