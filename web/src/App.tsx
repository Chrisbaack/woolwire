import React, { useState, useEffect, useRef } from 'react'
import QRCode from 'qrcode'
import jsQR from 'jsqr'
import { Welcome } from './components/Welcome.tsx'
import { Dashboard } from './components/Dashboard.tsx'
import { MyChats } from './components/MyChats.tsx'
import { Community } from './components/Community.tsx'
import { Settings } from './components/Settings.tsx'
import { RoomAdmin } from './components/RoomAdmin.tsx'

interface AppState {
  configured: boolean
  display_name: string
  device_public: string
  member_id?: string
  room: {
    room_id: string
    room_name: string
    role: string
    invitation_code: string
    approval_mode: boolean
    roster_version: number
  } | null
}

type Tab = 'dashboard' | 'chats' | 'community' | 'settings' | 'admin'

interface ErrorBoundaryProps {
  children: React.ReactNode
}

interface ErrorBoundaryState {
  hasError: boolean
  error: Error | null
}

class ErrorBoundary extends React.Component<ErrorBoundaryProps, ErrorBoundaryState> {
  constructor(props: ErrorBoundaryProps) {
    super(props)
    this.state = { hasError: false, error: null }
  }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { hasError: true, error }
  }

  componentDidCatch(error: Error, errorInfo: React.ErrorInfo) {
    console.error("Uncaught error:", error, errorInfo)
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="card" style={{ padding: '1.5rem', border: '1px solid var(--accent-danger)' }}>
          <h2 style={{ color: 'var(--accent-danger)', marginBottom: '0.75rem' }}>Something went wrong</h2>
          <p style={{ color: 'var(--text-secondary)', marginBottom: '1.25rem' }}>
            {this.state.error?.message || 'An unexpected error occurred while rendering this view.'}
          </p>
          <button className="btn btn-primary" onClick={() => this.setState({ hasError: false, error: null })}>
            Try Again
          </button>
        </div>
      )
    }
    return this.props.children
  }
}

