import axios, { AxiosError } from 'axios'
import type { AxiosInstance, AxiosRequestConfig } from 'axios'
import type {
  AdminAccount,
  AdminLogsResponse,
  AdminNode,
  AdminOverview,
  AdminSandboxesResponse,
  APIKey,
  AuthConfigResponse,
  AuthLoginResponse,
  AuthRegisterResponse,
  BillingConfigResponse,
  BootTime,
  Build,
  Cluster,
  CreateAPIKeyResponse,
  ExecResult,
  FileEntry,
  Node,
  Operation,
  PoolStats,
  Sandbox,
  Snapshot,
  Template,
  TransactionResponse,
  UsageResponse,
  UsageSummary,
  Webhook,
  WebhookEventName,
} from './types'

// LogSource is the origin of a sandbox log line, also the ?source=
// filter accepted by the logs endpoints.
export type LogSource = 'master' | 'agent' | 'guest'

// LogEntry is one line in a sandbox's merged log stream, as returned by
// GET /v1/sandboxes/{id}/logs and pushed over the /logs/stream socket.
export interface LogEntry {
  timestamp: string
  source: LogSource
  level: 'INFO' | 'WARN' | 'ERROR'
  message: string
}

// The JWT is held in module memory for the request interceptor and
// mirrored to localStorage so a page reload keeps the user signed in.
const TOKEN_KEY = 'vajra.token'

// readStoredToken pulls a persisted JWT from localStorage. Wrapped in a
// try/catch because Safari private mode throws on any storage access.
function readStoredToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY)
  } catch {
    return null
  }
}

let inMemoryToken: string | null = readStoredToken()

// setAuthToken updates the in-memory JWT and mirrors it to localStorage
// so it survives a reload. Passing null clears both.
export function setAuthToken(token: string | null) {
  inMemoryToken = token
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    // Storage unavailable (private mode / disabled): fall back to an
    // in-memory token; the session simply won't survive a reload.
  }
}

export function getAuthToken(): string | null {
  return inMemoryToken
}

const baseURL = (import.meta.env.VITE_API_URL as string | undefined) ?? ''

const http: AxiosInstance = axios.create({
  baseURL,
  headers: { 'Content-Type': 'application/json' },
})

http.interceptors.request.use((config) => {
  if (inMemoryToken) {
    config.headers.Authorization = `Bearer ${inMemoryToken}`
  }
  return config
})

// onAuthRefresh is installed by AuthProvider. When a request comes back
// 401 the response interceptor calls it once to re-mint a token, then
// replays the original request — so dashboard polling survives a token
// that lapsed while the tab sat idle (e.g. through an autoscale wait).
// Returning null means the session is unrecoverable (genuine logout).
let onAuthRefresh: (() => Promise<string | null>) | null = null

export function setAuthRefreshHandler(
  fn: (() => Promise<string | null>) | null,
) {
  onAuthRefresh = fn
}

http.interceptors.response.use(
  (resp) => resp,
  async (error: unknown) => {
    if (!axios.isAxiosError(error)) return Promise.reject(error)
    const cfg = error.config as
      | (AxiosRequestConfig & { _retried?: boolean })
      | undefined
    // Retry exactly once on 401, and never for the auth endpoints
    // themselves — a failed login or refresh must surface, not loop.
    if (
      error.response?.status === 401 &&
      cfg &&
      !cfg._retried &&
      onAuthRefresh &&
      !(cfg.url ?? '').includes('/v1/auth/')
    ) {
      cfg._retried = true
      const fresh = await onAuthRefresh()
      // The request interceptor re-reads the in-memory token, so once
      // refresh succeeds the replay automatically carries the new one.
      if (fresh) return http.request(cfg)
    }
    return Promise.reject(error)
  },
)

export class ApiError extends Error {
  status: number
  body: unknown
  constructor(status: number, message: string, body: unknown) {
    super(message)
    this.status = status
    this.body = body
  }
}

