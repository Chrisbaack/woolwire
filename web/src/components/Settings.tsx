import React, { useState, useEffect, useRef } from 'react'
import { api } from '../api.ts'
import { RunnerStatus } from './RunnerStatus.tsx'
import './settings.css'

interface HostedModel {
  id: string
  name: string
  model_type: string
  endpoint_url: string
  api_key: string
  context_limit: number
  max_tokens: number
  enabled: boolean
  published: boolean
  revision: number
}

interface HostLimits {
  MaxActive: number
  MaxQueuedPerMember: number
  MaxQueuedTotal: number
  QueueTimeoutSeconds: number
  ExecutionTimeoutSeconds: number
}

interface HardwareProfile {
  arch: string
  os: string
  cpu_cores: number
  total_ram_mb: number
  has_nvidia_gpu: boolean
  gpu_name?: string
}

// One GGUF file a Hugging Face repository publishes.
interface RepoWeight {
  path: string
  filename: string
  size_bytes: number
  url: string
  quantization?: string
  // Above one when the model is split across files, all of which are needed.
  shard_count?: number
  // Files that belong with this model, chosen for it rather than offered as
  // choices: a projector for vision, an MTP module for speculative decoding.
  companions?: RepoCompanion[]
  // The whole download: this model plus its companions.
  total_bytes: number
}

interface RepoCompanion {
  kind: 'projector' | 'draft'
  path: string
  filename: string
  size_bytes: number
  url: string
}

interface RunnerHealth {
  configured: boolean
  status: string
  loaded_model_id?: string
  loaded_file?: string
  engine_pid?: number
  // The runner's own hardware. The app container cannot see the GPU: it is
  // passed to the runner, so the runner is the one that reports it.
  has_gpu?: boolean
  gpu_name?: string
  // What the engine said when it failed, so a model that cannot be loaded
  // explains itself instead of leaving the runner silently in "error".
  engine_error?: string
  error?: string
  // The launch settings the engine is actually running with. They seed the
  // advanced form, and read back as zero while no model is loaded.
  context_limit?: number
  threads?: number
  gpu_layers?: number | null
  extra_args?: string[]
  processing?: boolean
  queued_requests?: number
  prompt_tokens?: number
  completion_tokens?: number
  current_context_tokens?: number | null
}

interface ArtifactManifest {
  id: string
  name: string
  filename: string
  // Where the weights sit relative to the models directory. A downloaded
  // model is at the top level; one found in a Hugging Face cache is nested.
  path: string
  size_bytes: number
  sha256: string
  source_url?: string
  context_limit: number
  installed_at: number
  // 'download' for weights Woolwire installed, 'cache' for weights it found.
  source?: string
  repo_id?: string
  architecture?: string
  // Supporting files loaded with this model, never listed as models.
  companions?: { kind: 'projector' | 'draft'; path?: string; filename: string }[]
}

interface PopularModelDef {
  id: string
  name: string
  creator: string
  category:
    | 'Edge & Fast'
    | 'Coding'
    | 'Reasoning'
    | 'General'
    | 'SOTA Flagship'
    | 'MoE Flagship'
    | 'Workhorse'
    | 'Frontier MoE'
    | 'Frontier Multimodal'
    | 'Coding & Agents'
  url: string
  filename: string
  sizeMB: number
  contextLimit: number
  description: string
  quant: string
}

const POPULAR_MODELS: PopularModelDef[] = [
  {
    id: 'qwen3.8-27b-instruct',
    name: 'Qwen3.8 27B Instruct',
    creator: 'Qwen / Alibaba',
    category: 'SOTA Flagship',
    url: 'https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/main/Qwen3.8-27B-UD-Q4_K_M.gguf',
    filename: 'Qwen3.8-27B-UD-Q4_K_M.gguf',
    sizeMB: 15701,
    contextLimit: 32768,
    quant: 'UD-Q4_K_M',
    description: 'Premier open-weight dense SOTA leader (10M+ downloads) with unsloth dynamic quants; top-tier coding, mathematics, and reasoning.',
  },
  {
    id: 'ornith-1.5-35b-a3b',
    name: 'Ornith 1.5 35B (A3B)',
    creator: 'Ornith AI',
    category: 'MoE Flagship',
    url: 'https://huggingface.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF/resolve/main/Ornith-1.5-35B-Q4_K_M.gguf',
    filename: 'Ornith-1.5-35B-Q4_K_M.gguf',
    sizeMB: 20707,
    contextLimit: 65536,
    quant: 'Q4_K_M',
    description: 'Breakthrough 35B MoE with 3B active parameters (A3B). Delivers extreme generation throughput with deep 35B analytical capabilities.',
  },
  {
    id: 'ornith-1.5-9b',
    name: 'Ornith 1.5 9B Instruct',
    creator: 'Ornith AI',
    category: 'Workhorse',
    url: 'https://huggingface.co/ornith-ai/Ornith-1.5-9B-GGUF/resolve/main/Ornith-1.5-9B-Q4_K_M.gguf',
    filename: 'Ornith-1.5-9B-Q4_K_M.gguf',
    sizeMB: 5512,
    contextLimit: 32768,
    quant: 'Q4_K_M',
    description: 'High-efficiency dense 9B workhorse outperforming previous-gen 14B models with low memory overhead on consumer hardware.',
  },
  {
    id: 'qwen3.8-flash-next',
    name: 'Qwen3.8 Flash Next',
    creator: 'Qwen / Alibaba',
    category: 'Frontier MoE',
    url: 'https://huggingface.co/Cyronius/Qwen3.8-Flash-Next-131B-A6B-GGUF/resolve/main/qwen38-keep1-Q3KXL.gguf',
    filename: 'qwen38-keep1-Q3KXL.gguf',
    sizeMB: 61785,
    contextLimit: 131072,
    quant: 'Q3_K_XL',
    description: 'Next-generation 131B MoE with 6B active parameters (A6B) providing frontier-grade intelligence at rapid inference speeds.',
  },
  {
    id: 'glm-5.3-flash',
    name: 'GLM 5.3 Flash',
    creator: 'Zhipu AI / ZAI',
    category: 'Frontier Multimodal',
    url: 'https://huggingface.co/patrickbdevaney/GLM-5.3-Flash-REAP50-GGUF/resolve/main/GLM-5.3-Flash-REAP50-IQ3_M.gguf',
    filename: 'GLM-5.3-Flash-REAP50-IQ3_M.gguf',
    sizeMB: 68790,
    contextLimit: 131072,
    quant: 'REAP50-IQ3_M',
    description: 'Zhipu premier open-weight flagship multimodal and reasoning architecture with massive 131k context and deep analytical fidelity.',
  },
  {
    id: 'deepseek-v4-flash-0731',
    name: 'DeepSeek-V4 Flash 0731',
    creator: 'DeepSeek / Unsloth',
    category: 'Reasoning',
    url: 'https://huggingface.co/unsloth/DeepSeek-V4-Flash-0731-GGUF/resolve/main/dspark-DeepSeek-V4-Flash-0731-Q8_0.gguf',
    filename: 'dspark-DeepSeek-V4-Flash-0731-Q8_0.gguf',
    sizeMB: 10391,
    contextLimit: 65536,
    quant: 'Q8_0',
    description: 'DeepSeek V4 Flash generation offering extreme speed and native chain-of-thought logic.',
  },
  {
    id: 'ministral-3-14b-instruct',
    name: 'Ministral 3 14B Instruct',
    creator: 'Mistral AI',
    category: 'Coding & Agents',
    url: 'https://huggingface.co/mistralai/Ministral-3-14B-Instruct-2512-GGUF/resolve/main/Ministral-3-14B-Instruct-2512-Q4_K_M.gguf',
    filename: 'Ministral-3-14B-Instruct-2512-Q4_K_M.gguf',
    sizeMB: 7857,
    contextLimit: 32768,
    quant: 'Q4_K_M',
    description: 'Mistral AI cutting-edge 14B model tailored for multi-step agentic workflows and advanced programming.',
  },
  {
    id: 'ministral-3-3b-instruct',
    name: 'Ministral 3 3B Instruct',
    creator: 'Mistral AI',
    category: 'Edge & Fast',
    url: 'https://huggingface.co/mistralai/Ministral-3-3B-Instruct-2512-GGUF/resolve/main/Ministral-3-3B-Instruct-2512-Q4_K_M.gguf',
    filename: 'Ministral-3-3B-Instruct-2512-Q4_K_M.gguf',
    sizeMB: 2047,
    contextLimit: 16384,
    quant: 'Q4_K_M',
    description: 'Ultra-efficient edge model optimized for low-latency chat, lightweight mobile/laptop inference, and instant response.',
  },
]

function getHardwareFitBadge(modelSizeMB: number, hw: HardwareProfile | null): {
  label: string
  color: string
  tooltip: string
} {
  if (!hw) {
    return { label: 'Unknown Fit', color: 'var(--text-secondary)', tooltip: 'Hardware profile not detected' }
  }

  const ramMB = hw.total_ram_mb || 0
  const hasGPU = hw.has_nvidia_gpu

  if (hasGPU) {
    if (modelSizeMB <= 6000) {
      return {
        label: '🟢 Full GPU Offload',
        color: 'var(--accent-success)',
        tooltip: 'Model fits comfortably in dedicated GPU VRAM for maximum tokens/sec.',
      }
    }
    if (modelSizeMB <= 16000) {
      return {
        label: '🟢 GPU Accelerated',
        color: 'var(--accent-success)',
        tooltip: 'Substantial GPU acceleration with partial or full VRAM offload (8-16GB VRAM).',
      }
    }
    if (modelSizeMB <= 24000) {
      return {
        label: '🟢 High-VRAM GPU',
        color: 'var(--accent-success)',
        tooltip: 'Offloads to high-capacity consumer GPUs (RTX 3090/4090 24GB VRAM).',
      }
    }
  }

  if (ramMB > 0) {
    const freeBudgetMB = ramMB * 0.8
    if (modelSizeMB <= freeBudgetMB) {
      return {
        label: hasGPU ? '🟡 Hybrid GPU/RAM' : '🟢 Fits in RAM',
        color: hasGPU ? 'var(--accent-primary)' : 'var(--accent-success)',
        tooltip: `Fits in system RAM (${(ramMB / 1024).toFixed(1)} GB detected).`,
      }
    }
    if (modelSizeMB <= ramMB) {
      return {
        label: '🟡 Tight Memory Fit',
        color: '#f59e0b',
        tooltip: `Tight fit for ${(ramMB / 1024).toFixed(1)} GB RAM. May experience swapping or paging under load.`,
      }
    }
    return {
      label: '🔴 Exceeds System RAM',
      color: 'var(--accent-danger)',
      tooltip: `Model (${(modelSizeMB / 1024).toFixed(1)} GB) exceeds system RAM (${(ramMB / 1024).toFixed(1)} GB). High risk of out-of-memory crash.`,
    }
  }

  return { label: 'Compatible', color: 'var(--text-secondary)', tooltip: 'Check your available RAM' }
}

