import React, { useState, useEffect } from 'react'
import QRCode from 'qrcode'

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

interface RunnerHealth {
  configured: boolean
  status: string
  loaded_model_id?: string
  loaded_file?: string
  engine_pid?: number
}

interface ArtifactManifest {
  id: string
  name: string
  filename: string
  size_bytes: number
  sha256: string
  source_url?: string
  context_limit: number
  installed_at: number
}

export const Settings: React.FC = () => {
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
  const [artifacts, setArtifacts] = useState<ArtifactManifest[]>([])

  const [name, setName] = useState('')
  const [endpointUrl, setEndpointUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [contextLimit, setContextLimit] = useState(4096)
  const [maxTokens, setMaxTokens] = useState(1024)
  const [published, setPublished] = useState(true)

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

  // Device Quick Login & Setup Token
  const [setupToken, setSetupToken] = useState('')
  const [setupQrUrl, setSetupQrUrl] = useState('')
  const [copiedLoginLink, setCopiedLoginLink] = useState(false)
  const [copiedSetupToken, setCopiedSetupToken] = useState(false)
  const [showSetupSecret, setShowSetupSecret] = useState(false)
  const [newSetupPin, setNewSetupPin] = useState('')
  const [pinError, setPinError] = useState('')
  const [pinSuccess, setPinSuccess] = useState('')
  const [updatingPin, setUpdatingPin] = useState(false)

  const fetchSetupInfo = async () => {
    try {
      const res = await fetch('/api/v1/setup/info')
      if (res.ok) {
        const data = await res.json()
        if (data.token) {
          setSetupToken(data.token)
          const loginUrl = `${window.location.origin}/#token=${data.token}`
          QRCode.toDataURL(loginUrl, {
            width: 240,
            margin: 2,
            color: { dark: '#000000', light: '#ffffff' },
          })
            .then(setSetupQrUrl)
            .catch(() => setSetupQrUrl(''))
        }
      }
    } catch {
      // ignore
    }
  }

  const handleUpdatePin = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!newSetupPin.trim() || newSetupPin.trim().length < 4) {
      setPinError('Setup secret or PIN must be at least 4 characters')
      return
    }
    try {
      setUpdatingPin(true)
      setPinError('')
      setPinSuccess('')
      const res = await fetch('/api/v1/setup/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: newSetupPin.trim() }),
      })
      if (!res.ok) {
        const errText = await res.text()
        throw new Error(errText || 'Failed to update setup secret')
      }
      const data = await res.json()
      setSetupToken(data.token)
      setNewSetupPin('')
      setPinSuccess('Setup secret / PIN updated successfully!')
      const loginUrl = `${window.location.origin}/#token=${data.token}`
      QRCode.toDataURL(loginUrl, {
        width: 240,
        margin: 2,
        color: { dark: '#000000', light: '#ffffff' },
      })
        .then(setSetupQrUrl)
        .catch(() => setSetupQrUrl(''))
      setTimeout(() => setPinSuccess(''), 4000)
    } catch (err: any) {
      setPinError(err.message || 'Failed to update setup secret')
    } finally {
      setUpdatingPin(false)
    }
  }

  const copyLoginLink = () => {
    const loginUrl = `${window.location.origin}/#token=${setupToken}`
    navigator.clipboard.writeText(loginUrl)
    setCopiedLoginLink(true)
    setTimeout(() => setCopiedLoginLink(false), 2000)
  }

  const copySetupToken = () => {
    navigator.clipboard.writeText(setupToken)
    setCopiedSetupToken(true)
    setTimeout(() => setCopiedSetupToken(false), 2000)
  }

  // Contributions & Social Recognition settings
  const [optOut, setOptOut] = useState(false)
  const [contribMsg, setContribMsg] = useState('')

  const fetchContributionsSettings = async () => {
    try {
      const res = await fetch('/api/v1/contributions/settings')
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
      const res = await fetch('/api/v1/contributions/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
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
      const res = await fetch('/api/v1/hosted-models')
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
      const res = await fetch('/api/v1/host-limits')
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
      const hwRes = await fetch('/api/v1/hardware')
      if (hwRes.ok) setHardware(await hwRes.json())

      const rRes = await fetch('/api/v1/managed-models/runner-health')
      if (rRes.ok) setRunner(await rRes.json())

      const artRes = await fetch('/api/v1/managed-models/artifacts')
      if (artRes.ok) {
        const data = await artRes.json()
        setArtifacts(Array.isArray(data) ? data : [])
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
  }, [])

  const handleAddModel = async (e: React.FormEvent) => {
    e.preventDefault()
    setModelError('')
    setModelSuccess('')

    try {
      const res = await fetch('/api/v1/hosted-models', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: name.trim(),
          endpoint_url: endpointUrl.trim(),
          api_key: apiKey.trim(),
          context_limit: Number(contextLimit),
          max_tokens: Number(maxTokens),
          enabled: true,
          published: published,
        }),
      })

      if (!res.ok) {
        const err = await res.text()
        throw new Error(err || 'Failed to save hosted model')
      }

      setModelSuccess('Hosted model saved successfully!')
      setName('')
      setEndpointUrl('')
      setApiKey('')
      await fetchModels()
    } catch (err: any) {
      setModelError(err.message || 'Error saving model')
    }
  }

  const handleDeleteModel = async (id: string) => {
    if (!confirm('Remove this hosted model?')) return
    try {
      const res = await fetch(`/api/v1/hosted-models/${id}`, { method: 'DELETE' })
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
      const res = await fetch('/api/v1/hosted-models/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          endpoint_url: endpointUrl.trim(),
          api_key: apiKey.trim(),
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
      const res = await fetch('/api/v1/hosted-models/discover', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          endpoint_url: endpointUrl.trim(),
          api_key: apiKey.trim(),
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
      const res = await fetch('/api/v1/hosted-models/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
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
      const res = await fetch('/api/v1/host-limits', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(limits),
      })
      if (res.ok) {
        setLimitSuccess('Host limits updated successfully!')
      }
    } catch {
      // ignore
    }
  }

  const handleDownloadArtifact = async (e: React.FormEvent) => {
    e.preventDefault()
    setDlLoading(true)
    setDlError('')
    setDlSuccess('')
    try {
      const res = await fetch('/api/v1/managed-models/download', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          source_url: dlUrl.trim(),
          filename: dlFilename.trim(),
          expected_sha256: dlSha.trim(),
        }),
      })
      if (!res.ok) {
        const msg = await res.text()
        throw new Error(msg || 'Download failed')
      }
      setDlSuccess('GGUF model artifact downloaded and verified successfully!')
      setDlUrl('')
      setDlFilename('')
      setDlSha('')
      await fetchHardwareAndRunner()
    } catch (err: any) {
      setDlError(err.message || 'Download error')
    } finally {
      setDlLoading(false)
    }
  }

  const handleLoadArtifactIntoRunner = async (filename: string) => {
    try {
      const res = await fetch('/api/v1/managed-models/load', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          model_id: filename.replace('.gguf', ''),
          filename: filename,
          context_limit: 4096,
          threads: 4,
          gpu_layers: hardware?.has_nvidia_gpu ? 33 : 0,
        }),
      })
      if (res.ok) {
        await fetchHardwareAndRunner()
      } else {
        alert(await res.text())
      }
    } catch (err: any) {
      alert(err.message)
    }
  }

  const handleUnloadRunner = async () => {
    try {
      const res = await fetch('/api/v1/managed-models/unload', { method: 'POST' })
      if (res.ok) {
        await fetchHardwareAndRunner()
      }
    } catch {
      // ignore
    }
  }

  const handleDeleteArtifact = async (filename: string) => {
    if (!confirm(`Delete ${filename}?`)) return
    try {
      const res = await fetch(`/api/v1/managed-models/artifacts/${encodeURIComponent(filename)}`, {
        method: 'DELETE',
      })
      if (res.ok) {
        await fetchHardwareAndRunner()
      }
    } catch {
      // ignore
    }
  }

  return (
    <div>
      {/* Device Access & Multi-Device Login Section */}
      <div className="card">
        <h2>📱 Connect Another Device / Quick Login</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          Scan the QR code or use the quick login link to instantly authenticate your phone, tablet, or another browser on this local network without typing long tokens.
        </p>

        <div style={{ display: 'flex', gap: '1.5rem', flexWrap: 'wrap', alignItems: 'flex-start' }}>
          {setupQrUrl ? (
            <div style={{ padding: '0.75rem', background: '#ffffff', borderRadius: '8px', textAlign: 'center', boxShadow: '0 2px 8px rgba(0,0,0,0.1)' }}>
              <img src={setupQrUrl} alt="Quick Login QR Code" style={{ display: 'block', width: '200px', height: '200px', margin: '0 auto' }} />
              <div style={{ color: '#333333', fontSize: '0.75rem', marginTop: '0.5rem', fontWeight: 500 }}>
                Point phone camera here to log in
              </div>
            </div>
          ) : (
            <div style={{ width: '200px', height: '200px', display: 'flex', alignItems: 'center', justifyContent: 'center', background: 'var(--bg-primary)', borderRadius: '8px' }}>
              <span style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>Loading QR...</span>
            </div>
          )}

          <div style={{ flex: '1', minWidth: '260px' }}>
            <div className="form-group" style={{ marginBottom: '1rem' }}>
              <label style={{ fontSize: '0.85rem', fontWeight: 600 }}>Quick Login Link</label>
              <div style={{ display: 'flex', gap: '0.5rem' }}>
                <input
                  type="text"
                  readOnly
                  className="form-control"
                  style={{ fontSize: '0.85rem', background: 'var(--bg-primary)' }}
                  value={`${window.location.origin}/#token=${setupToken}`}
                />
                <button
                  type="button"
                  className="btn btn-secondary"
                  style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
                  onClick={copyLoginLink}
                >
                  {copiedLoginLink ? 'Copied!' : 'Copy Link'}
                </button>
              </div>
            </div>

            <div className="form-group" style={{ marginBottom: '1.25rem' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <label style={{ fontSize: '0.85rem', fontWeight: 600, marginBottom: 0 }}>Active Setup Token / PIN</label>
                <button
                  type="button"
                  style={{ background: 'none', border: 'none', color: 'var(--accent-primary)', cursor: 'pointer', fontSize: '0.75rem' }}
                  onClick={() => setShowSetupSecret(!showSetupSecret)}
                >
                  {showSetupSecret ? 'Hide' : 'Reveal'}
                </button>
              </div>
              <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.25rem' }}>
                <input
                  type={showSetupSecret ? 'text' : 'password'}
                  readOnly
                  className="form-control"
                  style={{ fontSize: '0.85rem', background: 'var(--bg-primary)', fontFamily: 'monospace' }}
                  value={setupToken}
                />
                <button
                  type="button"
                  className="btn btn-secondary"
                  style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
                  onClick={copySetupToken}
                >
                  {copiedSetupToken ? 'Copied!' : 'Copy Token'}
                </button>
              </div>
            </div>

            {/* Custom PIN update */}
            <form onSubmit={handleUpdatePin} style={{ borderTop: '1px solid var(--border-color)', paddingTop: '1rem' }}>
              <label style={{ fontSize: '0.85rem', fontWeight: 600 }}>Customize Setup Secret / PIN</label>
              <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', margin: '0.2rem 0 0.5rem 0' }}>
                Set a memorable PIN or password (e.g. <code>123456</code> or a passphrase) so you can easily unlock new devices manually without scanning.
              </p>
              {pinError && <div className="alert alert-error" style={{ fontSize: '0.85rem', padding: '0.5rem', marginBottom: '0.5rem' }}>{pinError}</div>}
              {pinSuccess && <div className="alert alert-success" style={{ fontSize: '0.85rem', padding: '0.5rem', marginBottom: '0.5rem' }}>{pinSuccess}</div>}
              <div style={{ display: 'flex', gap: '0.5rem' }}>
                <input
                  type="text"
                  className="form-control"
                  placeholder="e.g. 123456 or my-secret-pin"
                  value={newSetupPin}
                  onChange={(e) => setNewSetupPin(e.target.value)}
                  style={{ fontSize: '0.85rem' }}
                />
                <button
                  type="submit"
                  className="btn btn-primary"
                  style={{ whiteSpace: 'nowrap', fontSize: '0.85rem' }}
                  disabled={updatingPin || !newSetupPin.trim()}
                >
                  {updatingPin ? 'Saving...' : 'Set New PIN'}
                </button>
              </div>
            </form>
          </div>
        </div>
      </div>

      {/* Hardware & Managed Deployment Section */}
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
              <div style={{ fontSize: '1rem', fontWeight: 600, color: hardware.has_nvidia_gpu ? 'var(--accent-success)' : 'inherit' }}>
                {hardware.has_nvidia_gpu ? hardware.gpu_name || 'NVIDIA GPU' : 'CPU Only'}
              </div>
            </div>

            <div style={{ padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)' }}>
              <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>Managed Runner</div>
              <div style={{ fontSize: '1rem', fontWeight: 600 }}>
                {runner?.configured ? (
                  <span style={{ color: runner.status === 'ready' ? 'var(--accent-success)' : 'var(--accent-primary)' }}>
                    {runner.status.toUpperCase()} {runner.loaded_file ? `(${runner.loaded_file})` : ''}
                  </span>
                ) : (
                  <span style={{ color: 'var(--text-secondary)' }}>Not Configured</span>
                )}
              </div>
            </div>
          </div>
        )}

        {/* Managed GGUF Artifacts (Only shown if managed runner is configured) */}
        {runner?.configured && (
          <div style={{ borderTop: '1px solid var(--border-color)', paddingTop: '1.25rem' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
              <h3 style={{ fontSize: '1rem' }}>Installed GGUF Model Artifacts ({(artifacts || []).length})</h3>
              {runner?.loaded_model_id && (
                <button className="btn btn-secondary" style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }} onClick={handleUnloadRunner}>
                  Unload Current Model
                </button>
              )}
            </div>

            {(artifacts || []).length === 0 ? (
              <p style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', marginBottom: '1.5rem' }}>
                No GGUF weight files downloaded yet. You can download GGUF models directly via HTTPS below.
              </p>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', marginBottom: '1.5rem' }}>
                {(artifacts || []).map((a) => (
                  <div
                    key={a.filename}
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
                      <div style={{ fontWeight: 600 }}>{a.filename}</div>
                      <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                        {(a.size_bytes / (1024 * 1024)).toFixed(1)} MB &bull; SHA-256: {a.sha256.slice(0, 16)}...
                      </div>
                    </div>

                    <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                      {runner?.loaded_file === a.filename ? (
                        <span className="badge" style={{ backgroundColor: 'var(--accent-success)' }}>Loaded Active</span>
                      ) : (
                        <button
                          className="btn btn-primary"
                          style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }}
                          onClick={() => handleLoadArtifactIntoRunner(a.filename)}
                          disabled={!runner?.configured}
                        >
                          Load
                        </button>
                      )}
                      <button
                        className="btn btn-secondary"
                        style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }}
                        onClick={() => handleDeleteArtifact(a.filename)}
                      >
                        Delete
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            )}

            {/* Download Artifact Form */}
            <h4 style={{ fontSize: '0.9rem', marginBottom: '0.75rem' }}>Download Model Weights (HTTPS)</h4>
            {dlError && <div className="alert alert-error">{dlError}</div>}
            {dlSuccess && <div className="alert alert-success">{dlSuccess}</div>}

            <form onSubmit={handleDownloadArtifact}>
              <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr 1fr', gap: '0.75rem' }}>
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
                {dlLoading ? 'Downloading Artifact...' : 'Download & Verify GGUF'}
              </button>
            </form>
          </div>
        )}
      </div>

      {/* Hosted Models Section */}
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
                        onClick={() => setName(mName)}
                      >
                        {mName}
                      </button>
                    ))}
                  </div>
                </div>
              )}

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem' }}>
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

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem' }}>
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

              <button type="submit" className="btn btn-primary" style={{ alignSelf: 'flex-start', marginTop: '0.5rem' }}>
                Add Hosted Model
              </button>
            </div>
          </form>
        </div>
      </div>

      {/* Host Limits Section */}
      <div className="card">
        <h2>Host Resource Limits</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1.25rem' }}>
          Configure fair queuing and execution caps when serving inference to peers.
        </p>

        {limitSuccess && <div className="alert alert-success">{limitSuccess}</div>}

        <form onSubmit={handleSaveLimits}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '1rem' }}>
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

      {/* Local Compatibility Section */}
      <div className="card">
        <h2>Local Compatibility Endpoint</h2>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', marginBottom: '1rem' }}>
          Connect third-party tools (like Open WebUI, Continue.dev, or Python OpenAI SDK) to your Woolwire instance.
        </p>

        <div style={{ backgroundColor: 'var(--bg-primary)', padding: '1rem', borderRadius: 'var(--radius)', border: '1px solid var(--border-color)', fontSize: '0.9rem' }}>
          <div><strong>Base URL:</strong> <code>http://127.0.0.1:7070/v1</code></div>
          <div style={{ marginTop: '0.5rem' }}><strong>Chat Completions:</strong> <code>POST /v1/chat/completions</code></div>
          <div style={{ marginTop: '0.5rem' }}><strong>Models List:</strong> <code>GET /v1/models</code></div>
        </div>
      </div>
    </div>
  )
}
