import React, { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api.ts'
import './RunnerStatus.css'

export interface RunnerHealth {
  configured?: boolean
  status?: string
  loaded_model_id?: string | null
  loaded_file?: string | null
  engine_pid?: number | null
  has_gpu?: boolean | null
  gpu_name?: string | null
  engine_error?: string | null
  engine_notice?: string | null
  context_limit?: number | null
  current_context_tokens?: number | null
  current_context?: number | null
  threads?: number | null
  gpu_layers?: number | null
  extra_args?: string[] | string | null
  processing?: boolean | null
  queued_requests?: number | null
  host_active_requests?: number | null
  host_queued_requests?: number | null
  requests_completed?: number | null
  requests_failed?: number | null
  prompt_tokens?: number | null
  completion_tokens?: number | null
  last_prompt_tokens?: number | null
  last_completion_tokens?: number | null
  last_tokens_per_second?: number | null
  started_at?: string | null
  error?: string | null
}

interface RunnerStatusProps {
  /** Optional labels from the managed model catalog. */
  modelNames?: Record<string, string>
}

const POLL_MS = 3000

function present(value: unknown): value is string | number | boolean {
  return value !== null && value !== undefined && value !== ''
}

function numberValue(value: unknown): string {
  return present(value) && typeof value === 'number' ? value.toLocaleString() : 'Unavailable'
}

function modelLabel(health: RunnerHealth, names: Record<string, string>): string {
  if (health.loaded_model_id && names[health.loaded_model_id]) return names[health.loaded_model_id]
  // A cache path runs to a hundred characters, so the file name stands in for
  // the model before the opaque id does. The full path is in the advanced
  // details, which is the one place that needs it.
  if (health.loaded_file) return health.loaded_file.split('/').pop()?.replace(/\.gguf$/i, '') || health.loaded_file
  return health.loaded_model_id || 'No model loaded'
}

function formatDate(value: number | string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'Unknown time' : date.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' })
}

function statusCopy(health: RunnerHealth | null): { label: string; tone: string; detail: string } {
  if (!health) return { label: 'Connecting', tone: 'loading', detail: 'Checking the managed runner…' }
  if (health.configured === false) return { label: 'Not configured', tone: 'offline', detail: 'Configure a managed runner to load local models.' }
  const status = (health.status || '').toLowerCase()
  if (status === 'error' || health.error || health.engine_error) return { label: 'Needs attention', tone: 'error', detail: health.error || health.engine_error || 'The runner reported an error.' }
  if (health.processing === true || status === 'busy' || status === 'processing') return { label: 'Processing', tone: 'busy', detail: 'The runner is generating a response.' }
  if (status === 'offline') return { label: 'Offline', tone: 'offline', detail: 'The managed runner is not reachable.' }
  if (status === 'idle') return { label: 'Idle', tone: 'ready', detail: health.loaded_model_id || health.loaded_file ? 'Ready for the next request.' : 'Load a model to begin.' }
  if (status === 'ready' || status === 'running') return { label: 'Ready', tone: 'ready', detail: health.engine_notice || 'Ready for local inference.' }
  return { label: status ? status[0].toUpperCase() + status.slice(1) : 'Unknown', tone: 'loading', detail: health.engine_notice || 'Waiting for runner status.' }
}

export const RunnerStatus: React.FC<RunnerStatusProps> = ({ modelNames = {} }) => {
  const [health, setHealth] = useState<RunnerHealth | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [updatedAt, setUpdatedAt] = useState<number | null>(null)
  const mounted = useRef(true)
  const request = useRef<AbortController | null>(null)
  const inFlight = useRef(false)
  const requestId = useRef(0)

  const refresh = useCallback(async () => {
    if (inFlight.current) return
    inFlight.current = true
    const id = ++requestId.current
    const controller = new AbortController()
    request.current = controller
    let timedOut = false
    const timeout = window.setTimeout(() => { timedOut = true; controller.abort() }, 10000)
    try {
      const response = await api('/api/v1/managed-models/runner-health', { signal: controller.signal })
      if (!response.ok) throw new Error(`Runner status unavailable (${response.status})`)
      const data = await response.json() as RunnerHealth
      if (!mounted.current || id !== requestId.current) return
      setHealth(data)
      setUpdatedAt(Date.now())
      setError('')
    } catch (cause) {
      if (!mounted.current || id !== requestId.current) return
      if (cause instanceof DOMException && cause.name === 'AbortError' && !timedOut) return
      setError(timedOut ? 'The runner took too long to respond.' : cause instanceof Error ? cause.message : 'Could not refresh runner status.')
    } finally {
      window.clearTimeout(timeout)
      inFlight.current = false
      if (mounted.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    mounted.current = true
    let timer: number | undefined
    // Polls on a self-rescheduling timer rather than an interval so a slow
    // response cannot stack requests behind itself.
    const poll = async () => {
      await refresh()
      if (mounted.current) timer = window.setTimeout(() => void poll(), POLL_MS)
    }
    void poll()
    return () => {
      mounted.current = false
      if (timer !== undefined) window.clearTimeout(timer)
      request.current?.abort()
    }
  }, [refresh])

  const state = statusCopy(health)
  const context = health?.current_context ?? health?.current_context_tokens
  const totalTokens = health?.prompt_tokens != null && health?.completion_tokens != null
    ? health.prompt_tokens + health.completion_tokens
    : null
  const args = Array.isArray(health?.extra_args) ? health?.extra_args.join(' ') : health?.extra_args

  return (
    <section className="runner-status" aria-labelledby="runner-status-title">
      <div className="runner-status__header">
        <div>
          <span className="eyebrow">MANAGED RUNNER</span>
          <h2 id="runner-status-title">Runner status</h2>
          <p className="runner-status__lede">A live view of the local llama.cpp process and its work queue.</p>
        </div>
        <button className="btn btn-secondary runner-status__refresh" type="button" onClick={() => { setLoading(true); void refresh() }} disabled={loading}>
          {loading ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>

      {error && <div className="runner-status__notice runner-status__notice--error" role="alert"><strong>Refresh failed.</strong> {error}{updatedAt && <span> Showing the last successful snapshot from {formatDate(updatedAt)}.</span>}</div>}
      {updatedAt && error && <div className="runner-status__stale" aria-live="polite">Status may be stale · last checked {formatDate(updatedAt)}</div>}

      <div className="runner-status__hero">
        <div className={`runner-status__dot runner-status__dot--${state.tone}`} aria-hidden="true" />
        <div><span className="runner-status__label">{state.label}</span><p>{state.detail}</p></div>
        {updatedAt && !error && <span className="runner-status__updated">Updated {formatDate(updatedAt)}</span>}
      </div>

      <div className="runner-status__grid">
        <article className="runner-status__card runner-status__card--wide"><h3>Loaded model</h3><strong>{health ? modelLabel(health, modelNames) : 'Loading…'}</strong><dl><div><dt>Configured context</dt><dd>{numberValue(health?.context_limit)}</dd></div><div><dt>Current context</dt><dd>{numberValue(context)}</dd></div></dl></article>
        <article className="runner-status__card"><h3>Runner work queue</h3><div className="runner-status__metric"><strong>{numberValue(health?.queued_requests)}</strong><span>queued for this runner</span></div><dl><div><dt>Active requests (all models)</dt><dd>{numberValue(health?.host_active_requests)}</dd></div><div><dt>Queued requests (all models)</dt><dd>{numberValue(health?.host_queued_requests)}</dd></div></dl><p className="runner-status__submetric">{health?.processing === true ? 'A request is processing now.' : health?.processing === false ? 'Runner is currently idle.' : 'Processing state unavailable.'}</p></article>
        <article className="runner-status__card"><h3>Token activity</h3><p className="runner-status__scope">Since runner startup</p><dl className="runner-status__stats"><div><dt>Prompt tokens</dt><dd>{numberValue(health?.prompt_tokens)}</dd></div><div><dt>Completion tokens</dt><dd>{numberValue(health?.completion_tokens)}</dd></div><div><dt>Total observed</dt><dd>{totalTokens == null ? 'Unavailable' : totalTokens.toLocaleString()}</dd></div><div><dt>Last throughput</dt><dd>{typeof health?.last_tokens_per_second === 'number' ? `${health.last_tokens_per_second.toFixed(1)} tok/s` : 'Unavailable'}</dd></div></dl></article>
        <article className="runner-status__card"><h3>Requests</h3><p className="runner-status__scope">Since runner startup</p><dl className="runner-status__stats"><div><dt>Completed</dt><dd>{numberValue(health?.requests_completed)}</dd></div><div><dt>Failed</dt><dd>{numberValue(health?.requests_failed)}</dd></div><div><dt>Last prompt</dt><dd>{numberValue(health?.last_prompt_tokens)}</dd></div><div><dt>Last completion</dt><dd>{numberValue(health?.last_completion_tokens)}</dd></div></dl></article>
      </div>

      <details className="runner-status__advanced"><summary>Advanced process details</summary><dl><div><dt>Weights file</dt><dd className="runner-status__args">{health?.loaded_file || 'Unavailable'}</dd></div><div><dt>Engine PID</dt><dd>{numberValue(health?.engine_pid)}</dd></div><div><dt>Accelerator</dt><dd>{health?.has_gpu === true ? health.gpu_name || 'GPU available' : health?.has_gpu === false ? 'CPU only' : 'Unavailable'}</dd></div><div><dt>Threads</dt><dd>{numberValue(health?.threads)}</dd></div><div><dt>GPU layers</dt><dd>{health?.gpu_layers != null ? health.gpu_layers.toLocaleString() : health?.configured ? 'Auto' : 'Unavailable'}</dd></div><div><dt>Started</dt><dd>{health?.started_at ? formatDate(health.started_at) : 'Unavailable'}</dd></div><div><dt>llama.cpp arguments</dt><dd className="runner-status__args">{health?.extra_args == null ? 'Unavailable' : args || 'None'}</dd></div></dl></details>
    </section>
  )
}

export default RunnerStatus