export const Settings: React.FC = () => {
  type SettingsTab = 'status' | 'managed' | 'connected' | 'hosting' | 'access'
  const [activeTab, setActiveTab] = useState<SettingsTab>('status')
  const [managedContextLimit, setManagedContextLimit] = useState(4096)
  const [managedThreads, setManagedThreads] = useState(4)
  const [managedGpuLayers, setManagedGpuLayers] = useState<'auto' | string>('auto')
  const [managedCustomGpuLayers, setManagedCustomGpuLayers] = useState('0')
  const [managedExtraArgs, setManagedExtraArgs] = useState('[]')
  const [managedConfigError, setManagedConfigError] = useState('')
  const managedConfigHydrated = useRef(false)
  const [models, setModels] = useState<HostedModel[]>([])
  const [limits, setLimits] = useState<HostLimits>({
    MaxActive: 1,
    MaxQueuedPerMember: 1,
    MaxQueuedTotal: 10,
    QueueTimeoutSeconds: 300,
    ExecutionTimeoutSeconds: 600,
  })
  const [hardware, setHardware] = useState<HardwareProfile | null>(null)

  const [runner, setRunner] = useState<RunnerHealth | null>(null)

  // When a runner is configured it is the authority on the GPU: the device is
  // passed to that container, and the app's own probe can only see the host's
  // driver, not whether this deployment can actually use it.
  const gpuAvailable = runner?.configured ? !!runner.has_gpu : !!hardware?.has_nvidia_gpu
  const gpuLabel = (runner?.configured ? runner.gpu_name : hardware?.gpu_name) || 'NVIDIA GPU'
  const [artifacts, setArtifacts] = useState<ArtifactManifest[]>([])
  const [loadError, setLoadError] = useState<string>('')
  const [loadSuccess, setLoadSuccess] = useState<string>('')
  // The model the loader is pointed at, and — while a load is in flight — the
  // one being started. The load request does not return until the engine
  // reports healthy, so that wait has to be visible.
  const [selectedArtifactID, setSelectedArtifactID] = useState<string>('')
  const [loadingModelID, setLoadingModelID] = useState<string>('')
  const selectedArtifact = (artifacts || []).find((a) => a.id === selectedArtifactID) || null
  const selectedIsLoaded = !!selectedArtifact && runner?.loaded_file === (selectedArtifact.path || selectedArtifact.filename)
  const loadingModel = loadingModelID !== ''
  const loadingName = (artifacts || []).find((a) => a.id === loadingModelID)?.name || 'the model'
  // What the runner is serving, named rather than pathed: a Hugging Face
  // cache path is long enough to swamp anything shown beside it.
  const loadedModelName = runner?.loaded_file
    ? (artifacts || []).find((a) => (a.path || a.filename) === runner.loaded_file)?.name
      || runner.loaded_file.split('/').pop()?.replace(/\.gguf$/i, '')
      || ''
    : ''

  const [name, setName] = useState('')
  const [endpointUrl, setEndpointUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [contextLimit, setContextLimit] = useState(4096)
  const [maxTokens, setMaxTokens] = useState(2048)
  const [published, setPublished] = useState(true)
  // backendModel is what the backend server is sent in the OpenAI "model"
  // field. It defaults to the display name; a multi-model server needs the
  // identifier it knows, not Woolwire's opaque model id.
  const [backendModel, setBackendModel] = useState('')
  // allowPrivateNetwork opts one endpoint out of the private-range and
  // non-standard-port blocks. Off by default.
  const [allowPrivateNetwork, setAllowPrivateNetwork] = useState(false)

  const [modelError, setModelError] = useState('')
  const [modelSuccess, setModelSuccess] = useState('')
  const [limitSuccess, setLimitSuccess] = useState('')

  // Test & Discovery states for Hosted Models
  const [testingEndpoint, setTestingEndpoint] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string } | null>(null)
  const [discoveringModels, setDiscoveringModels] = useState(false)
  const [discoveredModels, setDiscoveredModels] = useState<string[]>([])
  const [modelTestStatus, setModelTestStatus] = useState<Record<string, { testing?: boolean; ok?: boolean; latency?: number; error?: string }>>({})

  // Managed artifact download form
  const [dlUrl, setDlUrl] = useState('')
  const [dlFilename, setDlFilename] = useState('')
  const [dlSha, setDlSha] = useState('')
  const [dlLoading, setDlLoading] = useState(false)
  const [dlError, setDlError] = useState('')
  const [dlSuccess, setDlSuccess] = useState('')
  const [dlProgress, setDlProgress] = useState<number | null>(null)

  // Storage & Popular Models
  const [storageInfo, setStorageInfo] = useState<{ configured: boolean; used_bytes?: number; budget_bytes?: number; read_only?: boolean } | null>(null)
  const [budgetGB, setBudgetGB] = useState('')
  const [savingBudget, setSavingBudget] = useState(false)
  const [budgetMsg, setBudgetMsg] = useState('')
  const budgetHydrated = useRef(false)
  const [popularCategory, setPopularCategory] = useState<string>('All')
  const [hfResolveInput, setHfResolveInput] = useState<string>('')
  const [hfResolveMsg, setHfResolveMsg] = useState<string>('')
  const [hfFiles, setHfFiles] = useState<RepoWeight[]>([])
  const [hfLoading, setHfLoading] = useState(false)

  // Device pairing & setup secret
  // The active setup secret is never fetched back: it is single-use and the
  // server does not return it. Only a freshly minted one is shown, once.
  const [setupRedeemed, setSetupRedeemed] = useState(true)
  const [mintedSecret, setMintedSecret] = useState('')
  const [copiedSetupToken, setCopiedSetupToken] = useState(false)
  const [newSetupPin, setNewSetupPin] = useState('')
  const [pinError, setPinError] = useState('')
  const [pinSuccess, setPinSuccess] = useState('')
  const [updatingPin, setUpdatingPin] = useState(false)

  // Local API token for the OpenAI-compatible routes.
  const [localAPIToken, setLocalAPIToken] = useState('')
  const [showLocalAPIToken, setShowLocalAPIToken] = useState(false)
  const [copiedLocalAPIToken, setCopiedLocalAPIToken] = useState(false)
  const [regeneratingToken, setRegeneratingToken] = useState(false)

  const fetchSetupInfo = async () => {
    try {
      const res = await api('/api/v1/setup/info')
      if (res.ok) {
        const data = await res.json()
        setSetupRedeemed(Boolean(data.redeemed))
      }
    } catch {
      // ignore
    }
  }

  const fetchLocalAPIToken = async () => {
    try {
      const res = await api('/api/v1/local-api-token')
      if (res.ok) {
        const data = await res.json()
        setLocalAPIToken(data.token || '')
      }
    } catch {
      // ignore
    }
  }

  const regenerateLocalAPIToken = async () => {
    try {
      setRegeneratingToken(true)
      const res = await api('/api/v1/local-api-token/regenerate', { method: 'POST', body: '{}' })
      if (res.ok) {
        const data = await res.json()
        setLocalAPIToken(data.token || '')
      }
    } finally {
      setRegeneratingToken(false)
    }
  }

  // mintSetupSecret asks the server for a fresh one-time secret. Leaving it
  // blank generates a 128-bit value; a custom phrase must be at least 12
  // characters, because a short PIN on a LAN-reachable node is guessable.
  const mintSetupSecret = async (custom: string) => {
    const res = await api('/api/v1/setup/token', {
      method: 'POST',
      body: JSON.stringify(custom ? { token: custom } : {}),
    })
    if (!res.ok) {
      throw new Error((await res.text()) || 'Failed to mint a setup secret')
    }
    const data = await res.json()
    setMintedSecret(data.token)
    setSetupRedeemed(false)
    return data.token as string
  }

  const handleUpdatePin = async (e: React.FormEvent) => {
    e.preventDefault()
    const custom = newSetupPin.trim()
    if (custom && custom.length < 12) {
      setPinError('A custom setup secret must be at least 12 characters')
      return
    }
    try {
      setUpdatingPin(true)
      setPinError('')
      setPinSuccess('')
      await mintSetupSecret(custom)
      setNewSetupPin('')
      setPinSuccess('New one-time setup secret issued. Copy it now; it is shown only here.')
      setTimeout(() => setPinSuccess(''), 8000)
    } catch (err: any) {
      setPinError(err.message || 'Failed to update setup secret')
    } finally {
      setUpdatingPin(false)
    }
  }

  const copySetupToken = () => {
    navigator.clipboard.writeText(mintedSecret)
    setCopiedSetupToken(true)
    setTimeout(() => setCopiedSetupToken(false), 2000)
  }

  // Contributions & Social Recognition settings
  const [optOut, setOptOut] = useState(false)
  const [contribMsg, setContribMsg] = useState('')

  const fetchContributionsSettings = async () => {
    try {
      const res = await api('/api/v1/contributions/settings')
      if (res.ok) {
        const data = await res.json()
        setOptOut(Boolean(data.opt_out))
      }
    } catch {
      // ignore
    }
  }

  const handleToggleOptOut = async (newVal: boolean) => {
    try {
      setContribMsg('')
      const res = await api('/api/v1/contributions/settings', {
        method: 'POST',
          body: JSON.stringify({ opt_out: newVal }),
      })
      if (res.ok) {
        setOptOut(newVal)
        setContribMsg(newVal ? 'Opted out of public contribution receipts.' : 'Opted into public contribution receipts.')
        setTimeout(() => setContribMsg(''), 4000)
      }
    } catch {
      // ignore
    }
  }

  const fetchModels = async () => {
    try {
      const res = await api('/api/v1/hosted-models')
      if (res.ok) {
        const data = await res.json()
        setModels(Array.isArray(data) ? data : [])
      }
    } catch {
      // ignore
    }
  }

  const fetchLimits = async () => {
    try {
      const res = await api('/api/v1/host-limits')
      if (res.ok) {
        const data = await res.json()
        if (data && typeof data === 'object') {
          setLimits((prev) => ({ ...prev, ...data }))
        }
      }
    } catch {
      // ignore
    }
  }

  const fetchHardwareAndRunner = async () => {
    try {
      const hwRes = await api('/api/v1/hardware')
      if (hwRes.ok) setHardware(await hwRes.json())

      const rRes = await api('/api/v1/managed-models/runner-health')
      if (rRes.ok) {
        const nextRunner = await rRes.json()
        setRunner(nextRunner)
        // Seed the advanced fields from the settings the runner is actually
        // using, but only once a model is loaded: with an idle runner every
        // number reads back as zero, which would show "0 threads" and then be
        // rejected by this form's own validation. Hydration is a one-shot so
        // polling never overwrites what the user is typing.
        if (!managedConfigHydrated.current && Number(nextRunner.context_limit) > 0) {
          setManagedContextLimit(Number(nextRunner.context_limit))
          if (Number(nextRunner.threads) > 0) setManagedThreads(Number(nextRunner.threads))
          // The dropdown only offers auto / 0 / custom, so any other layer
          // count has to arrive as "custom" or the select would render blank.
          if (nextRunner.gpu_layers == null) {
            setManagedGpuLayers('auto')
          } else if (Number(nextRunner.gpu_layers) === 0) {
            setManagedGpuLayers('0')
          } else {
            setManagedGpuLayers('custom')
            setManagedCustomGpuLayers(String(nextRunner.gpu_layers))
          }
          setManagedExtraArgs(JSON.stringify(nextRunner.extra_args ?? [], null, 2))
          managedConfigHydrated.current = true
        }
      }

      const artRes = await api('/api/v1/managed-models/artifacts')
      if (artRes.ok) {
        const data = await artRes.json()
        setArtifacts(Array.isArray(data) ? data : [])
      }

      const sRes = await api('/api/v1/managed-models/storage')
      if (sRes.ok) {
        const storage = await sRes.json()
        setStorageInfo(storage)
        // Seeded once, so the field is not rewritten under someone typing in
        // it every time this polls.
        if (!budgetHydrated.current && Number(storage.budget_bytes) > 0) {
          setBudgetGB(String(Math.round(Number(storage.budget_bytes) / (1024 * 1024 * 1024))))
          budgetHydrated.current = true
        }
      }
    } catch {
      // ignore
    }
  }

  useEffect(() => {
    fetchModels()
    fetchLimits()
    fetchHardwareAndRunner()
    fetchContributionsSettings()
    fetchSetupInfo()
    fetchLocalAPIToken()
  }, [])

  // Keep the picker pointed at something real: the model the runner is
  // already serving when there is one, otherwise the first on disk.
  useEffect(() => {
    if ((artifacts || []).length === 0) {
      setSelectedArtifactID('')
      return
    }
    setSelectedArtifactID((prev) => {
      if (prev && artifacts.some((a) => a.id === prev)) return prev
      const loaded = artifacts.find((a) => (a.path || a.filename) === runner?.loaded_file)
      return (loaded || artifacts[0]).id
    })
  }, [artifacts, runner?.loaded_file])

  // A model's own context limit is the right default for it. The engine's
  // live value wins for the model already loaded, and this deliberately does
  // not re-run on every keystroke, so a typed value survives polling.
  useEffect(() => {
    const artifact = (artifacts || []).find((a) => a.id === selectedArtifactID)
    if (!artifact) return
    if ((artifact.path || artifact.filename) === runner?.loaded_file && Number(runner?.context_limit) > 0) return
    setManagedContextLimit(artifact.context_limit || 4096)
  }, [selectedArtifactID])

  const handleSelectArtifact = (id: string) => {
    setSelectedArtifactID(id)
    setLoadError('')
    setLoadSuccess('')
    setManagedConfigError('')
  }

  // Downloaded weights are handed to the loader rather than started from the
  // download list: loading is the other section's job.
  const handleRevealInLoader = (artifact: ArtifactManifest) => {
    handleSelectArtifact(artifact.id)
    document.getElementById('model-loader')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    document.getElementById('model-loader-select')?.focus({ preventScroll: true })
  }

  const handleAddModel = async (e: React.FormEvent) => {
    e.preventDefault()
    setModelError('')
    setModelSuccess('')

    try {
      const res = await api('/api/v1/hosted-models', {
        method: 'POST',
          body: JSON.stringify({
          name: name.trim(),
          endpoint_url: endpointUrl.trim(),
          api_key: apiKey.trim(),
          backend_model: backendModel.trim(),
          context_limit: Number(contextLimit),
          max_tokens: Number(maxTokens),
          enabled: true,
          published: published,
          allow_private_network: allowPrivateNetwork,
        }),
      })

      if (!res.ok) {
        const err = await res.text()
        throw new Error(err || 'Failed to save hosted model')
      }

      setModelSuccess('Hosted model saved successfully!')
      setName('')
      setBackendModel('')
      setEndpointUrl('')
      setApiKey('')
      setAllowPrivateNetwork(false)
      await fetchModels()
    } catch (err: any) {
      setModelError(err.message || 'Error saving model')
    }
  }

  const handleDeleteModel = async (id: string) => {
    if (!confirm('Remove this hosted model?')) return
    try {
      const res = await api(`/api/v1/hosted-models/${id}`, { method: 'DELETE' })
      if (res.ok) {
        await fetchModels()
      }
    } catch {
      // ignore
    }
  }

  const handleTestEndpoint = async () => {
    if (!endpointUrl.trim()) {
      setTestResult({ ok: false, message: 'Please enter an endpoint URL first' })
      return
    }
    setTestingEndpoint(true)
    setTestResult(null)
    try {
      const res = await api('/api/v1/hosted-models/test', {
        method: 'POST',
          body: JSON.stringify({
          endpoint_url: endpointUrl.trim(),
          api_key: apiKey.trim(),
          allow_private_network: allowPrivateNetwork,
        }),
      })
      const data = await res.json()
      if (data.ok) {
        setTestResult({
          ok: true,
          message: `Connected successfully! Latency: ${data.latency_ms}ms (${(data.models || []).length} models available)`,
        })
        if (data.models && data.models.length > 0) {
          setDiscoveredModels(data.models)
        }
      } else {
        setTestResult({
          ok: false,
          message: data.error || 'Endpoint test failed',
        })
      }
    } catch (err: any) {
      setTestResult({ ok: false, message: err.message || 'Failed to reach endpoint' })
    } finally {
      setTestingEndpoint(false)
    }
  }

  const handleDiscoverModels = async () => {
    if (!endpointUrl.trim()) {
      setTestResult({ ok: false, message: 'Please enter an endpoint URL first' })
      return
    }
    setDiscoveringModels(true)
    setTestResult(null)
    try {
      const res = await api('/api/v1/hosted-models/discover', {
        method: 'POST',
          body: JSON.stringify({
          endpoint_url: endpointUrl.trim(),
          api_key: apiKey.trim(),
          allow_private_network: allowPrivateNetwork,
        }),
      })
      const data = await res.json()
      if (data.ok && Array.isArray(data.models) && data.models.length > 0) {
        setDiscoveredModels(data.models)
        setTestResult({
          ok: true,
          message: `Found ${data.models.length} model(s). Click one below to auto-fill!`,
        })
      } else {
        setTestResult({
          ok: false,
          message: data.error || 'No models returned from endpoint',
        })
      }
    } catch (err: any) {
      setTestResult({ ok: false, message: err.message || 'Failed to discover models' })
    } finally {
      setDiscoveringModels(false)
    }
  }

  const handleTestExistingModel = async (id: string) => {
    setModelTestStatus((prev) => ({ ...prev, [id]: { testing: true } }))
    try {
      const res = await api('/api/v1/hosted-models/test', {
        method: 'POST',
          body: JSON.stringify({ id }),
      })
      const data = await res.json()
      if (data.ok) {
        setModelTestStatus((prev) => ({
          ...prev,
          [id]: { ok: true, latency: data.latency_ms },
        }))
      } else {
        setModelTestStatus((prev) => ({
          ...prev,
          [id]: { ok: false, error: data.error || 'Connection failed' },
        }))
      }
    } catch (err: any) {
      setModelTestStatus((prev) => ({
        ...prev,
        [id]: { ok: false, error: err.message || 'Test failed' },
      }))
    }
  }

  const handleSaveLimits = async (e: React.FormEvent) => {
    e.preventDefault()
    setLimitSuccess('')
    try {
      const res = await api('/api/v1/host-limits', {
        method: 'POST',
          body: JSON.stringify(limits),
      })
      if (res.ok) {
        setLimitSuccess('Host limits updated successfully!')
      }
    } catch {
      // ignore
    }
  }

  // Downloads run in the background on the server; this polls the job rather
  // than holding an HTTP request open for a multi-gigabyte transfer.
  const startDownloadJob = async (
    sourceUrl: string,
    filename: string,
    sha: string = '',
    companions: RepoCompanion[] = [],
  ) => {
    setDlLoading(true)
    setDlError('')
    setDlSuccess('')
    setDlProgress(null)
    try {
      const res = await api('/api/v1/managed-models/download', {
        method: 'POST',
        body: JSON.stringify({
          source_url: sourceUrl.trim(),
          filename: filename.trim(),
          expected_sha256: sha.trim(),
          // The projector and draft module come with the model. Which ones
          // they are follows from the model, so they are not a choice.
          companions: companions.map((c) => ({
            kind: c.kind,
            source_url: c.url,
            filename: c.filename,
            max_size_bytes: c.size_bytes,
          })),
        }),
      })
      if (!res.ok) {
        const msg = await res.text()
        throw new Error(msg || 'Download failed')
      }
      const { download_id: downloadID } = await res.json()

      for (;;) {
        await new Promise((resolve) => setTimeout(resolve, 1000))
        const statusRes = await api(`/api/v1/managed-models/downloads?id=${encodeURIComponent(downloadID)}`)
        if (!statusRes.ok) {
          throw new Error('Lost track of the download job')
        }
        const jobs = await statusRes.json()
        const job = Array.isArray(jobs) ? jobs[0] : null
        if (!job) {
          throw new Error('Download job disappeared')
        }
        setDlProgress(job.bytes_downloaded || 0)
        if (job.status === 'complete') {
          break
        }
        if (job.status === 'failed' || job.status === 'cancelled') {
          throw new Error(job.error || `Download ${job.status}`)
        }
      }

      setDlSuccess(`GGUF model ${filename} downloaded and verified successfully!`)
      setDlUrl('')
      setDlFilename('')
      setDlSha('')
      await fetchHardwareAndRunner()
    } catch (err: any) {
      setDlError(err.message || 'Download error')
    } finally {
      setDlLoading(false)
      setDlProgress(null)
    }
  }

  const handleSaveBudget = async (e: React.FormEvent) => {
    e.preventDefault()
    setBudgetMsg('')
    const gb = Number(budgetGB)
    if (!Number.isFinite(gb) || gb <= 0) {
      setBudgetMsg('Enter a limit of at least 1 GB.')
      return
    }
    try {
      setSavingBudget(true)
      const res = await api('/api/v1/managed-models/storage', {
        method: 'POST',
        body: JSON.stringify({ budget_bytes: Math.round(gb * 1024 * 1024 * 1024) }),
      })
      if (!res.ok) {
        setBudgetMsg((await res.text()) || 'Could not save the storage limit.')
        return
      }
      setStorageInfo(await res.json())
      setBudgetMsg('Saved.')
      setTimeout(() => setBudgetMsg(''), 4000)
    } catch (err: any) {
      setBudgetMsg(err.message || 'Could not save the storage limit.')
    } finally {
      setSavingBudget(false)
    }
  }

  const handleDownloadArtifact = async (e: React.FormEvent) => {
    e.preventDefault()
    await startDownloadJob(dlUrl, dlFilename, dlSha)
  }

  const handleResolveHfUrl = async (e: React.FormEvent) => {
    e.preventDefault()
    setHfResolveMsg('')
    setHfFiles([])
    const input = hfResolveInput.trim()
    if (!input) return

    // A direct link to a file needs no lookup.
    if (input.endsWith('.gguf')) {
      const parts = input.split('/')
      const fname = parts[parts.length - 1]
      setDlUrl(input)
      setDlFilename(fname)
      setHfResolveMsg(`Filled in the form for ${fname}`)
      return
    }

    // Otherwise ask Hugging Face what the repository holds. Guessing a
    // filename from the repo name was wrong for most repositories, and could
    // never know which quantizations were published.
    setHfLoading(true)
    try {
      const res = await api(`/api/v1/managed-models/huggingface?repo=${encodeURIComponent(input)}`)
      if (!res.ok) {
        setHfResolveMsg(await res.text())
        return
      }
      const data: { repo: string; files: RepoWeight[] } = await res.json()
      setHfFiles(data.files || [])
      setHfResolveMsg(`${(data.files || []).length} model files in ${data.repo} — pick one`)
    } catch (err: any) {
      setHfResolveMsg(err.message || 'lookup failed')
    } finally {
      setHfLoading(false)
    }
  }

  const handlePickHfFile = (file: RepoWeight) => {
    setDlUrl(file.url)
    setDlFilename(file.filename)
    setHfResolveMsg(`Filled in the form for ${file.filename}`)
  }

  const handleLoadArtifactIntoRunner = async (artifact: ArtifactManifest) => {
    setLoadError('')
    setLoadSuccess('')
    setManagedConfigError('')
    const selectedContext = Number(managedContextLimit) || artifact.context_limit || 4096
    if (!Number.isInteger(selectedContext) || selectedContext < 256) {
      setManagedConfigError('Context window must be a whole number of at least 256 tokens.')
      return
    }
    if (!Number.isInteger(managedThreads) || managedThreads < 1) {
      setManagedConfigError('CPU threads must be a positive whole number.')
      return
    }
    const calculatedMaxTokens = Math.min(4096, Math.max(256, Math.floor(selectedContext / 2)), selectedContext)
    let extraArgs: string[] = []
    try {
      const parsed = JSON.parse(managedExtraArgs)
      if (!Array.isArray(parsed) || parsed.some((item) => typeof item !== 'string')) {
        throw new Error('Extra arguments must be a JSON array of strings.')
      }
      extraArgs = parsed
    } catch (err: any) {
      setManagedConfigError(err.message || 'Extra arguments must be valid JSON.')
      return
    }
    const gpuLayers = managedGpuLayers === 'auto' ? undefined : Number(managedGpuLayers === 'custom' ? managedCustomGpuLayers : managedGpuLayers)
    if (gpuLayers !== undefined && (!Number.isInteger(gpuLayers) || gpuLayers < 0)) {
      setManagedConfigError('GPU offload must be Auto or a non-negative whole number of layers.')
      return
    }
    // The request is held open until the engine reports healthy, which for a
    // large model is a minute or more, so the button and the panel below it
    // show the wait instead of looking inert.
    setLoadingModelID(artifact.id)
    try {
      const res = await api('/api/v1/managed-models/load', {
        method: 'POST',
        body: JSON.stringify({
          // The artifact id is stable and safe in a URL segment; the path it
          // was found at is not, and two models in different directories can
          // share a filename.
          model_id: artifact.id,
          name: artifact.name,
          filename: artifact.path || artifact.filename,
          context_limit: selectedContext,
          max_tokens: calculatedMaxTokens,
          threads: Number(managedThreads) || 4,
          ...(gpuLayers !== undefined ? { gpu_layers: gpuLayers } : {}),
          extra_args: extraArgs,
          published: models.find((model) => model.id === artifact.id)?.published ?? true,
        }),
      })
      if (res.ok) {
        // Loading now also creates the hosted-model row, so the catalog
        // reflects it immediately.
        await fetchModels()
        await fetchHardwareAndRunner()
        setLoadSuccess(`${artifact.name} is loaded and serving.`)
        // The runner status tab is where the freshly started engine can
        // actually be watched, so the load ends there.
        setActiveTab('status')
      } else {
        setLoadError(await res.text())
      }
    } catch (err: any) {
      setLoadError(err.message)
    } finally {
      setLoadingModelID('')
    }
  }

  const handleUnloadRunner = async () => {
    try {
      const res = await api('/api/v1/managed-models/unload', { method: 'POST', body: '{}' })
      if (res.ok) {
        setLoadSuccess('')
        setLoadError('')
        await fetchModels()
        await fetchHardwareAndRunner()
      }
    } catch {
      // ignore
    }
  }

  const handleDeleteArtifact = async (artifact: ArtifactManifest) => {
    if (!confirm(`Delete ${artifact.filename}?`)) return
    try {
      // The path may contain separators, and each segment is escaped
      // individually so the server still sees a path.
      const ref = (artifact.path || artifact.filename).split('/').map(encodeURIComponent).join('/')
      const res = await api(`/api/v1/managed-models/artifacts/${ref}`, {
        method: 'DELETE',
      })
      if (!res.ok) {
        alert(await res.text())
        return
      }
      await fetchHardwareAndRunner()
    } catch {
      // ignore
    }
  }

  return (
    <div className="settings-page">
      <div className="settings-intro">
        <div>
          <span className="settings-eyebrow">NODE SETTINGS</span>
          <h1>Make this node yours</h1>
          <p>Manage your runner, model library, connected endpoints, and sharing controls from one place.</p>
        </div>
        <span className={`settings-status-pill${runner?.status === 'ready' || runner?.status === 'idle' ? ' is-ready' : ''}`}>
          <span aria-hidden="true">●</span>{' '}
          {!runner?.configured ? 'No managed runner' : `Runner ${runner.status || 'unknown'}`}
        </span>
      </div>
      <nav className="settings-tabs" aria-label="Settings sections" role="tablist">
        {([
          ['status', 'Runner status', 'Live model and queue'],
          ['managed', 'Managed models', 'Library and llama.cpp'],
          ['connected', 'Connected models', 'External endpoints'],
          ['hosting', 'Hosting', 'Limits and contributions'],
          ['access', 'Access & API', 'Pairing and compatibility'],
        ] as [SettingsTab, string, string][]).map(([id, label, description]) => (
          <button
            key={id}
            id={`settings-tab-${id}`}
            type="button"
            role="tab"
            aria-selected={activeTab === id}
            aria-controls={`settings-panel-${id}`}
            tabIndex={activeTab === id ? 0 : -1}
            className={`settings-tab ${activeTab === id ? 'active' : ''}`}
            onClick={() => setActiveTab(id)}
            onKeyDown={(event) => {
              if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
              event.preventDefault()
              const ids: SettingsTab[] = ['status', 'managed', 'connected', 'hosting', 'access']
              const next = ids[(ids.indexOf(id) + (event.key === 'ArrowRight' ? 1 : ids.length - 1)) % ids.length]
              setActiveTab(next)
              document.getElementById(`settings-tab-${next}`)?.focus()
            }}
          >
            <span>{label}</span><small>{description}</small>
          </button>
        ))}
      </nav>
      <div className="settings-tab-panels">
      {activeTab === 'status' && (
        <section id="settings-panel-status" role="tabpanel" aria-labelledby="settings-tab-status" tabIndex={0}>
          <RunnerStatus modelNames={Object.fromEntries(models.map((model) => [model.id, model.name]))} />
        </section>
      )}
      {activeTab === 'access' && (
      <section id="settings-panel-access" role="tabpanel" aria-labelledby="settings-tab-access" tabIndex={0}>
      {/* Device Access Section */}
      <div className="card">
        <h2>📱 Connect Another Device</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          Pairing uses a one-time setup secret that stops working the moment it
          is redeemed. The secret is shown here once and is never stored in a
          link: a credential in a URL ends up in browser history, referrers, and
          proxy logs.
        </p>

        <div style={{ marginBottom: '1rem', fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
          Current status:{' '}
          <strong style={{ color: setupRedeemed ? 'var(--text-secondary)' : 'var(--accent-primary)' }}>
            {setupRedeemed ? 'no unredeemed secret outstanding' : 'a secret is issued and waiting to be used'}
          </strong>
        </div>

        {mintedSecret && (
          <div className="form-group" style={{ marginBottom: '1.25rem' }}>
            <label style={{ fontSize: '0.85rem', fontWeight: 600 }}>New one-time secret (shown once)</label>
            <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.25rem' }}>
              <input
                type="text"
                readOnly
                className="form-control"
                style={{ fontSize: '0.85rem', background: 'var(--bg-primary)', fontFamily: 'monospace' }}
                value={mintedSecret}
              />
              <button
                type="button"
                className="btn btn-secondary"
                style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
                onClick={copySetupToken}
              >
                {copiedSetupToken ? 'Copied!' : 'Copy'}
              </button>
            </div>
          </div>
        )}

        <form onSubmit={handleUpdatePin} style={{ borderTop: '1px solid var(--border-color)', paddingTop: '1rem' }}>
          <label style={{ fontSize: '0.85rem', fontWeight: 600 }}>Issue a new setup secret</label>
          <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', margin: '0.2rem 0 0.5rem 0' }}>
            Leave the field blank for a random 128-bit secret, or supply a
            passphrase of at least 12 characters. Anything shorter is guessable
            over a LAN, so short PINs are refused.
          </p>
          {pinError && <div className="alert alert-error" style={{ fontSize: '0.85rem', padding: '0.5rem', marginBottom: '0.5rem' }}>{pinError}</div>}
          {pinSuccess && <div className="alert alert-success" style={{ fontSize: '0.85rem', padding: '0.5rem', marginBottom: '0.5rem' }}>{pinSuccess}</div>}
          <div style={{ display: 'flex', gap: '0.5rem' }}>
            <input
              type="text"
              className="form-control"
              placeholder="blank for a random secret, or a passphrase of 12+ characters"
              value={newSetupPin}
              onChange={(e) => setNewSetupPin(e.target.value)}
              style={{ fontSize: '0.85rem' }}
            />
            <button
              type="submit"
              className="btn btn-primary"
              style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
              disabled={updatingPin}
            >
              {updatingPin ? 'Issuing...' : 'Issue Secret'}
            </button>
          </div>
        </form>
      </div>
      {/* Local API token for OpenAI-compatible clients */}
      <div className="card">
        <h2>🔑 Local API Token</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          OpenAI-compatible clients must send this as a bearer token on{' '}
          <code>/v1/models</code> and <code>/v1/chat/completions</code>. It is
          always required: these routes drive inference on other members'
          hardware, so leaving them open to any page in your browser is not an
          option.
        </p>

        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <input
            type={showLocalAPIToken ? 'text' : 'password'}
            readOnly
            className="form-control"
            style={{ fontSize: '0.85rem', background: 'var(--bg-primary)', fontFamily: 'monospace' }}
            value={localAPIToken}
          />
          <button
            type="button"
            className="btn btn-secondary"
            style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
            onClick={() => setShowLocalAPIToken(!showLocalAPIToken)}
          >
            {showLocalAPIToken ? 'Hide' : 'Reveal'}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
            onClick={() => {
              navigator.clipboard.writeText(localAPIToken)
              setCopiedLocalAPIToken(true)
              setTimeout(() => setCopiedLocalAPIToken(false), 2000)
            }}
          >
            {copiedLocalAPIToken ? 'Copied!' : 'Copy'}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
            onClick={regenerateLocalAPIToken}
            disabled={regeneratingToken}
          >
            {regeneratingToken ? 'Rotating...' : 'Regenerate'}
          </button>
        </div>
      </div>

      {/* Local compatibility details stay with access credentials. */}
      <div className="card">
        <h2>Local Compatibility Endpoint</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1rem' }}>
          Connect OpenAI-compatible tools to this Woolwire instance.
        </p>
        <div className="settings-code-block">
          <div><strong>Base URL:</strong> <code>http://127.0.0.1:7070/v1</code></div>
          <div><strong>Chat Completions:</strong> <code>POST /v1/chat/completions</code></div>
          <div><strong>Models List:</strong> <code>GET /v1/models</code></div>
        </div>
      </div>
      </section>
      )}

      {/* Hardware & Managed Deployment Section */}
      {activeTab === 'managed' && (
      <section id="settings-panel-managed" role="tabpanel" aria-labelledby="settings-tab-managed" tabIndex={0}>
      <div className="card">
        <h2>Hardware & Deployment Profile</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          Deployment container resources and companion runner configuration.
        </p>

        {hardware && (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: '1rem', marginBottom: '1.5rem' }}>
            <div style={{ padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>CPU Platform</div>
              <div style={{ fontSize: '1rem', fontWeight: 600 }}>{hardware.os} / {hardware.arch} ({hardware.cpu_cores} cores)</div>
            </div>

            <div style={{ padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>System Memory</div>
              <div style={{ fontSize: '1rem', fontWeight: 600 }}>
                {hardware.total_ram_mb > 0 ? `${(hardware.total_ram_mb / 1024).toFixed(1)} GB RAM` : 'N/A'}
              </div>
            </div>

            <div style={{ padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>GPU Acceleration</div>
              <div style={{ fontSize: '1rem', fontWeight: 600, color: gpuAvailable ? 'var(--accent-success)' : 'inherit' }}>
                {gpuAvailable ? gpuLabel : 'CPU Only'}
              </div>
              {runner?.configured && (
                <div style={{ fontSize: '0.7rem', color: 'var(--text-secondary)', marginTop: '0.15rem' }}>
                  {runner.has_gpu
                    ? 'reported by the runner, which sizes the offload to free VRAM'
                    : 'the runner has no GPU attached; models run on the CPU'}
                </div>
              )}
            </div>

            <div style={{ padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>Managed Runner</div>
              <div style={{ fontSize: '1rem', fontWeight: 600 }}>
                {runner?.configured ? (
                  <span style={{ color: runner.status === 'ready' ? 'var(--accent-success)' : runner.status === 'error' ? 'var(--accent-error, #d9534f)' : 'var(--accent-primary)' }}>
                    {runner.status.toUpperCase()}{loadedModelName ? ` (${loadedModelName})` : ''}
                  </span>
                ) : (
                  <span style={{ color: 'var(--text-secondary)' }}>Not Configured</span>
                )}
              </div>
              {(runner?.engine_error || runner?.error) && (
                <div style={{ fontSize: '0.7rem', color: 'var(--text-secondary)', marginTop: '0.25rem', overflowWrap: 'anywhere' }}>
                  {runner.engine_error || runner.error}
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      {/* Loading a model is its own job: pick one of the models on disk, set
          the launch parameters, then start the engine. Downloading weights is
          a different job and lives in its own card below. */}
      <div className="card" id="model-loader">
        <h2>Load a Model</h2>
        <p className="settings-lede">
          Choose one of the GGUF models on this node, set how it should run, and start the
          engine. Loading replaces whatever the runner is serving now.
        </p>

        {!runner?.configured ? (
          <p className="settings-muted">
            No managed runner is attached to this node, so models cannot be loaded here.
            Downloading weights still works from the section below.
          </p>
        ) : (artifacts || []).length === 0 ? (
          <p className="settings-muted">
            No GGUF language models found in the models directory. Woolwire scans it, including
            a Hugging Face cache laid out as <code>hub/models--org--repo/snapshots/...</code>, so
            pointing it at weights you already have is enough. You can also download models in
            the section below — those are written into that same cache layout, so other tools on
            this machine find them too.
          </p>
        ) : (
          <>
            {loadError && (
              <div className="alert alert-error" style={{ fontSize: '0.8rem', overflowWrap: 'anywhere', marginBottom: '1rem' }}>
                {loadError}
              </div>
            )}
            {loadSuccess && (
              <div className="alert alert-success" style={{ fontSize: '0.85rem', marginBottom: '1rem' }}>
                {loadSuccess}
              </div>
            )}

            <div className="form-group">
              <label htmlFor="model-loader-select">Downloaded model</label>
              <select
                id="model-loader-select"
                className="form-control"
                value={selectedArtifactID}
                onChange={(event) => handleSelectArtifact(event.target.value)}
                disabled={loadingModel}
              >
                {(artifacts || []).map((a) => (
                  <option key={a.id} value={a.id}>
                    {a.name}
                    {' — '}
                    {(a.size_bytes / (1024 * 1024 * 1024)).toFixed(2)} GB
                    {a.architecture ? ` · ${a.architecture}` : ''}
                    {runner?.loaded_file === (a.path || a.filename) ? ' · loaded now' : ''}
                  </option>
                ))}
              </select>
            </div>

            <div className="settings-grid settings-grid-3">
              <div className="form-group">
                <label htmlFor="managed-context">Context window</label>
                <input
                  id="managed-context"
                  className="form-control"
                  type="number"
                  min={256}
                  step={256}
                  value={managedContextLimit}
                  onChange={(event) => setManagedContextLimit(Number(event.target.value))}
                  disabled={loadingModel}
                  aria-describedby="managed-context-help"
                />
                <small id="managed-context-help">Tokens the engine keeps in memory. Larger windows cost more RAM or VRAM.</small>
              </div>
              <div className="form-group">
                <label htmlFor="managed-threads">CPU threads</label>
                <input
                  id="managed-threads"
                  className="form-control"
                  type="number"
                  min={1}
                  value={managedThreads}
                  onChange={(event) => setManagedThreads(Number(event.target.value))}
                  disabled={loadingModel}
                  aria-describedby="managed-threads-help"
                />
                <small id="managed-threads-help">
                  {hardware?.cpu_cores ? `This machine reports ${hardware.cpu_cores} cores.` : 'Used for layers that stay on the CPU.'}
                </small>
              </div>
              <div className="form-group">
                <label htmlFor="managed-gpu-layers">GPU offload</label>
                <select
                  id="managed-gpu-layers"
                  className="form-control"
                  value={managedGpuLayers}
                  onChange={(event) => setManagedGpuLayers(event.target.value)}
                  disabled={loadingModel}
                  aria-describedby="managed-gpu-help"
                >
                  <option value="auto">Auto (recommended)</option>
                  <option value="0">CPU only (0 layers)</option>
                  <option value="custom">Custom…</option>
                </select>
                {managedGpuLayers === 'custom' && (
                  <input
                    className="form-control"
                    type="number"
                    min={0}
                    aria-label="Custom GPU layers"
                    value={managedCustomGpuLayers}
                    onChange={(event) => setManagedCustomGpuLayers(event.target.value)}
                    disabled={loadingModel}
                    style={{ marginTop: '0.35rem' }}
                  />
                )}
                <small id="managed-gpu-help">
                  {gpuAvailable ? 'Auto sizes the offload to free VRAM on the runner.' : 'No GPU is attached to the runner; layers stay on the CPU.'}
                </small>
              </div>
            </div>

            <details className="settings-advanced-panel model-loader-advanced">
              <summary>Advanced llama.cpp arguments</summary>
              <div className="form-group" style={{ marginTop: '0.75rem' }}>
                <label htmlFor="managed-extra-args">Extra arguments</label>
                <textarea
                  id="managed-extra-args"
                  className="form-control settings-code-input"
                  rows={3}
                  value={managedExtraArgs}
                  onChange={(event) => setManagedExtraArgs(event.target.value)}
                  disabled={loadingModel}
                  aria-describedby="managed-extra-help"
                />
                <small id="managed-extra-help">
                  JSON array of strings, for example <code>[&quot;--no-mmap&quot;]</code>. Woolwire validates each item before sending it to the runner.
                </small>
              </div>
            </details>

            {managedConfigError && <div className="alert alert-error" role="alert" style={{ fontSize: '0.8rem' }}>{managedConfigError}</div>}

            <div className="model-loader-actions">
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => selectedArtifact && handleLoadArtifactIntoRunner(selectedArtifact)}
                disabled={loadingModel || !selectedArtifact}
              >
                {loadingModel ? (
                  <>
                    <span className="model-loader-spinner" aria-hidden="true" />
                    Loading…
                  </>
                ) : selectedIsLoaded ? (
                  'Reload Model'
                ) : (
                  'Load Model'
                )}
              </button>
              {runner?.loaded_model_id && (
                <button type="button" className="btn btn-secondary" onClick={handleUnloadRunner} disabled={loadingModel}>
                  Unload current model
                </button>
              )}
              {selectedArtifact && selectedArtifact.source !== 'cache' && (
                <button
                  type="button"
                  className="btn btn-secondary"
                  style={{ color: 'var(--accent-danger)' }}
                  onClick={() => handleDeleteArtifact(selectedArtifact)}
                  disabled={loadingModel}
                >
                  Delete weights
                </button>
              )}
              {selectedIsLoaded && !loadingModel && (
                <span className="badge" style={{ backgroundColor: 'var(--accent-success)' }}>Loaded</span>
              )}
            </div>

            {/* The load request does not return until the engine reports
                healthy, so the wait is real and needs to be visible. */}
            {loadingModel && (
              <div className="model-loader-progress" role="status" aria-live="polite">
                <div className="model-loader-bar"><span /></div>
                <p>
                  Starting llama.cpp with <strong>{loadingName}</strong> and waiting for it to report
                  healthy. A large model can take a minute or two the first time it is read from disk.
                </p>
              </div>
            )}
          </>
        )}
      </div>

      {/* Downloading weights is a separate function from loading them, so it
          gets its own card rather than sitting under the model list. */}
      <div className="card">
        <h2>Download Models</h2>
        <p className="settings-lede">
          Fetch GGUF weights over HTTPS into this node's models directory. Downloads run in the
          background on the server; a finished model shows up in the loader above.
        </p>

        {storageInfo && !storageInfo.configured && (
          <p className="settings-muted">
            No models directory is configured on this node, so downloads are unavailable.
          </p>
        )}

        {storageInfo && storageInfo.configured && (
          <>
            {/* Storage Usage Meter */}
            {storageInfo.budget_bytes ? (
              <div style={{ marginBottom: '1.5rem', padding: '0.85rem 1rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.85rem', marginBottom: '0.4rem' }}>
                  <span><strong>Model Storage Usage:</strong> {((storageInfo.used_bytes || 0) / (1024 * 1024 * 1024)).toFixed(2)} GB / {((storageInfo.budget_bytes || 0) / (1024 * 1024 * 1024)).toFixed(0)} GB</span>
                  <span style={{ color: 'var(--text-secondary)' }}>{(((storageInfo.used_bytes || 0) / (storageInfo.budget_bytes || 1)) * 100).toFixed(1)}% used</span>
                </div>
                <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginBottom: '0.4rem' }}>
                  Counts weights Woolwire downloaded. Models found in the directory are served but
                  not charged against the budget, since Woolwire did not put them there.
                  {storageInfo.read_only && ' The models directory is read-only, so downloads are disabled.'}
                </div>
                <div style={{ height: '8px', backgroundColor: 'var(--bg-secondary)', borderRadius: '4px', overflow: 'hidden' }}>
                  <div style={{ height: '100%', width: `${Math.min(100, (((storageInfo.used_bytes || 0) / (storageInfo.budget_bytes || 1)) * 100))}%`, backgroundColor: 'var(--accent-primary)', transition: 'width 0.3s ease' }} />
                </div>

                {/* The budget used to be a constant nobody could reach. How
                    much disk this node gives models is the operator's call. */}
                <form className="storage-budget" onSubmit={handleSaveBudget}>
                  <label htmlFor="storage-budget-input">Storage limit</label>
                  <input
                    id="storage-budget-input"
                    className="form-control"
                    type="number"
                    min={1}
                    step={1}
                    value={budgetGB}
                    onChange={(event) => setBudgetGB(event.target.value)}
                    disabled={savingBudget}
                    aria-describedby="storage-budget-help"
                  />
                  <span className="storage-budget__unit">GB</span>
                  <button type="submit" className="btn btn-secondary" disabled={savingBudget}>
                    {savingBudget ? 'Saving…' : 'Save limit'}
                  </button>
                  {budgetMsg && <span className="storage-budget__msg">{budgetMsg}</span>}
                </form>
                <div id="storage-budget-help" style={{ fontSize: '0.72rem', color: 'var(--text-secondary)', marginTop: '0.35rem' }}>
                  Lowering the limit below what is already installed blocks new downloads; it never
                  deletes weights you already have.
                </div>
              </div>
            ) : null}

            {/* Curated Popular Hugging Face Models Section */}
            <div style={{ marginBottom: '1.75rem', backgroundColor: 'var(--bg-primary)', padding: '1rem', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.75rem', flexWrap: 'wrap', gap: '0.5rem' }}>
                <div>
                  <h4 style={{ fontSize: '0.95rem', margin: 0, fontWeight: 600 }}>🌟 Popular Hugging Face Models (1-Click Download)</h4>
                  <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', margin: '0.2rem 0 0 0' }}>
                    Verified GGUF weights with automatic hardware fit indicators based on your detected CPU, RAM, and GPU.
                  </p>
                </div>
                <div style={{ display: 'flex', gap: '0.35rem', flexWrap: 'wrap' }}>
                  {['All', 'Edge & Fast', 'Coding', 'Reasoning', 'General'].map((cat) => (
                    <button
                      key={cat}
                      type="button"
                      className={`btn ${popularCategory === cat ? 'btn-primary' : 'btn-secondary'}`}
                      style={{ padding: '0.25rem 0.6rem', fontSize: '0.75rem' }}
                      onClick={() => setPopularCategory(cat)}
                    >
                      {cat}
                    </button>
                  ))}
                </div>
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 280px), 1fr))', gap: '0.75rem' }}>
                {POPULAR_MODELS.filter((m) => popularCategory === 'All' || m.category === popularCategory).map((m) => {
                  const installed = (artifacts || []).find((a) => a.filename === m.filename)
                  const isInstalled = installed !== undefined
                  const isLoaded = runner?.loaded_file === (installed?.path || m.filename)
                  const fit = getHardwareFitBadge(m.sizeMB, hardware)

                  return (
                    <div
                      key={m.id}
                      style={{
                        padding: '0.85rem',
                        borderRadius: 'var(--radius)',
                        border: '1px solid var(--border-color)',
                        backgroundColor: 'var(--bg-secondary)',
                        display: 'flex',
                        flexDirection: 'column',
                        justifyContent: 'space-between',
                        gap: '0.6rem',
                      }}
                    >
                      <div>
                        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '0.25rem' }}>
                          <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>{m.name}</div>
                          <span className="badge" style={{ fontSize: '0.65rem', backgroundColor: 'var(--bg-primary)' }}>{m.creator}</span>
                        </div>
                        <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginBottom: '0.4rem', lineHeight: '1.3' }}>
                          {m.description}
                        </div>
                        <div style={{ display: 'flex', gap: '0.35rem', flexWrap: 'wrap', alignItems: 'center' }}>
                          <span style={{ fontSize: '0.7rem', backgroundColor: 'var(--bg-primary)', padding: '0.15rem 0.4rem', borderRadius: '4px' }}>
                            {(m.sizeMB / 1024).toFixed(1)} GB &bull; {m.quant}
                          </span>
                          <span style={{ fontSize: '0.7rem', backgroundColor: 'var(--bg-primary)', padding: '0.15rem 0.4rem', borderRadius: '4px' }}>
                            {m.contextLimit / 1024}k Ctx
                          </span>
                          <span
                            style={{ fontSize: '0.7rem', color: fit.color, fontWeight: 500 }}
                            title={fit.tooltip}
                          >
                            {fit.label}
                          </span>
                        </div>
                      </div>

                      <div style={{ display: 'flex', gap: '0.4rem', marginTop: '0.2rem' }}>
                        {isLoaded ? (
                          <span className="badge" style={{ backgroundColor: 'var(--accent-success)', width: '100%', textAlign: 'center', padding: '0.4rem' }}>
                            Loaded Active
                          </span>
                        ) : isInstalled ? (
                          // Already on disk: loading is the loader's job, so this
                          // hands the model to it rather than starting an engine
                          // from inside the download list.
                          <button
                            type="button"
                            className="btn btn-secondary"
                            style={{ flex: 1, padding: '0.35rem', fontSize: '0.8rem' }}
                            onClick={() => installed && handleRevealInLoader(installed)}
                          >
                            Downloaded — select to load
                          </button>
                        ) : (
                          <>
                            <button
                              type="button"
                              className="btn btn-primary"
                              style={{ flex: 1, padding: '0.35rem', fontSize: '0.8rem' }}
                              onClick={() => startDownloadJob(m.url, m.filename)}
                              disabled={dlLoading}
                            >
                              {dlLoading ? 'Downloading...' : '1-Click Download'}
                            </button>
                            <button
                              type="button"
                              className="btn btn-secondary"
                              style={{ padding: '0.35rem 0.5rem', fontSize: '0.8rem' }}
                              title="Copy details to the custom download form below"
                              onClick={() => {
                                setDlUrl(m.url)
                                setDlFilename(m.filename)
                                setDlSha('')
                              }}
                              disabled={dlLoading}
                            >
                              Fill
                            </button>
                          </>
                        )}
                      </div>
                    </div>
                  )
                })}
              </div>
            </div>

            {/* Hugging Face Quick Resolver Helper */}
            <div style={{ marginBottom: '1.5rem', padding: '1rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <h4 style={{ fontSize: '0.9rem', margin: '0 0 0.35rem 0' }}>🤗 Hugging Face Quick Resolver</h4>
              <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', margin: '0 0 0.75rem 0' }}>
                Paste a Hugging Face model URL or repo ID (e.g. <code>unsloth/DeepSeek-R1-Distill-Qwen-8B-GGUF</code>)
                to list the weight files it publishes, then pick the quantization you want. A direct
                <code>.gguf</code> link fills the form straight in.
              </p>
              <form onSubmit={handleResolveHfUrl} style={{ display: 'flex', gap: '0.5rem' }}>
                <input
                  type="text"
                  className="form-control"
                  placeholder="https://huggingface.co/org/repo or org/repo"
                  value={hfResolveInput}
                  onChange={(e) => setHfResolveInput(e.target.value)}
                  style={{ fontSize: '0.85rem' }}
                  disabled={dlLoading}
                />
                <button type="submit" className="btn btn-secondary" style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }} disabled={dlLoading || hfLoading}>
                  {hfLoading ? 'Looking up...' : 'List Models'}
                </button>
              </form>
              {hfResolveMsg && <div style={{ fontSize: '0.8rem', color: 'var(--accent-text)', marginTop: '0.4rem' }}>{hfResolveMsg}</div>}

              {hfFiles.length > 0 && (
                <div style={{ marginTop: '0.75rem', maxHeight: '18rem', overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: '0.35rem' }}>
                  {hfFiles.map((f) => {
                    const fit = getHardwareFitBadge((f.total_bytes || f.size_bytes) / (1024 * 1024), hardware)
                    const isSplit = (f.shard_count || 0) > 1
                    return (
                      <div
                        key={f.path}
                        style={{
                          display: 'flex',
                          justifyContent: 'space-between',
                          alignItems: 'center',
                          gap: '0.75rem',
                          padding: '0.4rem 0.6rem',
                          backgroundColor: 'var(--bg-secondary)',
                          borderRadius: 'var(--radius)',
                          border: '1px solid var(--border-color)',
                        }}
                      >
                        <div style={{ minWidth: 0 }}>
                          <div style={{ fontSize: '0.8rem', fontWeight: 600, overflowWrap: 'anywhere' }}>{f.filename}</div>
                          <div style={{ fontSize: '0.72rem', color: 'var(--text-secondary)' }}>
                            {(f.size_bytes / (1024 * 1024 * 1024)).toFixed(2)} GB
                            {f.quantization && <> &bull; {f.quantization}</>}
                            {!isSplit && <> &bull; <span style={{ color: fit.color }} title={fit.tooltip}>{fit.label}</span></>}
                            {isSplit && <> &bull; split across {f.shard_count} files</>}
                          </div>
                          {(f.companions || []).length > 0 && (
                            <div style={{ fontSize: '0.7rem', color: 'var(--text-secondary)', opacity: 0.85 }}>
                              includes{' '}
                              {(f.companions || [])
                                .map((c) => (c.kind === 'projector' ? `vision projector (${c.filename})` : `MTP draft module (${c.filename})`))
                                .join(' and ')}
                              {' '}&bull; {(f.total_bytes / (1024 * 1024 * 1024)).toFixed(2)} GB total
                            </div>
                          )}
                        </div>
                        {isSplit ? (
                          <span
                            style={{ fontSize: '0.72rem', color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}
                            title="Every shard has to be present before the model will load, and the downloader fetches one file at a time. Fetch it with huggingface-cli into your models directory instead."
                          >
                            multi-part
                          </span>
                        ) : (
                          <div style={{ display: 'flex', gap: '0.35rem' }}>
                            <button
                              type="button"
                              className="btn btn-primary"
                              style={{ padding: '0.25rem 0.6rem', fontSize: '0.75rem', whiteSpace: 'nowrap' }}
                              onClick={() => startDownloadJob(f.url, f.filename, '', f.companions || [])}
                              disabled={dlLoading}
                            >
                              Download
                            </button>
                            <button
                              type="button"
                              className="btn btn-secondary"
                              style={{ padding: '0.25rem 0.6rem', fontSize: '0.75rem', whiteSpace: 'nowrap' }}
                              onClick={() => handlePickHfFile(f)}
                              disabled={dlLoading}
                              title="Fill the form below instead of downloading now"
                            >
                              Use
                            </button>
                          </div>
                        )}
                      </div>
                    )
                  })}
                </div>
              )}
            </div>

            {/* Custom Download Artifact Form */}
            <h4 style={{ fontSize: '0.9rem', marginBottom: '0.75rem' }}>Custom Model Download (HTTPS GGUF)</h4>
            {dlError && <div className="alert alert-error">{dlError}</div>}
            {dlSuccess && <div className="alert alert-success">{dlSuccess}</div>}

            <form onSubmit={handleDownloadArtifact}>
              <div className="settings-fields" style={{ display: 'grid', gridTemplateColumns: '2fr 1fr 1fr', gap: '0.75rem' }}>
                <input
                  type="url"
                  className="form-control"
                  placeholder="HTTPS Download URL (e.g. HuggingFace direct URL)"
                  value={dlUrl}
                  onChange={(e) => setDlUrl(e.target.value)}
                  required
                  disabled={dlLoading}
                />
                <input
                  type="text"
                  className="form-control"
                  placeholder="Filename (e.g. llama-3.gguf)"
                  value={dlFilename}
                  onChange={(e) => setDlFilename(e.target.value)}
                  required
                  disabled={dlLoading}
                />
                <input
                  type="text"
                  className="form-control"
                  placeholder="Expected SHA-256 (Optional)"
                  value={dlSha}
                  onChange={(e) => setDlSha(e.target.value)}
                  disabled={dlLoading}
                />
              </div>

              <button type="submit" className="btn btn-primary" style={{ marginTop: '0.75rem' }} disabled={dlLoading}>
                {dlLoading
                  ? dlProgress !== null
                    ? `Downloading... ${(dlProgress / (1024 * 1024)).toFixed(1)} MiB`
                    : 'Starting download...'
                  : 'Download & Verify GGUF'}
              </button>
            </form>
          </>
        )}
      </div>

      {/* Hosted Models Section */}
      </section>
      )}
      {activeTab === 'connected' && (
      <section id="settings-panel-connected" role="tabpanel" aria-labelledby="settings-tab-connected" tabIndex={0}>
      <div className="card">
        <h2>External Hosted Models</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          Connect your local inference engines (Ollama, llama.cpp server, or vLLM) and share them with the room.
        </p>

        {(models || []).length > 0 && (
          <div style={{ marginBottom: '2rem', display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
            {(models || []).map((m) => {
              const mId = m.id || (m as any).ID
              const mName = m.name || (m as any).Name
              const mUrl = m.endpoint_url || (m as any).EndpointURL
              const mCtx = m.context_limit ?? (m as any).ContextLimit ?? 4096
              const mMax = m.max_tokens ?? (m as any).MaxTokens ?? 1024
              const mPub = m.published ?? (m as any).Published ?? false
              const status = modelTestStatus[mId]

              return (
                <div
                  key={mId}
                  style={{
                    display: 'flex',
                    justifyContent: 'space-between',
                    alignItems: 'center',
                    padding: '0.75rem 1rem',
                    backgroundColor: 'var(--bg-primary)',
                    borderRadius: 'var(--radius)',
                    border: '1px solid var(--border-color)',
                  }}
                >
                  <div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem' }}>
                      <span style={{ fontWeight: 600, fontSize: '1rem' }}>{mName || 'Unnamed Model'}</span>
                      {status?.testing && (
                        <span className="badge" style={{ backgroundColor: 'var(--bg-secondary)', color: 'var(--text-secondary)', fontSize: '0.75rem' }}>
                          Testing...
                        </span>
                      )}
                      {status?.ok && (
                        <span className="badge" style={{ backgroundColor: 'var(--accent-success)', fontSize: '0.75rem' }}>
                          ✓ Online ({status.latency}ms)
                        </span>
                      )}
                      {status?.error && (
                        <span className="badge" style={{ backgroundColor: 'var(--accent-danger)', fontSize: '0.75rem' }} title={status.error}>
                          ✗ Error
                        </span>
                      )}
                    </div>
                    <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', marginTop: '0.25rem' }}>
                      {mUrl} &bull; Context: {mCtx} &bull; Max Tokens: {mMax}
                    </div>
                    {status?.error && (
                      <div style={{ fontSize: '0.75rem', color: 'var(--accent-danger)', marginTop: '0.2rem' }}>
                        {status.error}
                      </div>
                    )}
                  </div>
                  <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                    <span
                      className={`badge ${mPub ? '' : 'badge-disabled'}`}
                      style={{ backgroundColor: mPub ? 'var(--accent-success)' : 'var(--border-color)' }}
                    >
                      {mPub ? 'Published' : 'Unpublished'}
                    </span>
                    <button
                      type="button"
                      className="btn btn-secondary"
                      style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }}
                      onClick={() => handleTestExistingModel(mId)}
                      disabled={status?.testing}
                    >
                      Test
                    </button>
                    <button
                      type="button"
                      className="btn btn-secondary"
                      style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem', color: 'var(--accent-danger)' }}
                      onClick={() => handleDeleteModel(mId)}
                    >
                      Delete
                    </button>
                  </div>
                </div>
              )
            })}
          </div>
        )}

        <div style={{ borderTop: '1px solid var(--border-color)', paddingTop: '1.5rem' }}>
          <h3 style={{ fontSize: '1rem', marginBottom: '1rem' }}>Add External Inference Endpoint</h3>

          {modelError && <div className="alert alert-error">{modelError}</div>}
          {modelSuccess && <div className="alert alert-success">{modelSuccess}</div>}
          {testResult && (
            <div className={`alert ${testResult.ok ? 'alert-success' : 'alert-error'}`} style={{ marginBottom: '1rem' }}>
              {testResult.message}
            </div>
          )}

          <form onSubmit={handleAddModel}>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
              <div className="form-group">
                <label>Endpoint URL (OpenAI-compatible)</label>
                <div style={{ display: 'flex', gap: '0.5rem' }}>
                  <input
                    type="text"
                    className="form-control"
                    placeholder="http://127.0.0.1:11434/v1 or http://host.containers.internal:11434/v1"
                    value={endpointUrl}
                    onChange={(e) => setEndpointUrl(e.target.value)}
                    required
                  />
                  <button
                    type="button"
                    className="btn btn-secondary"
                    style={{ whiteSpace: 'nowrap', padding: '0.4rem 0.85rem', fontSize: '0.85rem' }}
                    onClick={handleTestEndpoint}
                    disabled={testingEndpoint}
                  >
                    {testingEndpoint ? 'Testing...' : 'Test Connection'}
                  </button>
                  <button
                    type="button"
                    className="btn btn-secondary"
                    style={{ whiteSpace: 'nowrap', padding: '0.4rem 0.85rem', fontSize: '0.85rem' }}
                    onClick={handleDiscoverModels}
                    disabled={discoveringModels}
                  >
                    {discoveringModels ? 'Listing...' : 'List Models'}
                  </button>
                </div>
                <p style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginTop: '0.25rem' }}>
                  Tip: If Woolwire is in a container and your LLM server (e.g. Ollama) is on the host, use <code>http://host.containers.internal:11434/v1</code> or your machine LAN IP.
                </p>
              </div>

              {discoveredModels.length > 0 && (
                <div style={{ padding: '0.75rem 1rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--accent-primary)' }}>
                  <div style={{ fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.5rem' }}>
                    Discovered Models from Endpoint (click to select):
                  </div>
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: '0.4rem' }}>
                    {discoveredModels.map((mName) => (
                      <button
                        key={mName}
                        type="button"
                        className="badge"
                        style={{
                          cursor: 'pointer',
                          border: '1px solid var(--accent-primary)',
                          backgroundColor: name === mName ? 'var(--accent-primary)' : 'transparent',
                          color: name === mName ? '#fff' : 'inherit',
                          fontSize: '0.85rem',
                          padding: '0.3rem 0.6rem',
                        }}
                        onClick={() => {
                          setName(mName)
                          setBackendModel(mName)
                        }}
                      >
                        {mName}
                      </button>
                    ))}
                  </div>
                </div>
              )}

              <div className="settings-fields" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem' }}>
                <div className="form-group">
                  <label>Model Display Name</label>
                  <input
                    type="text"
                    className="form-control"
                    list="discovered-models-list"
                    placeholder="e.g. llama3:latest"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    required
                  />
                  <datalist id="discovered-models-list">
                    {discoveredModels.map((mName) => (
                      <option key={mName} value={mName} />
                    ))}
                  </datalist>
                </div>

                <div className="form-group">
                  <label>API Key (Optional)</label>
                  <input
                    type="password"
                    className="form-control"
                    placeholder="sk-... (leave blank for local Ollama)"
                    value={apiKey}
                    onChange={(e) => setApiKey(e.target.value)}
                  />
                </div>
              </div>

              <div className="settings-fields" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem' }}>
                <div className="form-group">
                  <label>Context Limit</label>
                  <input
                    type="number"
                    className="form-control"
                    value={contextLimit}
                    onChange={(e) => setContextLimit(Number(e.target.value))}
                  />
                </div>
                <div className="form-group">
                  <label>Max Tokens</label>
                  <input
                    type="number"
                    className="form-control"
                    value={maxTokens}
                    onChange={(e) => setMaxTokens(Number(e.target.value))}
                  />
                </div>
              </div>

              <div className="form-group">
                <label>Backend Model Identifier (Optional)</label>
                <input
                  type="text"
                  className="form-control"
                  placeholder="defaults to the display name, e.g. llama3:latest"
                  value={backendModel}
                  onChange={(e) => setBackendModel(e.target.value)}
                />
                <small style={{ color: 'var(--text-secondary)', fontSize: '0.8rem' }}>
                  What the backend server expects in the OpenAI <code>model</code> field.
                  Set this if you want a friendlier display name than the identifier
                  the server knows.
                </small>
              </div>

              <div className="form-group" style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <input
                  type="checkbox"
                  id="pubCheck"
                  checked={published}
                  onChange={(e) => setPublished(e.target.checked)}
                />
                <label htmlFor="pubCheck" style={{ marginBottom: 0, cursor: 'pointer' }}>
                  Publish model advertisement to room members
                </label>
              </div>

              <div className="form-group" style={{ display: 'flex', alignItems: 'flex-start', gap: '0.5rem' }}>
                <input
                  type="checkbox"
                  id="privateNetCheck"
                  checked={allowPrivateNetwork}
                  onChange={(e) => setAllowPrivateNetwork(e.target.checked)}
                  style={{ marginTop: '0.25rem' }}
                />
                <label htmlFor="privateNetCheck" style={{ marginBottom: 0, cursor: 'pointer' }}>
                  Allow this endpoint to reach your private network
                  <small style={{ display: 'block', color: 'var(--text-secondary)', fontSize: '0.8rem', fontWeight: 400 }}>
                    Needed for a model server on another machine on your LAN, or on a
                    non-standard port. Off by default: without it Woolwire refuses
                    private, link-local, and cloud metadata addresses no matter what
                    the hostname resolves to.
                  </small>
                </label>
              </div>

              <button type="submit" className="btn btn-primary" style={{ alignSelf: 'flex-start', marginTop: '0.5rem' }}>
                Add Hosted Model
              </button>
            </div>
          </form>
        </div>
      </div>

      {/* Host Limits Section */}
      </section>
      )}
      {activeTab === 'hosting' && (
      <section id="settings-panel-hosting" role="tabpanel" aria-labelledby="settings-tab-hosting" tabIndex={0}>
      <div className="card">
        <h2>Host Resource Limits</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          Configure fair queuing and execution caps when serving inference to peers.
        </p>

        {limitSuccess && <div className="alert alert-success">{limitSuccess}</div>}

        <form onSubmit={handleSaveLimits}>
          <div className="settings-fields" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '1rem' }}>
            <div className="form-group">
              <label>Max Active Concurrent</label>
              <input
                type="number"
                className="form-control"
                value={limits?.MaxActive ?? 1}
                onChange={(e) => setLimits({ ...limits, MaxActive: Number(e.target.value) })}
                min={1}
                max={4}
              />
            </div>

            <div className="form-group">
              <label>Max Queued / Member</label>
              <input
                type="number"
                className="form-control"
                value={limits?.MaxQueuedPerMember ?? 1}
                onChange={(e) => setLimits({ ...limits, MaxQueuedPerMember: Number(e.target.value) })}
                min={1}
                max={5}
              />
            </div>

            <div className="form-group">
              <label>Max Queued Total</label>
              <input
                type="number"
                className="form-control"
                value={limits?.MaxQueuedTotal ?? 10}
                onChange={(e) => setLimits({ ...limits, MaxQueuedTotal: Number(e.target.value) })}
                min={1}
                max={50}
              />
            </div>

            <div className="form-group">
              <label>Queue Timeout (sec)</label>
              <input
                type="number"
                className="form-control"
                value={limits?.QueueTimeoutSeconds ?? 300}
                onChange={(e) => setLimits({ ...limits, QueueTimeoutSeconds: Number(e.target.value) })}
                min={30}
              />
            </div>

            <div className="form-group">
              <label>Execution Timeout (sec)</label>
              <input
                type="number"
                className="form-control"
                value={limits?.ExecutionTimeoutSeconds ?? 600}
                onChange={(e) => setLimits({ ...limits, ExecutionTimeoutSeconds: Number(e.target.value) })}
                min={30}
              />
            </div>
          </div>

          <button type="submit" className="btn btn-primary" style={{ marginTop: '0.5rem' }}>
            Save Limits
          </button>
        </form>
      </div>

      {/* Hosting Leaderboard & Contributions Section */}
      <div className="card">
        <h2>Hosting Leaderboard & Contributions</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1rem' }}>
          Woolwire acknowledges members who share compute through jointly-signed completion receipts.
        </p>

        {contribMsg && (
          <div className="alert alert-success" style={{ marginBottom: '1rem' }}>
            {contribMsg}
          </div>
        )}

        <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.75rem', cursor: 'pointer', fontSize: '0.95rem' }}>
          <input
            type="checkbox"
            checked={optOut}
            onChange={(e) => handleToggleOptOut(e.target.checked)}
            style={{ width: '1.2rem', height: '1.2rem', marginTop: '0.15rem' }}
          />
          <div>
            <strong>Opt out of public contribution receipts</strong>
            <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', margin: '0.25rem 0 0 0' }}>
              When enabled, your node will not sign or issue public receipts for hosted requests, and you will not appear on the room leaderboard.
            </p>
          </div>
        </label>
      </div>
      </section>
      )}

      </div>
    </div>
  )
}
