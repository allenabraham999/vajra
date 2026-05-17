import { useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { Highlight, themes } from 'prism-react-renderer'
import { Search, Copy, Check, Menu, X, ArrowRight, CornerDownRight, FileJson } from 'lucide-react'

/* ----------------------------------------------------------------------
 * Constants
 * -------------------------------------------------------------------- */
const HOST = 'http://13.202.0.224.nip.io'
const KEY = 'vj_live_b1d488...'
const OPENAPI_URL = '/v1/docs/openapi.yaml'
const SWAGGER_URL = '/v1/docs'

/* ----------------------------------------------------------------------
 * Types
 * -------------------------------------------------------------------- */
type Method = 'GET' | 'POST' | 'PUT' | 'DELETE'

interface Param {
  name: string
  type: string
  required: boolean
  description: string
}

interface CodeTab {
  label: string
  language: string
  code: string
}

interface Endpoint {
  id: string
  method: Method
  path: string
  description: string
  planned?: boolean
  params?: Param[]
  examples?: CodeTab[]
  response?: { status: string; body: string }
}

interface Section {
  id: string
  title: string
  anchors: { id: string; label: string }[]
  Body: () => ReactNode
}

/* ----------------------------------------------------------------------
 * Clipboard — the production dashboard is served over plain HTTP, where
 * navigator.clipboard is undefined. Fall back to the legacy execCommand
 * path so the copy buttons work everywhere.
 * -------------------------------------------------------------------- */
async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall through to legacy path */
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.focus()
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}

/* curl builds a cURL command string for an endpoint, with optional body. */
function curl(method: Method, path: string, body?: string): string {
  const lines = [`curl -X ${method} ${HOST}${path} \\`, `  -H "Authorization: Bearer ${KEY}"`]
  if (body !== undefined) {
    lines[1] += ' \\'
    lines.push('  -H "Content-Type: application/json" \\')
    lines.push(`  -d '${body}'`)
  }
  return lines.join('\n')
}

/* apiTabs assembles the standard Python | cURL | TypeScript tab triple. */
function apiTabs(python: string, curlCmd: string, ts: string): CodeTab[] {
  return [
    { label: 'Python', language: 'python', code: python },
    { label: 'cURL', language: 'bash', code: curlCmd },
    { label: 'TypeScript', language: 'typescript', code: ts },
  ]
}

/* ----------------------------------------------------------------------
 * Primitive components
 * -------------------------------------------------------------------- */
function CodeView({ code, language }: { code: string; language: string }) {
  return (
    <Highlight theme={themes.vsDark} code={code} language={language}>
      {({ style, tokens, getLineProps, getTokenProps }) => (
        <pre
          className="overflow-x-auto p-4 font-mono text-[12.5px] leading-6"
          style={{ ...style, background: 'transparent', margin: 0 }}
        >
          {tokens.map((line, i) => (
            <div key={i} {...getLineProps({ line })}>
              {line.map((token, k) => (
                <span key={k} {...getTokenProps({ token })} />
              ))}
            </div>
          ))}
        </pre>
      )}
    </Highlight>
  )
}

// CodeExample renders one or more code tabs with a per-block copy button.
function CodeExample({ tabs }: { tabs: CodeTab[] }) {
  const [active, setActive] = useState(0)
  const [copied, setCopied] = useState(false)
  const tab = tabs[Math.min(active, tabs.length - 1)]
  const multi = tabs.length > 1

  async function onCopy() {
    if (await copyText(tab.code)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    }
  }

  return (
    <div className="my-4 overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900">
      <div className="flex items-center justify-between border-b border-zinc-800 bg-zinc-900/80">
        {multi ? (
          <div className="flex">
            {tabs.map((t, i) => (
              <button
                key={t.label}
                onClick={() => {
                  setActive(i)
                  setCopied(false)
                }}
                className={`border-b-2 px-3.5 py-2 text-xs font-medium transition-colors ${
                  i === active
                    ? 'border-teal-400 text-teal-300'
                    : 'border-transparent text-zinc-500 hover:text-zinc-300'
                }`}
              >
                {t.label}
              </button>
            ))}
          </div>
        ) : (
          <span className="px-3.5 py-2 font-mono text-[11px] uppercase tracking-wider text-zinc-500">
            {tab.label}
          </span>
        )}
        <button
          onClick={onCopy}
          className="mr-2 flex items-center gap-1.5 rounded-md px-2 py-1 text-[11px] text-zinc-400 transition-colors hover:bg-zinc-800 hover:text-zinc-200"
        >
          {copied ? <Check size={12} className="text-teal-400" /> : <Copy size={12} />}
          {copied ? 'Copied!' : 'Copy'}
        </button>
      </div>
      <CodeView code={tab.code} language={tab.language} />
    </div>
  )
}

const methodStyles: Record<Method, string> = {
  GET: 'bg-green-500/10 text-green-400',
  POST: 'bg-teal-500/10 text-teal-400',
  PUT: 'bg-amber-500/10 text-amber-400',
  DELETE: 'bg-red-500/10 text-red-400',
}

function MethodPill({ method }: { method: Method }) {
  return (
    <span
      className={`rounded-full font-mono text-[11px] font-semibold ${methodStyles[method]}`}
      style={{ padding: '0.5em 0.75em' }}
    >
      {method}
    </span>
  )
}

function StatusPill({ status }: { status: string }) {
  const n = parseInt(status, 10)
  const tone =
    n < 300
      ? 'bg-green-500/10 text-green-400'
      : n < 400
        ? 'bg-amber-500/10 text-amber-400'
        : 'bg-red-500/10 text-red-400'
  return (
    <span className={`rounded-full px-2.5 py-0.5 font-mono text-[11px] font-semibold ${tone}`}>
      {status}
    </span>
  )
}