function unwrap(err: unknown): never {
  if (axios.isAxiosError(err)) {
    const ax = err as AxiosError<{ error?: string; message?: string }>
    const status = ax.response?.status ?? 0
    const data = ax.response?.data
    const msg =
      (typeof data === 'object' && data && (data.error ?? data.message)) ||
      ax.message ||
      'request failed'
    throw new ApiError(status, msg, data)
  }
  throw err
}

async function request<T>(config: AxiosRequestConfig): Promise<T> {
  try {
    const r = await http.request<T>(config)
    return r.data
  } catch (e) {
    unwrap(e)
  }
}

// --- Auth ---
export const auth = {
  login: (email: string, password: string) =>
    request<AuthLoginResponse>({
      method: 'POST',
      url: '/v1/auth/login',
      data: { email, password },
    }),
  register: (email: string, password: string) =>
    request<AuthRegisterResponse>({
      method: 'POST',
      url: '/v1/auth/register',
      data: { email, password },
    }),
  config: () =>
    request<AuthConfigResponse>({ method: 'GET', url: '/v1/auth/config' }),
  refresh: () =>
    request<AuthLoginResponse>({ method: 'POST', url: '/v1/auth/refresh' }),
}

// apiBase is the absolute URL prefix for full-page navigations to master
// endpoints (the OAuth initiate redirect, mainly). Empty string keeps
// same-origin behavior, matching the axios `baseURL` default.
export const apiBase: string =
  (import.meta.env.VITE_API_URL as string | undefined) ?? ''

// --- Sandboxes ---
export const sandboxes = {
  list: () => request<Sandbox[]>({ method: 'GET', url: '/v1/sandboxes' }),
  bootTimes: () =>
    request<BootTime[]>({ method: 'GET', url: '/v1/sandboxes/boot-times' }),
  get: (id: string) =>
    request<Sandbox>({ method: 'GET', url: `/v1/sandboxes/${id}` }),
  create: (body: {
    name: string
    source: 'image' | 'snapshot'
    template_id?: string
    snapshot_id?: string
    vcpus: number
    memory_mb: number
    disk_gb: number
    region?: string
    auto_stop_minutes?: number
    auto_archive_minutes?: number
    git_url?: string
    git_branch?: string
    git_token?: string
  }) =>
    request<Sandbox>({ method: 'POST', url: '/v1/sandboxes', data: body }),
  exec: (id: string, command: string, timeout_ms = 30_000) =>
    request<ExecResult>({
      method: 'POST',
      url: `/v1/sandboxes/${id}/exec`,
      data: { command, timeout_ms },
    }),
  stop: (id: string) =>
    request<Sandbox>({ method: 'POST', url: `/v1/sandboxes/${id}/stop` }),
  start: (id: string) =>
    request<Sandbox>({ method: 'POST', url: `/v1/sandboxes/${id}/start` }),
  destroy: (id: string) =>
    request<void>({ method: 'DELETE', url: `/v1/sandboxes/${id}` }),
  snapshot: (id: string) =>
    request<Snapshot>({
      method: 'POST',
      url: `/v1/sandboxes/${id}/snapshot`,
    }),
  listSnapshots: (id: string) =>
    request<Snapshot[]>({
      method: 'GET',
      url: `/v1/sandboxes/${id}/snapshots`,
    }),
  logs: (id: string, source: LogSource | 'all' = 'all', tail = 500) =>
    request<{ entries: LogEntry[] }>({
      method: 'GET',
      url: `/v1/sandboxes/${id}/logs`,
      params: { source, tail },
    }),
  // The master wraps the listing in {"entries": [...]}; unwrap it here
  // so callers get a plain array.
  listFiles: async (id: string, path = '/'): Promise<FileEntry[]> => {
    const r = await request<{ entries: FileEntry[] | null }>({
      method: 'GET',
      url: `/v1/sandboxes/${id}/files/list`,
      params: { path },
    })
    return r.entries ?? []
  },
  uploadFile: async (id: string, path: string, file: File) => {
    const buf = await file.arrayBuffer()
    return request<{ ok: boolean }>({
      method: 'POST',
      url: `/v1/sandboxes/${id}/files/upload`,
      headers: {
        'Content-Type': 'application/octet-stream',
        'X-Vajra-Path': path,
      },
      data: buf,
    })
  },
  downloadFileURL: (id: string, path: string) =>
    `${baseURL}/v1/sandboxes/${id}/files/download?path=${encodeURIComponent(path)}`,
  deleteFile: (id: string, path: string) =>
    request<{ ok: boolean }>({
      method: 'DELETE',
      url: `/v1/sandboxes/${id}/files`,
      params: { path },
    }),
}