export const App: React.FC = () => {
  const [state, setState] = useState<AppState | null>(null)
  const [authRequired, setAuthRequired] = useState(false)
  const [setupToken, setSetupToken] = useState('')
  const [authError, setAuthError] = useState('')
  const [loading, setLoading] = useState(true)
  const [activeTab, setActiveTab] = useState<Tab>('dashboard')
  const [selectedChatModel, setSelectedChatModel] = useState<any>(null)

  // Connect Device Modal (when authenticated)
  const [showConnectModal, setShowConnectModal] = useState(false)
  const [modalToken, setModalToken] = useState('')
  const [modalQrUrl, setModalQrUrl] = useState('')
  const [modalCopiedLink, setModalCopiedLink] = useState(false)
  const [modalCopiedToken, setModalCopiedToken] = useState(false)

  // Scanner state for the Setup login card (when unauthenticated)
  const [loginScanning, setLoginScanning] = useState(false)
  const [loginScanError, setLoginScanError] = useState('')
  const loginVideoRef = useRef<HTMLVideoElement | null>(null)
  const loginCanvasRef = useRef<HTMLCanvasElement | null>(null)
  const loginAnimFrameRef = useRef<number | null>(null)
  const loginStreamRef = useRef<MediaStream | null>(null)
  const loginCameraInputRef = useRef<HTMLInputElement | null>(null)

  const fetchState = async () => {
    try {
      const res = await fetch('/api/v1/state')
      if (res.status === 401) {
        setAuthRequired(true)
        setLoading(false)
        return
      }
      if (res.ok) {
        const data = await res.json()
        setState(data)
        setAuthRequired(false)
      }
    } catch {
      // network error
    } finally {
      setLoading(false)
    }
  }

  const loginWithToken = async (token: string) => {
    try {
      setAuthError('')
      setLoading(true)
      const res = await fetch('/api/v1/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: token.trim() }),
      })
      if (!res.ok) {
        throw new Error('Invalid setup token or expired link')
      }
      setAuthRequired(false)
      await fetchState()
    } catch (err: any) {
      setAuthError(err.message || 'Setup authentication failed')
      setAuthRequired(true)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    const hash = window.location.hash
    const match = hash.match(/token=([a-zA-Z0-9_-]+)/)
    if (match && match[1]) {
      const token = match[1]
      window.history.replaceState(null, '', window.location.pathname + window.location.search)
      loginWithToken(token)
    } else {
      fetchState()
    }
  }, [])

  const handleSetup = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!setupToken.trim()) return
    await loginWithToken(setupToken.trim())
  }

  const stopLoginScanner = () => {
    if (loginAnimFrameRef.current) {
      cancelAnimationFrame(loginAnimFrameRef.current)
      loginAnimFrameRef.current = null
    }
    if (loginStreamRef.current) {
      loginStreamRef.current.getTracks().forEach((track) => track.stop())
      loginStreamRef.current = null
    }
    setLoginScanning(false)
  }

  const processDecodedToken = (rawText: string) => {
    let token = rawText.trim()
    const match = token.match(/token=([a-zA-Z0-9_-]+)/)
    if (match && match[1]) {
      token = match[1]
    }
    stopLoginScanner()
    setSetupToken(token)
    loginWithToken(token)
  }

  const tickLoginScan = () => {
    if (loginVideoRef.current && loginVideoRef.current.readyState === loginVideoRef.current.HAVE_ENOUGH_DATA) {
      const canvas = loginCanvasRef.current || document.createElement('canvas')
      canvas.width = loginVideoRef.current.videoWidth
      canvas.height = loginVideoRef.current.videoHeight
      const ctx = canvas.getContext('2d')
      if (ctx) {
        ctx.drawImage(loginVideoRef.current, 0, 0, canvas.width, canvas.height)
        const imageData = ctx.getImageData(0, 0, canvas.width, canvas.height)
        const code = jsQR(imageData.data, imageData.width, imageData.height, {
          inversionAttempts: 'attemptBoth',
        })
        if (code && code.data) {
          processDecodedToken(code.data)
          return
        }
      }
    }
    loginAnimFrameRef.current = requestAnimationFrame(tickLoginScan)
  }

  const startLoginScanner = async () => {
    setLoginScanError('')
    setLoginScanning(true)
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: 'environment' },
      })
      loginStreamRef.current = stream
      if (loginVideoRef.current) {
        loginVideoRef.current.srcObject = stream
        loginVideoRef.current.setAttribute('playsinline', 'true')
        await loginVideoRef.current.play()
        loginAnimFrameRef.current = requestAnimationFrame(tickLoginScan)
      }
    } catch (err: any) {
      setLoginScanError(err.message || 'Camera permission denied or camera not accessible')
    }
  }

  const handleLoginScanClick = () => {
    if (loginScanning) {
      stopLoginScanner()
      return
    }
    setLoginScanError('')
    if (typeof navigator !== 'undefined' && navigator.mediaDevices && typeof navigator.mediaDevices.getUserMedia === 'function') {
      startLoginScanner()
    } else {
      loginCameraInputRef.current?.click()
    }
  }

  const handleLoginImageCapture = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    setLoginScanError('')
    const reader = new FileReader()
    reader.onload = (event) => {
      const img = new Image()
      img.onload = () => {
        let width = img.width
        let height = img.height
        const maxDim = 1200
        if (width > maxDim || height > maxDim) {
          if (width > height) {
            height = Math.round((height * maxDim) / width)
            width = maxDim
          } else {
            width = Math.round((width * maxDim) / height)
            height = maxDim
          }
        }
        const canvas = document.createElement('canvas')
        canvas.width = width
        canvas.height = height
        const ctx = canvas.getContext('2d')
        if (!ctx) {
          setLoginScanError('Could not process captured image.')
          return
        }
        ctx.drawImage(img, 0, 0, width, height)
        const imageData = ctx.getImageData(0, 0, width, height)
        const code = jsQR(imageData.data, imageData.width, imageData.height, {
          inversionAttempts: 'attemptBoth',
        })
        if (code && code.data) {
          processDecodedToken(code.data)
        } else {
          setLoginScanError('Could not detect a QR code in the image. Please try taking a closer photo or enter the secret.')
        }
      }
      img.onerror = () => {
        setLoginScanError('Failed to read image file')
      }
      img.src = event.target?.result as string
    }
    reader.readAsDataURL(file)
    e.target.value = ''
  }

  const openConnectModal = async () => {
    setShowConnectModal(true)
    try {
      const res = await fetch('/api/v1/setup/info')
      if (res.ok) {
        const data = await res.json()
        if (data.token) {
          setModalToken(data.token)
          const loginUrl = `${window.location.origin}/#token=${data.token}`
          QRCode.toDataURL(loginUrl, {
            width: 240,
            margin: 2,
            color: { dark: '#000000', light: '#ffffff' },
          })
            .then(setModalQrUrl)
            .catch(() => setModalQrUrl(''))
        }
      }
    } catch {
      // ignore
    }
  }

  const handleUpdateProfile = async (displayName: string) => {
    const res = await fetch('/api/v1/profile', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ display_name: displayName }),
    })
    if (!res.ok) {
      const msg = await res.text()
      throw new Error(msg || 'Failed to update profile')
    }
    await fetchState()
  }

  const handleHostRoom = async (roomName: string): Promise<string> => {
    const res = await fetch('/api/v1/room/host', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ room_name: roomName }),
    })
    if (!res.ok) {
      const msg = await res.text()
      throw new Error(msg || 'Failed to host room')
    }
    const data = await res.json()
    await fetchState()
    setActiveTab('dashboard')
    return data.invitation_code
  }

  const handleJoinRoom = async (code: string) => {
    const res = await fetch('/api/v1/room/join', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ invitation_code: code }),
    })
    if (!res.ok) {
      const msg = await res.text()
      throw new Error(msg || 'Failed to join room')
    }
    await fetchState()
    setActiveTab('dashboard')
  }

  const handleLeaveRoom = async () => {
    const res = await fetch('/api/v1/room/leave', { method: 'POST' })
    if (res.ok) {
      await fetchState()
      setActiveTab('dashboard')
    }
  }

  const handleRotateInvitation = async (): Promise<string> => {
    const res = await fetch('/api/v1/room-admin/invitation/rotate', { method: 'POST' })
    if (!res.ok) {
      throw new Error('Failed to rotate invitation')
    }
    const data = await res.json()
    await fetchState()
    return data.invitation_code
  }

  const handleRemoveMember = async (id: string, rotate: boolean) => {
    const res = await fetch(`/api/v1/room-admin/members/${id}/remove`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ rotate_invitation: rotate }),
    })
    if (!res.ok) {
      throw new Error('Failed to remove member')
    }
    await fetchState()
  }

  const handleSelectModelForChat = (model: any) => {
    setSelectedChatModel(model)
    setActiveTab('chats')
  }

  if (loading) {
    return <div className="app-container"><p>Loading Woolwire...</p></div>
  }

  if (authRequired) {
    return (
      <div className="app-container">
        <div className="card" style={{ maxWidth: '480px', margin: '4rem auto' }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem', flexWrap: 'wrap', gap: '0.5rem' }}>
            <h2 style={{ margin: 0 }}>Owner Setup Authentication</h2>
            <button
              type="button"
              className="btn btn-secondary"
              style={{ padding: '0.25rem 0.6rem', fontSize: '0.8rem' }}
              onClick={handleLoginScanClick}
            >
              {loginScanning ? 'Stop Camera' : '📷 Scan Login QR'}
            </button>
            <input
              ref={loginCameraInputRef}
              type="file"
              accept="image/*"
              capture="environment"
              style={{ display: 'none' }}
              onChange={handleLoginImageCapture}
            />
          </div>

          <p style={{ color: 'var(--text-secondary)', marginBottom: '1rem', fontSize: '0.9rem' }}>
            Enter your setup secret / PIN, or scan the Login QR code from another device already authenticated.
          </p>

          {loginScanError && !loginScanning && (
            <div className="alert alert-error" style={{ marginBottom: '0.75rem', fontSize: '0.85rem' }}>
              {loginScanError}
            </div>
          )}

          {loginScanning && (
            <div style={{ marginBottom: '1rem', padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--accent-primary)', textAlign: 'center' }}>
              <div style={{ position: 'relative', width: '100%', maxWidth: '280px', margin: '0 auto', overflow: 'hidden', borderRadius: '8px', background: '#000' }}>
                <video
                  ref={loginVideoRef}
                  style={{ width: '100%', height: 'auto', display: 'block' }}
                  playsInline
                  muted
                />
                <div style={{ position: 'absolute', top: '15%', left: '15%', right: '15%', bottom: '15%', border: '2px dashed var(--accent-primary)', pointerEvents: 'none', borderRadius: '8px' }} />
              </div>
              <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', marginTop: '0.5rem' }}>
                Point camera at the Login QR code on your computer
              </p>
              <div style={{ display: 'flex', gap: '0.5rem', justifyContent: 'center', marginTop: '0.5rem', flexWrap: 'wrap' }}>
                <button
                  type="button"
                  className="btn btn-secondary"
                  style={{ padding: '0.2rem 0.6rem', fontSize: '0.8rem' }}
                  onClick={() => loginCameraInputRef.current?.click()}
                >
                  📸 Take Photo / Pick Image
                </button>
                <button
                  type="button"
                  className="btn btn-secondary"
                  style={{ padding: '0.2rem 0.6rem', fontSize: '0.8rem' }}
                  onClick={stopLoginScanner}
                >
                  Close
                </button>
              </div>
              {loginScanError && <div className="alert alert-error" style={{ marginTop: '0.5rem', fontSize: '0.85rem' }}>{loginScanError}</div>}
              <canvas ref={loginCanvasRef} style={{ display: 'none' }} />
            </div>
          )}

          {authError && <div className="alert alert-error">{authError}</div>}
          <form onSubmit={handleSetup}>
            <div className="form-group">
              <label htmlFor="token">Setup Secret / PIN</label>
              <input
                id="token"
                type="password"
                className="form-control"
                placeholder="Enter setup token or custom PIN"
                value={setupToken}
                onChange={(e) => setSetupToken(e.target.value)}
                required
              />
            </div>
            <button type="submit" className="btn btn-primary" style={{ width: '100%' }}>
              Unlock Installation
            </button>
          </form>

          <div style={{ marginTop: '1.25rem', padding: '0.75rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', fontSize: '0.8rem', color: 'var(--text-secondary)' }}>
            💡 <strong>Tip for multi-device access:</strong> If you are already logged in on your computer, click <strong>📱 Connect Device</strong> in the top header or visit <strong>Settings</strong> to view a Quick Login QR code or customize your PIN.
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="app-container">
      <header className="header">
        <h1>
          <span>Woolwire</span>
          <span className="badge">v0.2</span>
        </h1>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          {state?.display_name && (
            <div style={{ fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
              Signed in as <strong>{state.display_name}</strong>
            </div>
          )}
          <button
            type="button"
            className="btn btn-secondary"
            style={{ padding: '0.3rem 0.65rem', fontSize: '0.8rem', display: 'flex', alignItems: 'center', gap: '0.35rem' }}
            onClick={openConnectModal}
            title="Connect your phone or another device to this Woolwire node"
          >
            <span>📱</span> Connect Device
          </button>
        </div>
      </header>

      <main>
        {!state?.room ? (
          <Welcome
            displayName={state?.display_name || ''}
            onUpdateProfile={handleUpdateProfile}
            onHostRoom={handleHostRoom}
            onJoinRoom={handleJoinRoom}
          />
        ) : (
          <div>
            {/* Navigation Tabs */}
            <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '1.5rem', borderBottom: '1px solid var(--border-color)', paddingBottom: '0.5rem' }}>
              <button
                className={`btn ${activeTab === 'dashboard' ? 'btn-primary' : 'btn-secondary'}`}
                style={{ padding: '0.5rem 1rem', fontSize: '0.9rem' }}
                onClick={() => setActiveTab('dashboard')}
              >
                Dashboard
              </button>

              <button
                className={`btn ${activeTab === 'chats' ? 'btn-primary' : 'btn-secondary'}`}
                style={{ padding: '0.5rem 1rem', fontSize: '0.9rem' }}
                onClick={() => setActiveTab('chats')}
              >
                My Chats
              </button>

              <button
                className={`btn ${activeTab === 'community' ? 'btn-primary' : 'btn-secondary'}`}
                style={{ padding: '0.5rem 1rem', fontSize: '0.9rem' }}
                onClick={() => setActiveTab('community')}
              >
                Community
              </button>

              <button
                className={`btn ${activeTab === 'settings' ? 'btn-primary' : 'btn-secondary'}`}
                style={{ padding: '0.5rem 1rem', fontSize: '0.9rem' }}
                onClick={() => setActiveTab('settings')}
              >
                Settings
              </button>

              {state.room.role === 'creator' && (
                <button
                  className={`btn ${activeTab === 'admin' ? 'btn-primary' : 'btn-secondary'}`}
                  style={{ padding: '0.5rem 1rem', fontSize: '0.9rem' }}
                  onClick={() => setActiveTab('admin')}
                >
                  Room Admin
                </button>
              )}
            </div>

            {/* Tab Views */}
            <ErrorBoundary key={activeTab}>
              {activeTab === 'dashboard' && (
                <Dashboard
                  roomName={state.room.room_name}
                  role={state.room.role}
                  displayName={state.display_name}
                  onLeaveRoom={handleLeaveRoom}
                  onSelectModelForChat={handleSelectModelForChat}
                />
              )}

              {activeTab === 'chats' && (
                <MyChats initialModel={selectedChatModel} />
              )}

              {activeTab === 'community' && (
                <Community
                  currentMemberID={state.member_id}
                  role={state.room.role}
                />
              )}

              {activeTab === 'settings' && (
                <Settings />
              )}

              {activeTab === 'admin' && state.room.role === 'creator' && (
                <RoomAdmin
                  invitationCode={state.room.invitation_code}
                  onRotateInvitation={handleRotateInvitation}
                  onRemoveMember={handleRemoveMember}
                />
              )}
            </ErrorBoundary>
          </div>
        )}
      </main>

      {showConnectModal && (
        <div style={{
          position: 'fixed',
          top: 0,
          left: 0,
          right: 0,
          bottom: 0,
          backgroundColor: 'rgba(0, 0, 0, 0.7)',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          zIndex: 1000,
          padding: '1rem',
        }}>
          <div className="card" style={{ maxWidth: '420px', width: '100%', margin: 0, boxShadow: '0 8px 30px rgba(0,0,0,0.3)' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
              <h3 style={{ margin: 0 }}>📱 Connect Another Device</h3>
              <button
                type="button"
                className="btn btn-secondary"
                style={{ padding: '0.2rem 0.5rem', fontSize: '0.8rem' }}
                onClick={() => setShowConnectModal(false)}
              >
                ✕
              </button>
            </div>

            <p style={{ color: 'var(--text-secondary)', fontSize: '0.85rem', marginBottom: '1rem' }}>
              Scan this QR code with your phone or tablet camera to instantly log in without typing.
            </p>

            {modalQrUrl ? (
              <div style={{ padding: '1rem', background: '#ffffff', borderRadius: '8px', textAlign: 'center', marginBottom: '1rem' }}>
                <img src={modalQrUrl} alt="Login QR Code" style={{ display: 'block', maxWidth: '200px', margin: '0 auto' }} />
                <div style={{ color: '#333333', fontSize: '0.75rem', marginTop: '0.5rem', fontWeight: 500 }}>
                  Open phone camera and point at this code
                </div>
              </div>
            ) : (
              <div style={{ width: '200px', height: '200px', margin: '0 auto 1rem auto', display: 'flex', alignItems: 'center', justifyContent: 'center', background: 'var(--bg-primary)', borderRadius: '8px' }}>
                <span style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>Loading QR...</span>
              </div>
            )}

            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', marginBottom: '1rem' }}>
              <button
                type="button"
                className="btn btn-primary"
                style={{ fontSize: '0.85rem' }}
                onClick={() => {
                  const url = `${window.location.origin}/#token=${modalToken}`
                  navigator.clipboard.writeText(url)
                  setModalCopiedLink(true)
                  setTimeout(() => setModalCopiedLink(false), 2000)
                }}
              >
                {modalCopiedLink ? 'Copied Link!' : '📋 Copy Quick Login Link'}
              </button>
              <button
                type="button"
                className="btn btn-secondary"
                style={{ fontSize: '0.85rem' }}
                onClick={() => {
                  navigator.clipboard.writeText(modalToken)
                  setModalCopiedToken(true)
                  setTimeout(() => setModalCopiedToken(false), 2000)
                }}
              >
                {modalCopiedToken ? 'Copied Token!' : '🔑 Copy Setup Secret'}
              </button>
            </div>

            <div style={{ borderTop: '1px solid var(--border-color)', paddingTop: '0.75rem', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>Want a memorable PIN?</span>
              <button
                type="button"
                className="btn btn-secondary"
                style={{ padding: '0.2rem 0.5rem', fontSize: '0.8rem' }}
                onClick={() => {
                  setShowConnectModal(false)
                  setActiveTab('settings')
                }}
              >
                Configure in Settings
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

export default App