function ParamTable({ params }: { params: Param[] }) {
  return (
    <div className="my-4 overflow-x-auto rounded-lg border border-zinc-800">
      <table className="w-full text-left text-sm">
        <thead>
          <tr className="border-b border-zinc-800 bg-zinc-900/60 text-[11px] uppercase tracking-wider text-zinc-500">
            <th className="px-3 py-2 font-medium">Name</th>
            <th className="px-3 py-2 font-medium">Type</th>
            <th className="px-3 py-2 font-medium">Required</th>
            <th className="px-3 py-2 font-medium">Description</th>
          </tr>
        </thead>
        <tbody>
          {params.map((p) => (
            <tr key={p.name} className="border-b border-zinc-800/60 last:border-0">
              <td className="whitespace-nowrap px-3 py-2 font-mono text-[12.5px] text-teal-300">
                {p.name}
              </td>
              <td className="whitespace-nowrap px-3 py-2 font-mono text-[12px] text-zinc-400">
                {p.type}
              </td>
              <td className="px-3 py-2 text-xs">
                {p.required ? (
                  <span className="text-amber-400">yes</span>
                ) : (
                  <span className="text-zinc-500">no</span>
                )}
              </td>
              <td className="px-3 py-2 text-zinc-400">{p.description}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// EndpointBlock renders one API endpoint following the standard format:
// method pill + path, description, params, tabbed examples, response.
function EndpointBlock({ ep }: { ep: Endpoint }) {
  return (
    <div
      id={ep.id}
      data-anchor
      className="scroll-mt-24 border-t border-zinc-800/70 pt-8 first:border-0 first:pt-2"
    >
      <div className="flex flex-wrap items-center gap-3">
        <MethodPill method={ep.method} />
        <code className="font-mono text-[15px] text-zinc-100">{ep.path}</code>
        {ep.planned && (
          <span className="rounded-full bg-zinc-800 px-2.5 py-0.5 text-[10px] font-medium uppercase tracking-wider text-zinc-400">
            Planned
          </span>
        )}
      </div>
      <p className="mt-3 text-sm leading-relaxed text-zinc-400">{ep.description}</p>
      {ep.params && ep.params.length > 0 && <ParamTable params={ep.params} />}
      {ep.examples && <CodeExample tabs={ep.examples} />}
      {ep.response && (
        <div className="mt-4">
          <div className="mb-2 flex items-center gap-2">
            <span className="font-mono text-[11px] uppercase tracking-wider text-zinc-500">
              Response
            </span>
            <StatusPill status={ep.response.status} />
          </div>
          <CodeExample tabs={[{ label: 'JSON', language: 'json', code: ep.response.body }]} />
        </div>
      )}
    </div>
  )
}

function H3({ id, children }: { id: string; children: ReactNode }) {
  return (
    <h3
      id={id}
      data-anchor
      className="scroll-mt-24 mb-2 mt-8 text-base font-semibold text-zinc-100"
    >
      {children}
    </h3>
  )
}

function P({ children }: { children: ReactNode }) {
  return <p className="my-3 text-sm leading-relaxed text-zinc-400">{children}</p>
}

function Lead({ children }: { children: ReactNode }) {
  return <p className="mb-5 text-[15px] leading-relaxed text-zinc-300">{children}</p>
}

// StateMachine draws the sandbox lifecycle as CSS pills, no images.
function StateMachine() {
  const states = ['PENDING', 'CREATING', 'RUNNING', 'STOPPING', 'STOPPED', 'DESTROYING', 'DESTROYED']
  return (
    <div className="my-5 rounded-lg border border-zinc-800 bg-zinc-900/50 p-5">
      <div className="flex flex-wrap items-center gap-y-3">
        {states.map((s, i) => (
          <div key={s} className="flex items-center">
            <span className="rounded-md border border-zinc-700 bg-zinc-800/80 px-2.5 py-1 font-mono text-[11px] text-zinc-200">
              {s}
            </span>
            {i < states.length - 1 && (
              <ArrowRight size={14} className="mx-1.5 shrink-0 text-zinc-600" />
            )}
          </div>
        ))}
      </div>
      <div className="mt-4 flex items-center gap-2 border-t border-zinc-800/70 pt-4">
        <CornerDownRight size={14} className="shrink-0 text-zinc-600" />
        <span className="text-xs text-zinc-500">Any state can transition to</span>
        <span className="rounded-md border border-red-500/30 bg-red-500/10 px-2.5 py-1 font-mono text-[11px] text-red-400">
          ERROR
        </span>
      </div>
    </div>
  )
}

/* ----------------------------------------------------------------------
 * Section bodies
 * -------------------------------------------------------------------- */
function QuickstartBody() {
  return (
    <>
      <Lead>Go from zero to a running, hardware-isolated sandbox in under a minute.</Lead>

      <H3 id="qs-api-key">1. Get an API key</H3>
      <P>
        Create a key on the{' '}
        <Link to="/api-keys" className="text-teal-400 hover:underline">
          API Keys
        </Link>{' '}
        page. Keys are shown once at creation — copy it somewhere safe.
      </P>

      <H3 id="qs-install">2. Install the SDK</H3>
      <P>The Python SDK ships the client library and the `vajra` CLI in a single package.</P>
      <CodeExample tabs={[{ label: 'bash', language: 'bash', code: 'pip install vajra' }]} />

      <H3 id="qs-first-sandbox">3. Create your first sandbox</H3>
      <P>
        Point the client at your key and create a sandbox from a template. The call returns as soon
        as a microVM is assigned — typically a sub-30ms pool hit.
      </P>
      <CodeExample
        tabs={apiTabs(
          `import vajra

client = vajra.Client(api_key="${KEY}")

sandbox = client.sandboxes.create(
    template_id="tpl-ubuntu-noble",
    vcpus=2,
    memory_mb=1024,
)
print(sandbox.id)`,
          curl(
            'POST',
            '/v1/sandboxes',
            '{\n    "template_id": "tpl-ubuntu-noble",\n    "vcpus": 2,\n    "memory_mb": 1024\n  }',
          ),
          `// Planned for v2 — Python and cURL work today.
import { Vajra } from "vajra"

const client = new Vajra({ apiKey: "${KEY}" })

const sandbox = await client.sandboxes.create({
  templateId: "tpl-ubuntu-noble",
  vcpus: 2,
  memoryMb: 1024,
})
console.log(sandbox.id)`,
        )}
      />
    </>
  )
}

function AuthBody() {
  return (
    <>
      <Lead>
        Vajra authenticates every API request with a bearer token. There are no sessions or cookies
        on the API surface — just your key.
      </Lead>

      <H3 id="auth-key">API keys</H3>
      <P>
        Generate keys from the{' '}
        <Link to="/api-keys" className="text-teal-400 hover:underline">
          API Keys
        </Link>{' '}
        page in the dashboard. Live keys are prefixed `vj_live_`. A key inherits the permissions of
        the account that created it.
      </P>

      <H3 id="auth-header">Header format</H3>
      <P>Pass the key in the Authorization header on every request:</P>
      <CodeExample
        tabs={[
          {
            label: 'bash',
            language: 'bash',
            code: `curl ${HOST}/v1/sandboxes \\
  -H "Authorization: Bearer ${KEY}"`,
          },
        ]}
      />

      <H3 id="auth-rate-limits">Rate limits</H3>
      <P>
        The authenticated API is limited to <strong className="text-zinc-200">200 requests per
        minute</strong> per account. Exceeding the limit returns `429 Too Many Requests` — back off
        and retry with exponential delay.
      </P>

      <H3 id="auth-practices">Best practices</H3>
      <P>
        Load keys from environment variables — never hardcode them in source or commit them to
        version control. Rotate keys periodically, and issue a separate key per environment or
        service so a leaked key can be revoked without disrupting everything else.
      </P>
    </>
  )
}

const sandboxEndpoints: Endpoint[] = [
  {
    id: 'create-sandbox',
    method: 'POST',
    path: '/v1/sandboxes',
    description:
      'Creates a sandbox from a template and schedules it onto a node. Returns immediately in state CREATING — poll GET /v1/sandboxes/{id} or subscribe to the sandbox.running webhook to learn when it is ready.',
    params: [
      { name: 'name', type: 'string', required: false, description: 'Display name, defaults to the sandbox ID' },
      { name: 'template_id', type: 'uuid', required: true, description: 'UUID of the template to boot' },
      { name: 'vcpus', type: 'integer', required: false, description: 'Number of vCPUs (default 1, max 8)' },
      { name: 'memory_mb', type: 'integer', required: false, description: 'RAM in MB (default 512, max 16384)' },
      { name: 'git_url', type: 'string', required: false, description: 'Optional repo to clone into /workspace' },
      { name: 'git_branch', type: 'string', required: false, description: 'Branch name, default "main"' },
    ],
    examples: apiTabs(
      `sandbox = client.sandboxes.create(
    template_id="tpl-ubuntu-noble",
    vcpus=2,
    memory_mb=1024,
)
print(sandbox.id)`,
      curl(
        'POST',
        '/v1/sandboxes',
        '{\n    "template_id": "tpl-ubuntu-noble",\n    "vcpus": 2,\n    "memory_mb": 1024\n  }',
      ),
      `// Planned for v2 — Python and cURL work today.
const sandbox = await client.sandboxes.create({
  templateId: "tpl-ubuntu-noble",
  vcpus: 2,
  memoryMb: 1024,
})`,
    ),
    response: {
      status: '201 Created',
      body: `{
  "id": "sb_4f8f7b3f51c1d28a",
  "state": "CREATING",
  "template_id": "tpl-ubuntu-noble",
  "vcpus": 2,
  "memory_mb": 1024,
  "created_at": "2026-05-17T19:00:00Z"
}`,
    },
  },
  {
    id: 'list-sandboxes',
    method: 'GET',
    path: '/v1/sandboxes',
    description: 'Lists every sandbox owned by the account, newest first.',
    params: [
      { name: 'state', type: 'string', required: false, description: 'Filter by lifecycle state, e.g. RUNNING' },
      { name: 'limit', type: 'integer', required: false, description: 'Maximum rows to return (default 50)' },
    ],
    examples: apiTabs(
      `for s in client.sandboxes.list():
    print(s.id, s.state)`,
      curl('GET', '/v1/sandboxes'),
      `// Planned for v2 — Python and cURL work today.
const sandboxes = await client.sandboxes.list()`,
    ),
    response: {
      status: '200 OK',
      body: `[
  {
    "id": "sb_4f8f7b3f51c1d28a",
    "state": "RUNNING",
    "template_id": "tpl-ubuntu-noble",
    "created_at": "2026-05-17T19:00:00Z"
  }
]`,
    },
  },
  {
    id: 'get-sandbox',
    method: 'GET',
    path: '/v1/sandboxes/{id}',
    description: 'Fetches the full record for a single sandbox, including its current state and the node it landed on.',
    examples: apiTabs(
      `sandbox = client.sandboxes.get("sb_4f8f7b3f51c1d28a")
print(sandbox.state)`,
      curl('GET', '/v1/sandboxes/sb_4f8f7b3f51c1d28a'),
      `// Planned for v2 — Python and cURL work today.
const sandbox = await client.sandboxes.get("sb_4f8f7b3f51c1d28a")`,
    ),
    response: {
      status: '200 OK',
      body: `{
  "id": "sb_4f8f7b3f51c1d28a",
  "state": "RUNNING",
  "template_id": "tpl-ubuntu-noble",
  "vcpus": 2,
  "memory_mb": 1024,
  "node_id": "node_sg_01",
  "boot_ms": 28,
  "created_at": "2026-05-17T19:00:00Z"
}`,
    },
  },
  {
    id: 'start-sandbox',
    method: 'POST',
    path: '/v1/sandboxes/{id}/start',
    description: 'Boots a stopped sandbox. If a memory snapshot exists it is restored, otherwise the sandbox cold-boots from its template.',
    examples: apiTabs(
      `client.sandboxes.start("sb_4f8f7b3f51c1d28a")`,
      curl('POST', '/v1/sandboxes/sb_4f8f7b3f51c1d28a/start'),
      `// Planned for v2 — Python and cURL work today.
await client.sandboxes.start("sb_4f8f7b3f51c1d28a")`,
    ),
    response: {
      status: '200 OK',
      body: `{
  "id": "sb_4f8f7b3f51c1d28a",
  "state": "RUNNING"
}`,
    },
  },
  {
    id: 'stop-sandbox',
    method: 'POST',
    path: '/v1/sandboxes/{id}/stop',
    description: 'Gracefully shuts down a running sandbox. The disk is preserved, so the sandbox can be started again later.',
    examples: apiTabs(
      `client.sandboxes.stop("sb_4f8f7b3f51c1d28a")`,
      curl('POST', '/v1/sandboxes/sb_4f8f7b3f51c1d28a/stop'),
      `// Planned for v2 — Python and cURL work today.
await client.sandboxes.stop("sb_4f8f7b3f51c1d28a")`,
    ),
    response: {
      status: '200 OK',
      body: `{
  "id": "sb_4f8f7b3f51c1d28a",
  "state": "STOPPED"
}`,
    },
  },
  {
    id: 'destroy-sandbox',
    method: 'DELETE',
    path: '/v1/sandboxes/{id}',
    description: 'Permanently destroys a sandbox and frees its resources. This cannot be undone — snapshot the sandbox first if you need to keep its state.',
    examples: apiTabs(
      `client.sandboxes.destroy("sb_4f8f7b3f51c1d28a")`,
      curl('DELETE', '/v1/sandboxes/sb_4f8f7b3f51c1d28a'),
      `// Planned for v2 — Python and cURL work today.
await client.sandboxes.destroy("sb_4f8f7b3f51c1d28a")`,
    ),
    response: {
      status: '200 OK',
      body: `{
  "id": "sb_4f8f7b3f51c1d28a",
  "state": "DESTROYING"
}`,
    },
  },
]

function SandboxesBody() {
  return (
    <>
      <Lead>
        A sandbox is a single isolated Linux microVM. Each one runs its own kernel under Cloud
        Hypervisor — stronger isolation than a container, with snapshot-restore boot times.
      </Lead>

      <H3 id="sandbox-states">Lifecycle</H3>
      <P>Every sandbox moves through a fixed state machine. Mutating endpoints advance it:</P>
      <StateMachine />

      <div className="space-y-8">
        {sandboxEndpoints.map((ep) => (
          <EndpointBlock key={ep.id} ep={ep} />
        ))}
      </div>
    </>
  )
}

const templateEndpoints: Endpoint[] = [
  {
    id: 'list-templates',
    method: 'GET',
    path: '/v1/templates',
    description: 'Lists templates available to your account — both the public base images and any custom templates you have built.',
    examples: apiTabs(
      `for t in client.templates.list():
    print(t.id, t.name, t.hash)`,
      curl('GET', '/v1/templates'),
      `// Planned for v2 — Python and cURL work today.
const templates = await client.templates.list()`,
    ),
    response: {
      status: '200 OK',
      body: `[
  {
    "id": "tpl-ubuntu-noble",
    "name": "ubuntu-noble",
    "hash": "sha256:e3584e4b74b9a9c5...945e26ff",
    "public": true
  }
]`,
    },
  },
  {
    id: 'build-template',
    method: 'POST',
    path: '/v1/templates/build',
    description: 'Builds a custom template by deriving from a base image and running setup commands inside it. Returns a build record — poll GET /v1/templates/builds/{id} until it completes.',
    params: [
      { name: 'name', type: 'string', required: true, description: 'Name for the new template' },
      { name: 'base_template_id', type: 'uuid', required: true, description: 'Template to derive from, e.g. ubuntu-noble' },
      { name: 'setup_commands', type: 'string[]', required: true, description: 'Shell commands run in order during the build' },
    ],
    examples: apiTabs(
      `build = client.templates.build(
    name="python-ml",
    base_template_id="tpl-ubuntu-noble",
    setup_commands=[
        "apt-get update",
        "pip install numpy pandas",
    ],
)
print(build.id, build.status)`,
      curl(
        'POST',
        '/v1/templates/build',
        '{\n    "name": "python-ml",\n    "base_template_id": "tpl-ubuntu-noble",\n    "setup_commands": ["apt-get update", "pip install numpy pandas"]\n  }',
      ),
      `// Planned for v2 — Python and cURL work today.
const build = await client.templates.build({
  name: "python-ml",
  baseTemplateId: "tpl-ubuntu-noble",
  setupCommands: ["apt-get update", "pip install numpy pandas"],
})`,
    ),
    response: {
      status: '201 Created',
      body: `{
  "build_id": "bld_91a2c0f3",
  "name": "python-ml",
  "status": "PENDING"
}`,
    },
  },
  {
    id: 'template-from-snapshot',
    method: 'POST',
    path: '/v1/templates/from-snapshot',
    planned: true,
    description:
      'Planned for v2. Today, promote a snapshot straight to a reusable template with POST /v1/snapshots/{id}/promote — see the Snapshots section below.',
  },
]

function TemplatesBody() {
  return (
    <>
      <Lead>
        Templates are the immutable root filesystems sandboxes boot from. Vajra ships a public
        `ubuntu-noble` base, and you can build your own on top of it.
      </Lead>

      <H3 id="templates-hashes">Content-addressable hashes</H3>
      <P>
        Every template is identified by the SHA-256 hash of its root filesystem, e.g.
        `sha256:e3584e4b74b9a9c5…945e26ff`. Identical filesystems collapse to the same hash, so a
        template built once is cached and reused across every node — there is no redundant storage
        or re-distribution. Agents pull a missing template on demand from the master the first time
        it is scheduled.
      </P>

      <div className="space-y-8">
        {templateEndpoints.map((ep) => (
          <EndpointBlock key={ep.id} ep={ep} />
        ))}
      </div>
    </>
  )
}

const execEndpoint: Endpoint = {
  id: 'exec-command',
  method: 'POST',
  path: '/v1/sandboxes/{id}/exec',
  description: 'Runs a command inside a running sandbox and returns its exit code, stdout, and stderr once it finishes.',
  params: [
    { name: 'command', type: 'string', required: true, description: 'Shell command to run' },
    { name: 'workdir', type: 'string', required: false, description: 'Working directory, default /workspace' },
    { name: 'timeout_seconds', type: 'integer', required: false, description: 'Kill the command after this many seconds (default 60)' },
    { name: 'env', type: 'object', required: false, description: 'Extra environment variables for this command' },
  ],
  examples: apiTabs(
    `result = client.sandboxes.exec(
    "sb_4f8f7b3f51c1d28a",
    "python3 main.py",
)
print(result.exit_code, result.stdout)`,
    curl(
      'POST',
      '/v1/sandboxes/sb_4f8f7b3f51c1d28a/exec',
      '{\n    "command": "python3 main.py",\n    "workdir": "/workspace"\n  }',
    ),
    `// Planned for v2 — Python and cURL work today.
const result = await client.sandboxes.exec(
  "sb_4f8f7b3f51c1d28a",
  { command: "python3 main.py" },
)`,
  ),
  response: {
    status: '200 OK',
    body: `{
  "exit_code": 0,
  "stdout": "hello from the sandbox\\n",
  "stderr": "",
  "duration_ms": 142
}`,
  },
}

function ExecutionBody() {
  return (
    <>
      <Lead>Run commands inside a sandbox and stream the results back over the API.</Lead>

      <div className="space-y-8">
        <EndpointBlock ep={execEndpoint} />
      </div>

      <H3 id="exec-workdir">Working directory</H3>
      <P>
        Commands default to `/workspace`, the writable directory that persists for the life of the
        sandbox. Files written there survive across exec calls and across stop/start cycles, and
        anything cloned from `git_url` lands here. Override it per call with the `workdir`
        parameter.
      </P>

      <H3 id="exec-timeouts">Timeouts</H3>
      <P>
        Each command runs with a 60-second timeout by default. A command that exceeds its
        `timeout_seconds` is killed and the response reports a non-zero exit code. Raise the timeout
        for long-running builds, and prefer backgrounding truly long jobs over holding the request
        open.
      </P>
    </>
  )
}

const fileEndpoints: Endpoint[] = [
  {
    id: 'upload-file',
    method: 'POST',
    path: '/v1/sandboxes/{id}/files/upload',
    description: 'Uploads a file into the sandbox filesystem. The request is multipart/form-data with a destination path and the file contents.',
    params: [
      { name: 'path', type: 'string', required: true, description: 'Absolute destination path inside the sandbox' },
      { name: 'file', type: 'file', required: true, description: 'The file contents (multipart field)' },
    ],
    examples: apiTabs(
      `client.sandboxes.files.upload(
    "sb_4f8f7b3f51c1d28a",
    path="/workspace/main.py",
    local="./main.py",
)`,
      `curl -X POST ${HOST}/v1/sandboxes/sb_4f8f7b3f51c1d28a/files/upload \\
  -H "Authorization: Bearer ${KEY}" \\
  -F "path=/workspace/main.py" \\
  -F "file=@./main.py"`,
      `// Planned for v2 — Python and cURL work today.
await client.sandboxes.files.upload("sb_4f8f7b3f51c1d28a", {
  path: "/workspace/main.py",
  file: localFile,
})`,
    ),
    response: { status: '200 OK', body: `{
  "path": "/workspace/main.py",
  "size": 482
}` },
  },
  {
    id: 'download-file',
    method: 'GET',
    path: '/v1/sandboxes/{id}/files/download',
    description: 'Streams a file out of the sandbox. The response body is the raw file content.',
    params: [{ name: 'path', type: 'string', required: true, description: 'Absolute path of the file to download' }],
    examples: apiTabs(
      `data = client.sandboxes.files.download(
    "sb_4f8f7b3f51c1d28a",
    path="/workspace/out.txt",
)`,
      `curl -X GET "${HOST}/v1/sandboxes/sb_4f8f7b3f51c1d28a/files/download?path=/workspace/out.txt" \\
  -H "Authorization: Bearer ${KEY}" -o out.txt`,
      `// Planned for v2 — Python and cURL work today.
const blob = await client.sandboxes.files.download(
  "sb_4f8f7b3f51c1d28a",
  { path: "/workspace/out.txt" },
)`,
    ),
  },
  {
    id: 'list-files',
    method: 'GET',
    path: '/v1/sandboxes/{id}/files/list',
    description: 'Lists the entries in a directory inside the sandbox.',
    params: [{ name: 'dir', type: 'string', required: false, description: 'Directory to list, default /workspace' }],
    examples: apiTabs(
      `entries = client.sandboxes.files.list(
    "sb_4f8f7b3f51c1d28a",
    dir="/workspace",
)`,
      `curl -X GET "${HOST}/v1/sandboxes/sb_4f8f7b3f51c1d28a/files/list?dir=/workspace" \\
  -H "Authorization: Bearer ${KEY}"`,
      `// Planned for v2 — Python and cURL work today.
const entries = await client.sandboxes.files.list(
  "sb_4f8f7b3f51c1d28a",
  { dir: "/workspace" },
)`,
    ),
    response: {
      status: '200 OK',
      body: `[
  { "name": "main.py", "size": 482, "is_dir": false },
  { "name": "data", "size": 0, "is_dir": true }
]`,
    },
  },
  {
    id: 'delete-file',
    method: 'DELETE',
    path: '/v1/sandboxes/{id}/files',
    description: 'Deletes a file inside the sandbox.',
    params: [{ name: 'path', type: 'string', required: true, description: 'Absolute path of the file to delete' }],
    examples: apiTabs(
      `client.sandboxes.files.delete(
    "sb_4f8f7b3f51c1d28a",
    path="/workspace/old.log",
)`,
      `curl -X DELETE "${HOST}/v1/sandboxes/sb_4f8f7b3f51c1d28a/files?path=/workspace/old.log" \\
  -H "Authorization: Bearer ${KEY}"`,
      `// Planned for v2 — Python and cURL work today.
await client.sandboxes.files.delete(
  "sb_4f8f7b3f51c1d28a",
  { path: "/workspace/old.log" },
)`,
    ),
    response: { status: '200 OK', body: `{ "deleted": "/workspace/old.log" }` },
  },
]

function FilesBody() {
  return (
    <>
      <Lead>
        Move files in and out of a sandbox without opening a shell. All paths are absolute; the
        writable workspace lives at `/workspace`.
      </Lead>
      <div className="space-y-8">
        {fileEndpoints.map((ep) => (
          <EndpointBlock key={ep.id} ep={ep} />
        ))}
      </div>
    </>
  )
}

const snapshotEndpoints: Endpoint[] = [
  {
    id: 'create-snapshot',
    method: 'POST',
    path: '/v1/sandboxes/{id}/snapshot',
    description: 'Captures the full memory and disk state of a running sandbox into a snapshot that can be restored later.',
    params: [{ name: 'name', type: 'string', required: false, description: 'Display name for the snapshot' }],
    examples: apiTabs(
      `snap = client.sandboxes.snapshot(
    "sb_4f8f7b3f51c1d28a",
    name="after-build",
)
print(snap.id)`,
      curl('POST', '/v1/sandboxes/sb_4f8f7b3f51c1d28a/snapshot', '{ "name": "after-build" }'),
      `// Planned for v2 — Python and cURL work today.
const snap = await client.sandboxes.snapshot(
  "sb_4f8f7b3f51c1d28a",
  { name: "after-build" },
)`,
    ),
    response: {
      status: '201 Created',
      body: `{
  "id": "snap_7c2e91ad",
  "sandbox_id": "sb_4f8f7b3f51c1d28a",
  "name": "after-build",
  "created_at": "2026-05-17T19:10:00Z"
}`,
    },
  },
  {
    id: 'list-snapshots',
    method: 'GET',
    path: '/v1/snapshots',
    description: 'Lists every snapshot owned by your account.',
    examples: apiTabs(
      `for s in client.snapshots.list():
    print(s.id, s.name)`,
      curl('GET', '/v1/snapshots'),
      `// Planned for v2 — Python and cURL work today.
const snapshots = await client.snapshots.list()`,
    ),
    response: {
      status: '200 OK',
      body: `[
  {
    "id": "snap_7c2e91ad",
    "name": "after-build",
    "created_at": "2026-05-17T19:10:00Z"
  }
]`,
    },
  },
  {
    id: 'restore-snapshot',
    method: 'POST',
    path: '/v1/snapshots/{id}/restore',
    description: 'Restores a snapshot into a brand-new sandbox. The original sandbox is untouched, so a snapshot can be restored any number of times.',
    examples: apiTabs(
      `sandbox = client.snapshots.restore("snap_7c2e91ad")
print(sandbox.id)`,
      curl('POST', '/v1/snapshots/snap_7c2e91ad/restore'),
      `// Planned for v2 — Python and cURL work today.
const sandbox = await client.snapshots.restore("snap_7c2e91ad")`,
    ),
    response: {
      status: '201 Created',
      body: `{
  "id": "sb_b91c4d20aa3f6e17",
  "state": "RUNNING",
  "restored_from": "snap_7c2e91ad"
}`,
    },
  },
  {
    id: 'promote-snapshot',
    method: 'POST',
    path: '/v1/snapshots/{id}/promote',
    description: 'Promotes a snapshot into a reusable template. New sandboxes can then boot from it like any other template.',
    params: [{ name: 'name', type: 'string', required: true, description: 'Name for the resulting template' }],
    examples: apiTabs(
      `tpl = client.snapshots.promote(
    "snap_7c2e91ad",
    name="prebuilt-env",
)
print(tpl.id)`,
      curl('POST', '/v1/snapshots/snap_7c2e91ad/promote', '{ "name": "prebuilt-env" }'),
      `// Planned for v2 — Python and cURL work today.
const tpl = await client.snapshots.promote(
  "snap_7c2e91ad",
  { name: "prebuilt-env" },
)`,
    ),
    response: {
      status: '201 Created',
      body: `{
  "id": "tpl-prebuilt-env",
  "name": "prebuilt-env",
  "hash": "sha256:9d41b8f0c2a7e3..."
}`,
    },
  },
]

function SnapshotsBody() {
  return (
    <>
      <Lead>
        Snapshots freeze a running sandbox — memory and disk — so you can restore it later in
        milliseconds. They are the building block for stateful, resumable work.
      </Lead>
      <div className="space-y-8">
        {snapshotEndpoints.map((ep) => (
          <EndpointBlock key={ep.id} ep={ep} />
        ))}
      </div>
    </>
  )
}

const createWebhookEndpoint: Endpoint = {
  id: 'create-webhook',
  method: 'POST',
  path: '/v1/webhooks',
  description: 'Registers a webhook endpoint. Vajra POSTs a JSON event to the URL whenever a subscribed event fires.',
  params: [
    { name: 'url', type: 'string', required: true, description: 'HTTPS endpoint that will receive events' },
    { name: 'events', type: 'string[]', required: false, description: 'Event types to subscribe to; defaults to all' },
    { name: 'secret', type: 'string', required: false, description: 'Signing secret for HMAC; generated if omitted' },
  ],
  examples: apiTabs(
    `hook = client.webhooks.create(
    url="https://example.com/vajra",
    events=["sandbox.running", "sandbox.error"],
)
print(hook.id, hook.secret)`,
    curl(
      'POST',
      '/v1/webhooks',
      '{\n    "url": "https://example.com/vajra",\n    "events": ["sandbox.running", "sandbox.error"]\n  }',
    ),
    `// Planned for v2 — Python and cURL work today.
const hook = await client.webhooks.create({
  url: "https://example.com/vajra",
  events: ["sandbox.running", "sandbox.error"],
})`,
  ),
  response: {
    status: '201 Created',
    body: `{
  "id": "wh_3f8a21cd",
  "url": "https://example.com/vajra",
  "events": ["sandbox.running", "sandbox.error"],
  "secret": "whsec_a91c..."
}`,
  },
}

const webhookEvents = [
  ['sandbox.created', 'A sandbox record was created'],
  ['sandbox.running', 'A sandbox finished booting and is ready'],
  ['sandbox.destroyed', 'A sandbox was destroyed'],
  ['sandbox.error', 'A sandbox entered the ERROR state'],
  ['payment.succeeded', 'A credit purchase completed'],
]

function WebhooksBody() {
  return (
    <>
      <Lead>
        Webhooks push lifecycle events to your services so you do not have to poll. Configure them
        from the dashboard or the API.
      </Lead>

      <H3 id="webhooks-config">Configuration</H3>
      <P>
        Add an endpoint on the{' '}
        <Link to="/webhooks" className="text-teal-400 hover:underline">
          Webhooks
        </Link>{' '}
        page, or register one programmatically with the endpoint below.
      </P>

      <div className="space-y-8">
        <EndpointBlock ep={createWebhookEndpoint} />
      </div>

      <H3 id="webhook-events">Event types</H3>
      <div className="my-4 overflow-x-auto rounded-lg border border-zinc-800">
        <table className="w-full text-left text-sm">
          <thead>
            <tr className="border-b border-zinc-800 bg-zinc-900/60 text-[11px] uppercase tracking-wider text-zinc-500">
              <th className="px-3 py-2 font-medium">Event</th>
              <th className="px-3 py-2 font-medium">Fires when</th>
            </tr>
          </thead>
          <tbody>
            {webhookEvents.map(([ev, desc]) => (
              <tr key={ev} className="border-b border-zinc-800/60 last:border-0">
                <td className="whitespace-nowrap px-3 py-2 font-mono text-[12.5px] text-teal-300">
                  {ev}
                </td>
                <td className="px-3 py-2 text-zinc-400">{desc}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <H3 id="webhook-signatures">Signature verification</H3>
      <P>
        Every delivery carries an `X-Vajra-Signature` header — the HMAC-SHA256 of the raw request
        body, keyed with your webhook's signing secret. Recompute it and compare in constant time
        before trusting the payload.
      </P>
      <CodeExample
        tabs={[
          {
            label: 'Python',
            language: 'python',
            code: `import hashlib
import hmac

def verify(body: bytes, signature: str, secret: str) -> bool:
    expected = hmac.new(
        secret.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)`,
          },
        ]}
      />

      <H3 id="webhook-retries">Retry policy</H3>
      <P>
        A delivery is considered failed if your endpoint returns a non-2xx status or times out.
        Failed deliveries are retried up to 3 times with exponential backoff — roughly 1s, 4s, then
        16s. After the final attempt the event is dropped.
      </P>
    </>
  )
}

function SdksBody() {
  return (
    <>
      <Lead>Talk to Vajra from Python, the command line, or — soon — TypeScript.</Lead>

      <H3 id="sdk-python">Python SDK</H3>
      <P>Install from PyPI. The package includes both the client library and the CLI.</P>
      <CodeExample tabs={[{ label: 'bash', language: 'bash', code: 'pip install vajra' }]} />
      <CodeExample
        tabs={[
          {
            label: 'Python',
            language: 'python',
            code: `import vajra

client = vajra.Client(api_key="${KEY}")

sandbox = client.sandboxes.create(template_id="tpl-ubuntu-noble")
result = client.sandboxes.exec(sandbox.id, "echo hello")
print(result.stdout)`,
          },
        ]}
      />

      <H3 id="sdk-cli">CLI</H3>
      <P>
        The `vajra` command ships with the Python package. Authenticate by exporting
        `VAJRA_API_KEY`, then drive sandboxes straight from the shell.
      </P>
      <CodeExample
        tabs={[
          {
            label: 'bash',
            language: 'bash',
            code: `export VAJRA_API_KEY="${KEY}"

vajra sandbox create --template tpl-ubuntu-noble
vajra sandbox list
vajra sandbox exec sb_4f8f7b3f51c1d28a "ls /workspace"
vajra sandbox destroy sb_4f8f7b3f51c1d28a`,
          },
        ]}
      />

      <H3 id="sdk-typescript">TypeScript SDK</H3>
      <div className="my-4 rounded-lg border border-zinc-800 bg-zinc-900/50 p-4">
        <div className="flex items-center gap-2">
          <span className="rounded-full bg-zinc-800 px-2.5 py-0.5 text-[10px] font-medium uppercase tracking-wider text-zinc-400">
            Planned for v2
          </span>
        </div>
        <p className="mt-2 text-sm leading-relaxed text-zinc-400">
          A first-class TypeScript SDK is on the roadmap. Until it ships, use the Python SDK or call
          the REST API directly — every endpoint works today over plain HTTP.
        </p>
      </div>
    </>
  )
}

const errorRows: [string, string, string][] = [
  ['200', 'Success', 'Request completed'],
  ['201', 'Created', 'After POST /v1/sandboxes'],
  ['400', 'Bad request', 'Invalid template_id'],
  ['401', 'Unauthorized', 'Missing or invalid API key'],
  ['403', 'Forbidden', 'API key valid but no permission'],
  ['404', 'Not found', "Sandbox doesn't exist"],
  ['409', 'Conflict', 'Sandbox in wrong state for action'],
  ['429', 'Rate limited', 'Too many requests'],
  ['500', 'Server error', 'Contact support'],
]

function ErrorsBody() {
  return (
    <>
      <Lead>
        Vajra uses conventional HTTP status codes and returns a structured JSON body on every
        error.
      </Lead>

      <H3 id="error-codes">Status codes</H3>
      <div className="my-4 overflow-x-auto rounded-lg border border-zinc-800">
        <table className="w-full text-left text-sm">
          <thead>
            <tr className="border-b border-zinc-800 bg-zinc-900/60 text-[11px] uppercase tracking-wider text-zinc-500">
              <th className="px-3 py-2 font-medium">Status</th>
              <th className="px-3 py-2 font-medium">Meaning</th>
              <th className="px-3 py-2 font-medium">Example</th>
            </tr>
          </thead>
          <tbody>
            {errorRows.map(([status, meaning, example]) => (
              <tr key={status} className="border-b border-zinc-800/60 last:border-0">
                <td className="px-3 py-2">
                  <StatusPill status={status} />
                </td>
                <td className="whitespace-nowrap px-3 py-2 text-zinc-200">{meaning}</td>
                <td className="px-3 py-2 text-zinc-400">{example}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <H3 id="error-shape">Error response shape</H3>
      <P>Every 4xx and 5xx response carries the same JSON envelope:</P>
      <CodeExample
        tabs={[
          {
            label: 'JSON',
            language: 'json',
            code: `{
  "error": "human-readable message",
  "code": "machine_readable_code",
  "details": {}
}`,
          },
        ]}
      />
    </>
  )
}

const bestPractices: [string, string, string][] = [
  [
    'bp-caching',
    'Caching templates',
    'Build a template once and reuse it. Because templates are content-addressable by the SHA-256 of their root filesystem, an identical build collapses to the same hash — no duplicate storage, no re-distribution. Bake your dependencies into a custom template instead of installing them on every sandbox create.',
  ],
  [
    'bp-pool',
    'Pool warm-up',
    'The first sandbox created from a brand-new template is a cold start (roughly 300ms to 3s while the image is staged and a microVM is built). Subsequent creates are pool hits — a pre-warmed VM is assigned in about 28ms. Keep traffic flowing to a template to keep its pool warm, and expect the first request after a quiet period to be slower.',
  ],
  [
    'bp-cost',
    'Cost optimization',
    'Billing meters running sandboxes per second, so destroy sandboxes the moment a job finishes rather than leaving them idle. For stateful or resumable work, snapshot the sandbox and restore it later instead of keeping a VM running between bursts of activity.',
  ],
  [
    'bp-security',
    'Security',
    'Sandboxes are isolated by design — each runs its own kernel under Cloud Hypervisor and KVM, and guest traffic is vsock-only. A sandbox has no direct internet access unless it is explicitly proxied through the master. Treat every sandbox as untrusted and never pass long-lived secrets into one you do not control.',
  ],
]

function BestPracticesBody() {
  return (
    <>
      <Lead>A few habits that keep sandboxes fast, cheap, and safe in production.</Lead>
      {bestPractices.map(([id, title, body]) => (
        <div key={id}>
          <H3 id={id}>{title}</H3>
          <P>{body}</P>
        </div>
      ))}
    </>
  )
}

function OpenApiBody() {
  return (
    <>
      <Lead>
        The complete API surface is published as a machine-readable OpenAPI document.
      </Lead>
      <div id="openapi-download" data-anchor className="scroll-mt-24">
        <P>
          Use Postman, Insomnia, or any OpenAPI-compatible tool to explore the full schema, generate
          clients, or drive contract tests.
        </P>
        <div className="my-4 flex flex-wrap gap-3">
          <a
            href={OPENAPI_URL}
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-2 rounded-lg border border-teal-500/40 bg-teal-500/10 px-4 py-2.5 text-sm font-medium text-teal-300 transition-colors hover:bg-teal-500/20"
          >
            <FileJson size={16} />
            Download openapi.yaml
          </a>
          <a
            href={SWAGGER_URL}
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-2 rounded-lg border border-zinc-700 px-4 py-2.5 text-sm font-medium text-zinc-200 transition-colors hover:border-zinc-500 hover:bg-zinc-900"
          >
            Interactive explorer
          </a>
        </div>
        <P>
          Prefer to poke at it in the browser? The raw spec lives at{' '}
          <code className="font-mono text-zinc-300">{OPENAPI_URL}</code> and the interactive
          explorer at <code className="font-mono text-zinc-300">{SWAGGER_URL}</code>.
        </P>
      </div>
    </>
  )
}

/* ----------------------------------------------------------------------
 * Section registry
 * -------------------------------------------------------------------- */
const SECTIONS: Section[] = [
  {
    id: 'quickstart',
    title: 'Quickstart',
    anchors: [
      { id: 'qs-api-key', label: 'Get an API key' },
      { id: 'qs-install', label: 'Install the SDK' },
      { id: 'qs-first-sandbox', label: 'Create a sandbox' },
    ],
    Body: QuickstartBody,
  },
  {
    id: 'authentication',
    title: 'Authentication',
    anchors: [
      { id: 'auth-key', label: 'API keys' },
      { id: 'auth-header', label: 'Header format' },
      { id: 'auth-rate-limits', label: 'Rate limits' },
      { id: 'auth-practices', label: 'Best practices' },
    ],
    Body: AuthBody,
  },
  {
    id: 'sandboxes',
    title: 'Sandboxes',
    anchors: [
      { id: 'sandbox-states', label: 'Lifecycle' },
      { id: 'create-sandbox', label: 'Create' },
      { id: 'list-sandboxes', label: 'List' },
      { id: 'get-sandbox', label: 'Get' },
      { id: 'start-sandbox', label: 'Start' },
      { id: 'stop-sandbox', label: 'Stop' },
      { id: 'destroy-sandbox', label: 'Destroy' },
    ],
    Body: SandboxesBody,
  },
  {
    id: 'templates',
    title: 'Templates',
    anchors: [
      { id: 'templates-hashes', label: 'Content-addressable' },
      { id: 'list-templates', label: 'List templates' },
      { id: 'build-template', label: 'Build template' },
      { id: 'template-from-snapshot', label: 'From snapshot' },
    ],
    Body: TemplatesBody,
  },
  {
    id: 'execution',
    title: 'Execution',
    anchors: [
      { id: 'exec-command', label: 'Run a command' },
      { id: 'exec-workdir', label: 'Working directory' },
      { id: 'exec-timeouts', label: 'Timeouts' },
    ],
    Body: ExecutionBody,
  },
  {
    id: 'files',
    title: 'Files',
    anchors: [
      { id: 'upload-file', label: 'Upload' },
      { id: 'download-file', label: 'Download' },
      { id: 'list-files', label: 'List' },
      { id: 'delete-file', label: 'Delete' },
    ],
    Body: FilesBody,
  },
  {
    id: 'snapshots',
    title: 'Snapshots',
    anchors: [
      { id: 'create-snapshot', label: 'Create' },
      { id: 'list-snapshots', label: 'List' },
      { id: 'restore-snapshot', label: 'Restore' },
      { id: 'promote-snapshot', label: 'Promote' },
    ],
    Body: SnapshotsBody,
  },
  {
    id: 'webhooks',
    title: 'Webhooks',
    anchors: [
      { id: 'webhooks-config', label: 'Configuration' },
      { id: 'create-webhook', label: 'Create webhook' },
      { id: 'webhook-events', label: 'Event types' },
      { id: 'webhook-signatures', label: 'Signature verification' },
      { id: 'webhook-retries', label: 'Retry policy' },
    ],
    Body: WebhooksBody,
  },
  {
    id: 'sdks',
    title: 'SDKs and CLI',
    anchors: [
      { id: 'sdk-python', label: 'Python SDK' },
      { id: 'sdk-cli', label: 'CLI' },
      { id: 'sdk-typescript', label: 'TypeScript SDK' },
    ],
    Body: SdksBody,
  },
  {
    id: 'errors',
    title: 'Errors',
    anchors: [
      { id: 'error-codes', label: 'Status codes' },
      { id: 'error-shape', label: 'Error response shape' },
    ],
    Body: ErrorsBody,
  },
  {
    id: 'best-practices',
    title: 'Best Practices',
    anchors: [
      { id: 'bp-caching', label: 'Caching templates' },
      { id: 'bp-pool', label: 'Pool warm-up' },
      { id: 'bp-cost', label: 'Cost optimization' },
      { id: 'bp-security', label: 'Security' },
    ],
    Body: BestPracticesBody,
  },
  {
    id: 'openapi',
    title: 'OpenAPI Spec',
    anchors: [{ id: 'openapi-download', label: 'Raw spec' }],
    Body: OpenApiBody,
  },
]

/* ----------------------------------------------------------------------
 * Sidebars
 * -------------------------------------------------------------------- */
function SearchInput({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="relative">
      <Search
        size={14}
        className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-zinc-500"
      />
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="Search docs"
        className="w-full rounded-md border border-zinc-800 bg-zinc-900 py-2 pl-8 pr-3 text-sm text-zinc-200 placeholder:text-zinc-600 focus:border-teal-500/50 focus:outline-none"
      />
    </div>
  )
}

function TocList({
  sections,
  activeSection,
  onNavigate,
}: {
  sections: Section[]
  activeSection: string
  onNavigate: (id: string) => void
}) {
  return (
    <nav className="mt-4 space-y-0.5">
      {sections.map((s) => (
        <button
          key={s.id}
          onClick={() => onNavigate(s.id)}
          className={`block w-full rounded-md px-3 py-1.5 text-left text-sm transition-colors ${
            activeSection === s.id
              ? 'bg-teal-500/10 font-medium text-teal-300'
              : 'text-zinc-400 hover:bg-zinc-900 hover:text-zinc-100'
          }`}
        >
          {s.title}
        </button>
      ))}
      {sections.length === 0 && (
        <p className="px-3 py-2 text-sm text-zinc-600">No matching sections.</p>
      )}
    </nav>
  )
}

/* ----------------------------------------------------------------------
 * Page
 * -------------------------------------------------------------------- */
export default function Docs() {
  const location = useLocation()
  const navigate = useNavigate()

  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [activeSection, setActiveSection] = useState(SECTIONS[0].id)
  const [activeAnchor, setActiveAnchor] = useState('')
  const [mobileOpen, setMobileOpen] = useState(false)
  const didInitialScroll = useRef(false)

  // Debounce the search input by 200ms.
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query.trim().toLowerCase()), 200)
    return () => clearTimeout(t)
  }, [query])

  const filteredSections = useMemo(() => {
    if (!debouncedQuery) return SECTIONS
    return SECTIONS.filter((s) => s.title.toLowerCase().includes(debouncedQuery))
  }, [debouncedQuery])

  // Deep-link: on first load, scroll to the URL hash if there is one.
  useEffect(() => {
    if (didInitialScroll.current) return
    didInitialScroll.current = true
    const id = location.hash.replace('#', '')
    if (!id) return
    const t = setTimeout(() => {
      document.getElementById(id)?.scrollIntoView({ behavior: 'auto', block: 'start' })
    }, 80)
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Scroll-spy: highlight the active section and anchor as the reader scrolls.
  useEffect(() => {
    const sections = Array.from(document.querySelectorAll<HTMLElement>('[data-section]'))
    const anchors = Array.from(document.querySelectorAll<HTMLElement>('[data-anchor]'))

    const secVisible = new Set<string>()
    const secObs = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (e.isIntersecting) secVisible.add(e.target.id)
          else secVisible.delete(e.target.id)
        }
        for (const el of sections) {
          if (secVisible.has(el.id)) {
            setActiveSection(el.id)
            break
          }
        }
      },
      { rootMargin: '-12% 0px -78% 0px' },
    )
    sections.forEach((el) => secObs.observe(el))

    const anchVisible = new Set<string>()
    const anchObs = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (e.isIntersecting) anchVisible.add(e.target.id)
          else anchVisible.delete(e.target.id)
        }
        for (const el of anchors) {
          if (anchVisible.has(el.id)) {
            setActiveAnchor(el.id)
            break
          }
        }
      },
      { rootMargin: '-12% 0px -80% 0px' },
    )
    anchors.forEach((el) => anchObs.observe(el))

    return () => {
      secObs.disconnect()
      anchObs.disconnect()
    }
  }, [debouncedQuery])

  function goTo(id: string) {
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    navigate(`#${id}`, { replace: true })
    setMobileOpen(false)
  }

  const activeSectionData =
    filteredSections.find((s) => s.id === activeSection) ?? filteredSections[0]

  return (
    <div className="min-h-full bg-zinc-950 text-zinc-100">
      {/* Mobile top bar */}
      <div className="sticky top-0 z-30 flex items-center gap-3 border-b border-zinc-900 bg-zinc-950/95 px-4 py-3 backdrop-blur lg:hidden">
        <button
          onClick={() => setMobileOpen((o) => !o)}
          className="rounded-md border border-zinc-800 p-1.5 text-zinc-300"
          aria-label="Toggle docs navigation"
        >
          {mobileOpen ? <X size={16} /> : <Menu size={16} />}
        </button>
        <span className="text-sm font-medium">Documentation</span>
      </div>

      {/* Mobile menu panel */}
      {mobileOpen && (
        <div className="border-b border-zinc-900 bg-zinc-950 px-4 py-4 lg:hidden">
          <SearchInput value={query} onChange={setQuery} />
          <TocList
            sections={filteredSections}
            activeSection={activeSection}
            onNavigate={goTo}
          />
        </div>
      )}

      <div className="mx-auto flex max-w-[1300px] gap-10 px-5 lg:px-8">
        {/* Left sidebar — table of contents */}
        <aside className="hidden w-60 shrink-0 lg:block">
          <div className="sticky top-0 max-h-screen overflow-y-auto py-10">
            <div className="mb-1 font-mono text-[11px] uppercase tracking-[0.2em] text-zinc-500">
              On this site
            </div>
            <div className="mt-3">
              <SearchInput value={query} onChange={setQuery} />
            </div>
            <TocList
              sections={filteredSections}
              activeSection={activeSection}
              onNavigate={goTo}
            />
          </div>
        </aside>

        {/* Main content */}
        <main className="min-w-0 flex-1 py-10">
          <div className="mx-auto max-w-[760px]">
            <header className="mb-4 border-b border-zinc-900 pb-6">
              <div className="font-mono text-[11px] uppercase tracking-[0.2em] text-teal-400">
                API Reference
              </div>
              <h1 className="mt-2 text-3xl font-semibold tracking-tight">Vajra Documentation</h1>
              <p className="mt-2 text-sm leading-relaxed text-zinc-400">
                Everything you need to launch hardware-isolated microVM sandboxes — the REST API,
                SDKs, and CLI.
              </p>
              <div className="mt-3 inline-flex items-center gap-2 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-1.5">
                <span className="text-[11px] uppercase tracking-wider text-zinc-500">Base URL</span>
                <code className="font-mono text-[12.5px] text-teal-300">{HOST}</code>
              </div>
            </header>

            {filteredSections.length === 0 ? (
              <div className="py-20 text-center">
                <p className="text-sm text-zinc-500">
                  No sections match “<span className="text-zinc-300">{query}</span>”.
                </p>
              </div>
            ) : (
              filteredSections.map((s, i) => (
                <section
                  key={s.id}
                  id={s.id}
                  data-section
                  className={`scroll-mt-24 ${
                    i === 0 ? 'pt-2' : 'mt-14 border-t border-zinc-900 pt-12'
                  }`}
                >
                  <h2 className="text-2xl font-semibold tracking-tight text-zinc-100">
                    {s.title}
                  </h2>
                  <div className="mt-3">
                    <s.Body />
                  </div>
                </section>
              ))
            )}
          </div>
        </main>

        {/* Right sidebar — on-page anchors for the active section */}
        <aside className="hidden w-52 shrink-0 xl:block">
          <div className="sticky top-0 max-h-screen overflow-y-auto py-10">
            {activeSectionData && activeSectionData.anchors.length > 0 && (
              <>
                <div className="mb-3 font-mono text-[11px] uppercase tracking-[0.2em] text-zinc-500">
                  On this page
                </div>
                <nav className="space-y-1 border-l border-zinc-800">
                  {activeSectionData.anchors.map((a) => (
                    <button
                      key={a.id}
                      onClick={() => goTo(a.id)}
                      className={`-ml-px block w-full border-l-2 px-3 py-1 text-left text-[13px] transition-colors ${
                        activeAnchor === a.id
                          ? 'border-teal-400 text-teal-300'
                          : 'border-transparent text-zinc-500 hover:text-zinc-200'
                      }`}
                    >
                      {a.label}
                    </button>
                  ))}
                </nav>
              </>
            )}
          </div>
        </aside>
      </div>
    </div>
  )
}
