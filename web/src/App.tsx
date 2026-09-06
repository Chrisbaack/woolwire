import React, { useState, useEffect } from 'react'
import { Welcome } from './components/Welcome.tsx'
import { Dashboard } from './components/Dashboard.tsx'
import { MyChats } from './components/MyChats.tsx'
import { Community } from './components/Community.tsx'
import { Settings } from './components/Settings.tsx'
import { RoomAdmin } from './components/RoomAdmin.tsx'
import { api, apiJSON } from './api.ts'

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

  // Connect Device modal (when authenticated). It mints a fresh one-time
  // secret rather than revealing the one already in use.
  const [showConnectModal, setShowConnectModal] = useState(false)
  const [modalToken, setModalToken] = useState('')
  const [modalError, setModalError] = useState('')
  const [modalCopiedToken, setModalCopiedToken] = useState(false)

  const fetchState = async () => {
    try {
      const res = await api('/api/v1/state')
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
      const res = await api('/api/v1/setup', {
        method: 'POST',
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
    fetchState()
  }, [])

  const handleSetup = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!setupToken.trim()) return
    await loginWithToken(setupToken.trim())
  }

  // openConnectModal mints a fresh one-time setup secret for pairing another
  // device. The secret already in use is never revealed: it is single-use, so
  // showing it again would only create another copy to leak.
  const openConnectModal = async () => {
    setShowConnectModal(true)
    setModalToken('')
    setModalError('')
    try {
      const data = await apiJSON<{ token: string }>('/api/v1/setup/token', {
        method: 'POST',
        body: JSON.stringify({}),
      })
      setModalToken(data.token)
    } catch (err: any) {
      setModalError(err.message || 'Failed to mint a setup secret')
    }
  }

  const handleUpdateProfile = async (displayName: string) => {
    const res = await api('/api/v1/profile', {
      method: 'POST',
      body: JSON.stringify({ display_name: displayName }),
    })
    if (!res.ok) {
      const msg = await res.text()
      throw new Error(msg || 'Failed to update profile')
    }
    await fetchState()
  }

  const handleHostRoom = async (roomName: string): Promise<string> => {
    const res = await api('/api/v1/room/host', {
      method: 'POST',
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
    const res = await api('/api/v1/room/join', {
      method: 'POST',
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
    const res = await api('/api/v1/room/leave', { method: 'POST' })
    if (res.ok) {
      await fetchState()
      setActiveTab('dashboard')
    }
  }

  const handleRotateInvitation = async (): Promise<string> => {
    const res = await api('/api/v1/room-admin/invitation/rotate', { method: 'POST' })
    if (!res.ok) {
      throw new Error('Failed to rotate invitation')
    }
    const data = await res.json()
    await fetchState()
    return data.invitation_code
  }

  const handleRemoveMember = async (id: string, rotate: boolean) => {
    const res = await api(`/api/v1/room-admin/members/${id}/remove`, {
      method: 'POST',
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
          <h2 style={{ margin: '0 0 0.5rem 0' }}>Owner Setup Authentication</h2>

          <p style={{ color: 'var(--text-secondary)', marginBottom: '1rem', fontSize: '0.9rem' }}>
            Enter the one-time setup secret printed on this node's first start.
            It stops working once redeemed; mint a new one from Settings on a
            device that is already signed in.
          </p>

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
                  approvalMode={state.room.approval_mode}
                  onRotateInvitation={handleRotateInvitation}
                  onRemoveMember={handleRemoveMember}
                  onApprovalModeChanged={fetchState}
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
              This is a fresh one-time setup secret. Type or paste it into the
              other device's setup screen; it stops working the moment it is
              used. It is shown once, so copy it before closing this dialog.
            </p>

            {modalError && <div className="alert alert-error" style={{ marginBottom: '1rem' }}>{modalError}</div>}

            {modalToken ? (
              <div
                style={{
                  padding: '0.85rem',
                  background: 'var(--bg-primary)',
                  borderRadius: 'var(--radius)',
                  border: '1px solid var(--border-color)',
                  marginBottom: '1rem',
                  fontFamily: 'monospace',
                  fontSize: '0.95rem',
                  wordBreak: 'break-all',
                  textAlign: 'center',
                }}
              >
                {modalToken}
              </div>
            ) : (
              !modalError && (
                <div style={{ marginBottom: '1rem', fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
                  Minting a one-time secret...
                </div>
              )
            )}

            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', marginBottom: '1rem' }}>
              <button
                type="button"
                className="btn btn-primary"
                style={{ fontSize: '0.85rem' }}
                disabled={!modalToken}
                onClick={() => {
                  navigator.clipboard.writeText(modalToken)
                  setModalCopiedToken(true)
                  setTimeout(() => setModalCopiedToken(false), 2000)
                }}
              >
                {modalCopiedToken ? 'Copied!' : '🔑 Copy Setup Secret'}
              </button>
            </div>

            <div style={{ borderTop: '1px solid var(--border-color)', paddingTop: '0.75rem', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>Prefer your own phrase?</span>
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
