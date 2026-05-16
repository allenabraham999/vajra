import axios, { AxiosError } from 'axios'
import type { AxiosInstance, AxiosRequestConfig } from 'axios'
import type {
  APIKey,
  AuthConfigResponse,
  AuthLoginResponse,
  AuthRegisterResponse,
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
  UsageResponse,
  Webhook,
  WebhookEventName,
} from './types'

// JWT lives in module-private memory. Reload = logout, by design.
let inMemoryToken: string | null = null

export function setAuthToken(token: string | null) {
  inMemoryToken = token
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
  restore: (id: string, name: string) =>
    request<Sandbox>({
      method: 'POST',
      url: `/v1/snapshots/${id}/restore`,
      data: { name },
    }),
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
}

// --- Pre-warm pool ---
export const pool = {
  stats: () => request<PoolStats>({ method: 'GET', url: '/v1/pool/stats' }),
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
  pool,
}