// --- Snapshots ---
export const snapshotsApi = {
  // list returns every snapshot the account owns, across all sandboxes.
  list: () => request<Snapshot[]>({ method: 'GET', url: '/v1/snapshots' }),
  restore: (id: string, name: string) =>
    request<Sandbox>({
      method: 'POST',
      url: `/v1/snapshots/${id}/restore`,
      data: { name },
    }),
  delete: (id: string) =>
    request<void>({ method: 'DELETE', url: `/v1/snapshots/${id}` }),
  clone: (id: string, name: string) =>
    request<Sandbox>({
      method: 'POST',
      url: `/v1/snapshots/${id}/clone`,
      data: { name },
    }),
  promote: (id: string, name: string, version: string) =>
    request<Template>({
      method: 'POST',
      url: `/v1/snapshots/${id}/promote`,
      data: { name, version },
    }),
}

// --- Templates ---
export const templates = {
  list: () => request<Template[]>({ method: 'GET', url: '/v1/templates' }),
  create: (body: {
    name: string
    version: string
    hash: string
    rootfs_path: string
    kernel_path: string
    snapshot_path?: string
  }) =>
    request<Template>({ method: 'POST', url: '/v1/templates', data: body }),
  build: (body: { name: string; version: string; dockerfile: string }) =>
    request<{ build_id: string; status: string; template_name: string; template_version: string; created_at: string }>({
      method: 'POST',
      url: '/v1/templates/build',
      data: body,
    }),
  buildStatus: (id: string) =>
    request<Build>({ method: 'GET', url: `/v1/templates/builds/${id}` }),
  listBuilds: () => request<Build[]>({ method: 'GET', url: '/v1/templates/builds' }),
}

// --- Webhooks ---
export const webhooks = {
  list: () => request<Webhook[]>({ method: 'GET', url: '/v1/webhooks' }),
  create: (url: string, events: WebhookEventName[]) =>
    request<Webhook>({
      method: 'POST',
      url: '/v1/webhooks',
      data: { url, events },
    }),
  delete: (id: string) =>
    request<void>({ method: 'DELETE', url: `/v1/webhooks/${id}` }),
  test: (id: string) =>
    request<{ webhook_id: string; delivered: boolean }>({
      method: 'POST',
      url: `/v1/webhooks/${id}/test`,
    }),
}

// --- Admin (clusters, nodes) ---
export const clusters = {
  list: () => request<Cluster[]>({ method: 'GET', url: '/v1/clusters' }),
}

export const nodes = {
  list: () => request<Node[]>({ method: 'GET', url: '/v1/nodes' }),
  drain: (id: string) =>
    request<void>({ method: 'POST', url: `/v1/nodes/${id}/drain` }),
}

// --- API keys ---
export const apiKeys = {
  list: () => request<APIKey[]>({ method: 'GET', url: '/v1/api-keys' }),
  create: (name: string) =>
    request<CreateAPIKeyResponse>({
      method: 'POST',
      url: '/v1/api-keys',
      data: { name },
    }),
  delete: (id: string) =>
    request<void>({ method: 'DELETE', url: `/v1/api-keys/${id}` }),
}

