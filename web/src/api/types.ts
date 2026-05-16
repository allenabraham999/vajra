// Types match the Go models in internal/models/. These are the JSON
// shapes the master returns; keep field names in sync with the Go
// `json:"…"` tags or the typed client will silently miss fields.

export type SandboxState =
  | 'PENDING'
  | 'CREATING'
  | 'RUNNING'
  | 'PAUSING'
  | 'PAUSED'
  | 'STOPPING'
  | 'STOPPED'
  | 'ARCHIVING'
  | 'ARCHIVED'
  | 'DESTROYING'
  | 'DESTROYED'
  | 'ERROR'

export type NodeState =
  | 'REGISTERING'
  | 'ACTIVE'
  | 'DRAINING'
  | 'QUARANTINED'
  | 'OFFLINE'
  | 'DECOMMISSIONED'

export type ClusterState = 'ACTIVE' | 'DRAINING' | 'OFFLINE'

export type OperationStatus =
  | 'PENDING'
  | 'IN_PROGRESS'
  | 'COMPLETED'
  | 'FAILED'

export type OperationType =
  | 'CREATE'
  | 'STOP'
  | 'START'
  | 'DESTROY'
  | 'SNAPSHOT'
  | 'RESTORE'
  | 'CLONE'
  | 'MIGRATE'
  | 'ARCHIVE'

export interface SandboxConfig {
  vcpus: number
  memory_mb: number
  disk_gb: number
}

export interface Sandbox {
  id: string
  name: string
  account_id: string
  node_id?: string | null
  cluster_id?: string | null
  template_id: string
  state: SandboxState
  config: SandboxConfig
  auto_stop_minutes?: number
  auto_archive_minutes?: number
  last_activity?: string
  created_at: string
  updated_at: string
  operation_id?: string
}

export type BuildStatus = 'PENDING' | 'BUILDING' | 'COMPLETED' | 'FAILED'

export interface Build {
  id: string
  account_id: string
  template_name: string
  template_version: string
  status: BuildStatus
  template_id?: string | null
  error?: string | null
  created_at: string
  completed_at?: string | null
}

export type WebhookEventName =
  | 'sandbox.created'
  | 'sandbox.running'
  | 'sandbox.stopped'
  | 'sandbox.destroyed'
  | 'sandbox.error'
  | 'sandbox.archived'

export interface Webhook {
  id: string
  account_id: string
  url: string
  secret?: string
  events: WebhookEventName[]
  active: boolean
  created_at: string
}

export interface NodeCapacity {
  total_cpu: number
  total_memory_mb: number
  total_disk_gb: number
}

export interface NodeUsage {
  used_cpu: number
  used_memory_mb: number
  used_disk_gb: number
}

export interface Node {
  id: string
  cluster_id: string
  hostname: string
  ip: string
  state: NodeState
  capacity: NodeCapacity
  used_resources: NodeUsage
  last_heartbeat: string
}

export interface Cluster {
  id: string
  name: string
  region: string
  state: ClusterState
  created_at: string
}

export interface Template {
  id: string
  account_id: string
  name: string
  version: string
  hash: string
  rootfs_path: string
  kernel_path: string
  snapshot_path: string
  created_at: string
}

export interface Snapshot {
  id: string
  sandbox_id: string
  account_id: string
  node_id: string
  storage_path: string
  size_bytes: number
  created_at: string
}

export interface Operation {
  id: string
  account_id: string
  sandbox_id: string
  type: OperationType
  status: OperationStatus
  started_at: string
  completed_at?: string | null
  error?: string | null
}

export interface APIKey {
  id: string
  name: string
  created_at: string
}

export interface CreateAPIKeyResponse extends APIKey {
  key: string
}

export interface AuthLoginResponse {
  token: string
  expires_at: string
}

export interface AuthRegisterResponse {
  account_id: string
  api_key: string
}

export interface AuthConfigResponse {
  google_oauth_enabled: boolean
  email_auth_enabled: boolean
}

export interface ExecResult {
  exit_code: number
  stdout: string
  stderr: string
}

export interface FileEntry {
  name: string
  path: string
  size: number
  is_dir: boolean
  mode: number
  mod_time: string
}

export interface ShareLink {
  id: string
  token?: string // only present at creation
  url?: string
  port?: number | null
  expires_at?: string | null
  created_at: string
  revoked_at?: string | null
}

export interface UsageRow {
  sandbox_id: string
  sandbox_name: string
  vcpus: number
  memory_mb: number
  disk_gb: number
  duration_hours: number
  cost_usd: number
}

export interface UsageResponse {
  rows: UsageRow[]
  total_cost_usd: number
  vcpu_hours: number
  memory_gb_hours: number
  storage_gb_hours: number
}

// PoolStats is the pre-warm pool snapshot from GET /v1/pool/stats.
export interface PoolStats {
  min_size: number
  max_size: number
  target_size: number
  available: number
  warming: number
  total_hits: number
  total_misses: number
  total_created: number
  hit_rate_pct: number
  template: string
}

// BootTime is one recent sandbox create from GET /v1/sandboxes/boot-times.
export interface BootTime {
  id: string
  name: string
  created_at: string
  time_to_running_ms: number
  pool_hit: boolean
}
