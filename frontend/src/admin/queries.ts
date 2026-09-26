import { useQuery } from '@tanstack/react-query'
import { api, type Account, type ApiKey, type Overview, type RequestLog } from './api'

export type Tab = 'overview' | 'accounts' | 'keys' | 'logs' | 'settings'

export const qk = {
  overview: ['overview'] as const,
  accounts: ['accounts'] as const,
  keys: ['apiKeys'] as const,
  logs: ['logs'] as const,
  status: ['status'] as const,
  version: ['version'] as const,
}

// Cadence mirrors the backend: overview is realtime, accounts/keys only move on the
// backend's 5-minute refresh cycle, so a 10 s poll while visible is plenty.
export function useOverview(active: boolean) {
  return useQuery({
    queryKey: qk.overview,
    queryFn: () => api<Overview>('/overview?days=14'),
    enabled: active,
    refetchInterval: active ? 1000 : false,
    staleTime: 800,
  })
}

export function useAccounts(active = false) {
  return useQuery({
    queryKey: qk.accounts,
    queryFn: () => api<Account[]>('/accounts').then((d) => (Array.isArray(d) ? d : [])),
    refetchInterval: active ? 10_000 : 60_000,
    staleTime: 4_000,
  })
}

export function useApiKeys(active = false) {
  return useQuery({
    queryKey: qk.keys,
    queryFn: () => api<{ apiKeys: ApiKey[] }>('/api-keys').then((d) => (Array.isArray(d?.apiKeys) ? d.apiKeys : [])),
    refetchInterval: active ? 10_000 : 60_000,
    staleTime: 4_000,
  })
}

export function useLogs(active: boolean) {
  return useQuery({
    queryKey: qk.logs,
    queryFn: () => api<{ logs: RequestLog[] }>('/logs').then((d) => (Array.isArray(d?.logs) ? d.logs : [])),
    enabled: active,
    refetchInterval: active ? 2000 : false,
    staleTime: 1000,
  })
}

export interface Status {
  version: string
  accounts: number
  available: number
  totalRequests: number
  uptime: number
}

export function useStatus() {
  return useQuery({
    queryKey: qk.status,
    queryFn: () => api<Status>('/status'),
    refetchInterval: 30_000,
    retry: 1,
  })
}