// --- Usage ---
export const usage = {
  get: (params?: { from?: string; to?: string }) =>
    request<UsageResponse>({ method: 'GET', url: '/v1/usage', params }),
  summary: () =>
    request<UsageSummary>({ method: 'GET', url: '/v1/usage/summary' }),
}

// --- Billing (Stripe credit purchases) ---
export const billing = {
  config: () =>
    request<BillingConfigResponse>({ method: 'GET', url: '/v1/billing/config' }),
  checkout: (amount_usd: number) =>
    request<{ url: string }>({
      method: 'POST',
      url: '/v1/billing/checkout',
      data: { amount_usd },
    }),
  transactions: () =>
    request<{ transactions: TransactionResponse[] }>({
      method: 'GET',
      url: '/v1/billing/transactions',
    }),
}

// --- Pre-warm pool ---
export const pool = {
  stats: () => request<PoolStats>({ method: 'GET', url: '/v1/pool/stats' }),
}

// --- Admin panel (operator-only; every endpoint is gated by requireAdmin) ---
export const admin = {
  overview: () =>
    request<AdminOverview>({ method: 'GET', url: '/v1/admin/cluster/overview' }),
  nodes: () => request<AdminNode[]>({ method: 'GET', url: '/v1/admin/nodes' }),
  drainNode: (id: string) =>
    request<void>({ method: 'POST', url: `/v1/admin/nodes/${id}/drain` }),
  cordonNode: (id: string) =>
    request<void>({ method: 'POST', url: `/v1/admin/nodes/${id}/cordon` }),
  uncordonNode: (id: string) =>
    request<void>({ method: 'POST', url: `/v1/admin/nodes/${id}/uncordon` }),
  terminateNode: (id: string) =>
    request<void>({ method: 'POST', url: `/v1/admin/nodes/${id}/terminate` }),
  sandboxes: (params?: {
    state?: string
    account?: string
    node?: string
    limit?: number
    offset?: number
  }) =>
    request<AdminSandboxesResponse>({
      method: 'GET',
      url: '/v1/admin/sandboxes',
      params,
    }),
  stopSandbox: (id: string) =>
    request<void>({ method: 'POST', url: `/v1/admin/sandboxes/${id}/stop` }),
  destroySandbox: (id: string) =>
    request<void>({ method: 'DELETE', url: `/v1/admin/sandboxes/${id}` }),
  accounts: () =>
    request<AdminAccount[]>({ method: 'GET', url: '/v1/admin/accounts' }),
  addCredits: (id: string, amount: number) =>
    request<{ account_id: string; added: number; credits: number }>({
      method: 'POST',
      url: `/v1/admin/accounts/${id}/credits`,
      data: { amount },
    }),
  suspendAccount: (id: string, suspended?: boolean) =>
    request<{ account_id: string; suspended: boolean }>({
      method: 'POST',
      url: `/v1/admin/accounts/${id}/suspend`,
      data: suspended === undefined ? {} : { suspended },
    }),
  promoteAccount: (id: string, isAdmin?: boolean) =>
    request<{ account_id: string; is_admin: boolean }>({
      method: 'POST',
      url: `/v1/admin/accounts/${id}/promote`,
      data: isAdmin === undefined ? {} : { is_admin: isAdmin },
    }),
  resetPassword: (id: string) =>
    request<{ account_id: string; temporary_password: string }>({
      method: 'POST',
      url: `/v1/admin/accounts/${id}/reset-password`,
    }),
  logs: (params?: {
    source?: 'master' | 'agent'
    tail?: number
    level?: string
    sandbox_id?: string
  }) => request<AdminLogsResponse>({ method: 'GET', url: '/v1/admin/logs', params }),
}

// --- Operations (synthesised from sandbox responses; no direct list endpoint
// exists yet, so callers fall back to deriving from sandboxes). ---
export type { Operation }

export default {
  auth,
  sandboxes,
  snapshots: snapshotsApi,
  templates,
  webhooks,
  clusters,
  nodes,
  apiKeys,
  usage,
  billing,
  admin,
  pool,
}
