import React, { useState, useEffect } from 'react'
import { api } from '../api.ts'
import { availabilityColor, availabilityLabel, isUnavailable } from '../availability.ts'

interface ModelAd {
  room_id: string
  host_member_id: string
  model_id: string
  name: string
  context_limit: number
  availability: string
  queue_estimate: number
  host_display_name: string
  is_managed?: boolean
}

interface LeaderboardEntry {
  host_member_id: string
  display_name: string
  score: number
  distinct_members_helped: number
}

interface DashboardProps {
  roomName: string
  role: string
  displayName: string
  onLeaveRoom: () => Promise<void>
  onSelectModelForChat: (model: ModelAd) => void
}

export const Dashboard: React.FC<DashboardProps> = ({
  roomName,
  role,
  displayName,
  onLeaveRoom,
  onSelectModelForChat,
}) => {
  const [catalog, setCatalog] = useState<ModelAd[]>([])
  const [leaderboard, setLeaderboard] = useState<LeaderboardEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [catalogError, setCatalogError] = useState('')
  const [leaderboardError, setLeaderboardError] = useState('')
  const [leaveError, setLeaveError] = useState('')
  const [search, setSearch] = useState('')
  const [usableOnly, setUsableOnly] = useState(false)

  const fetchCatalog = async () => {
    try {
      const res = await api('/api/v1/catalog')
      if (!res.ok) throw new Error('Catalog unavailable')
      setCatalogError('')
      if (res.ok) {
        const data = await res.json()
        setCatalog(data || [])
      }
    } catch {
      setCatalogError('Could not refresh models. The list may be out of date.')
    } finally {
      setLoading(false)
    }
  }

  const fetchLeaderboard = async () => {
    try {
      const res = await api('/api/v1/contributions/leaderboard')
      if (!res.ok) throw new Error('Leaderboard unavailable')
      setLeaderboardError('')
      if (res.ok) {
        const data = await res.json()
        setLeaderboard(data.leaderboard || [])
      }
    } catch {
      setLeaderboardError('Could not refresh contributions. Please try again.')
    }
  }

  useEffect(() => {
    fetchCatalog()
    fetchLeaderboard()
    const interval = setInterval(() => {
      fetchCatalog()
      fetchLeaderboard()
    }, 15000)
    return () => clearInterval(interval)
  }, [])

  // Filtering on 'ready' would hide exactly the models this room now offers
  // on demand, so the filter asks whether a model can take a request at all:
  // a downloaded one its host will load counts.
  const visibleModels = catalog.filter(m => (!usableOnly || !isUnavailable(m.availability)) && `${m.name} ${m.host_display_name}`.toLowerCase().includes(search.toLowerCase()))

  return (
    <div className="dashboard-layout">
      <section className="meadow-hero">
        <div><span className="eyebrow">THE MEADOW / {role.toUpperCase()}</span>
          <h2>A place for bright ideas.</h2>
          <p>Welcome back, {displayName || 'friend'}. Pull up a chair in <strong>{roomName}</strong>.<br />Explore a model, start a conversation, and see what grows.</p>
          <span className="hero-caption">Private room · Shared compute · A little curiosity</span>
        </div>
        <div className="meadow-art" aria-hidden="true"><span className="moon">✦</span><span className="hill hill-back" /><span className="hill hill-front" /><span className="sheep">🐑</span><span className="flower">✿</span></div>
      </section>
      <div className="room-stats" aria-label="Room overview">
        <div><strong>{catalog.length}</strong><span>Models in the meadow</span></div>
        <div><strong>{catalog.filter(m => !isUnavailable(m.availability)).length}</strong><span>Usable by peers</span></div>
        <div><strong>{catalog.filter(m => m.availability === 'ready').length}</strong><span>Ready now</span></div>
        <div><strong>{new Set(catalog.map(m => m.host_member_id)).size}</strong><span>Hosts sharing models</span></div>
      </div>
      <div className="card model-catalog">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <div><span className="eyebrow">PICK A THINKING PARTNER</span><h2>Models in your room <span className="badge">{catalog.length}</span></h2></div>
          <button className="btn btn-secondary" style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }} onClick={fetchCatalog}>
            Refresh
          </button>
        </div>

        <div className="catalog-tools"><input className="form-control" aria-label="Search models or hosts" placeholder="Find a model or a friend…" value={search} onChange={e => setSearch(e.target.value)} /><label title="Hides models whose host is offline. Downloaded models that load on request are still shown."><input type="checkbox" checked={usableOnly} onChange={e => setUsableOnly(e.target.checked)} /> Usable only</label></div>
        <p style={{ color: 'var(--text-secondary)', fontSize: '0.8rem', margin: '0 0 1rem' }}>
          Models marked <strong>Ready</strong> are currently advertised by their host. Downloaded managed models marked <strong>Starts on request</strong> are ready to use too; their host loads them when your first message arrives.
        </p>
        {catalogError && <div role="alert" className="alert alert-error">{catalogError}</div>}
        {loading ? (
          <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem' }}>Discovering models from peers...</p>
        ) : catalog.length === 0 ? (
          <div style={{ textAlign: 'center', padding: '2rem 1rem', color: 'var(--text-secondary)' }}>
            <p>No models currently advertised in this room.</p>
            <p style={{ fontSize: '0.85rem', marginTop: '0.5rem' }}>
              Host a model in <strong>Settings</strong> to offer inference to friends.
            </p>
          </div>
        ) : (
          <div className="model-grid">
            {visibleModels.length === 0 && <p className="empty-state">No models match. Try another search or turn off “Usable only”.</p>}
            {visibleModels.map((m) => (
              <div
                className="model-tile"
                key={`${m.host_member_id}/${m.model_id}`}
                style={{
                  padding: '1.25rem',
                  backgroundColor: 'var(--bg-primary)',
                  borderRadius: 'var(--radius)',
                  border: '1px solid var(--border-color)',
                  display: 'flex',
                  flexDirection: 'column',
                  justifyContent: 'space-between',
                }}
              >
                <div>
                  {/* The status badge cannot shrink, so on a narrow tile it
                      would squeeze a long model name into a one-character
                      column. Let it drop to its own line instead. */}
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: '0.5rem', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '0.5rem' }}>
                    <h3 style={{ fontSize: '1.1rem', fontWeight: 600 }}>{m.name}</h3>
                    <span
                      className="badge"
                      style={{
                        backgroundColor: availabilityColor(m.availability),
                        fontSize: '0.7rem',
                      }}
                      aria-label={`Model status: ${availabilityLabel(m.availability, m.is_managed)}`}
                    >
                      {availabilityLabel(m.availability, m.is_managed)}
                    </span>
                  </div>

                  <div style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', marginBottom: '0.75rem' }}>
                    Hosted by <strong>{m.host_display_name}</strong>
                  </div>

                  <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', marginBottom: '1rem' }}>
                    <span style={{ fontSize: '0.75rem', backgroundColor: 'var(--bg-secondary)', padding: '0.2rem 0.5rem', borderRadius: '4px' }}>
                      Context: {m.context_limit >= 1024 ? `${(m.context_limit / 1024).toFixed(0)}k` : m.context_limit.toLocaleString()}
                    </span>
                    <span style={{ fontSize: '0.75rem', backgroundColor: 'var(--bg-secondary)', padding: '0.2rem 0.5rem', borderRadius: '4px' }}>
                      Queue: {m.queue_estimate === 0 ? 'Clear' : m.queue_estimate}
                    </span>
                    {m.is_managed && (
                      <span style={{ fontSize: '0.75rem', backgroundColor: 'var(--accent-primary)', padding: '0.2rem 0.5rem', borderRadius: '4px', color: 'white' }}>
                        🚀 Managed Runner
                      </span>
                    )}
                  </div>
                  {m.availability === 'unloaded' && (
                    <p style={{ fontSize: '0.78rem', color: 'var(--text-secondary)', margin: '0 0 1rem' }}>
                      The host keeps this downloaded model ready on disk and will load it when you send your first message.
                    </p>
                  )}
                </div>

                <button
                  className="btn btn-primary"
                  style={{ width: '100%', fontSize: '0.9rem', padding: '0.5rem' }}
                  onClick={() => onSelectModelForChat(m)}
                  disabled={isUnavailable(m.availability)}
                  title={isUnavailable(m.availability) ? 'This host is currently unavailable' : undefined}
                >
                  {isUnavailable(m.availability) ? 'Unavailable' : 'Start a conversation ↗'}
                </button>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Hosting Leaderboard (30-day leaderboard) */}
      <div className="card contribution-panel">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.75rem' }}>
          <div>
            <span className="eyebrow">A LITTLE APPRECIATION</span><h2>The helping herd</h2>
            <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', marginTop: '0.25rem' }}>
              Rolling 30-day leaderboard of members hosting local compute for the room.
            </p>
          </div>
          <button className="btn btn-secondary" style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }} onClick={fetchLeaderboard}>
            Refresh
          </button>
        </div>

        {leaderboardError && <div className="alert alert-error" role="alert">{leaderboardError}</div>}
        {leaderboard.length === 0 ? (
          <p style={{ color: 'var(--text-secondary)', fontSize: '0.9rem', fontStyle: 'italic' }}>
            No hosted requests in the last 30 days yet. Complete inference requests between peers to populate the leaderboard!
          </p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '0.9rem' }}>
              <thead>
                <tr style={{ borderBottom: '1px solid var(--border-color)', color: 'var(--text-secondary)' }}>
                  <th style={{ padding: '0.6rem 0.5rem' }}>Rank</th>
                  <th style={{ padding: '0.6rem 0.5rem' }}>Host Member</th>
                  <th style={{ padding: '0.6rem 0.5rem' }}>Points</th>
                  <th style={{ padding: '0.6rem 0.5rem' }}>Members Helped</th>
                </tr>
              </thead>
              <tbody>
                {leaderboard.map((entry, idx) => (
                  <tr key={entry.host_member_id} style={{ borderBottom: '1px solid var(--border-color)' }}>
                    <td style={{ padding: '0.6rem 0.5rem', fontWeight: 600 }}>
                      {idx === 0 ? '🥇 1' : idx === 1 ? '🥈 2' : idx === 2 ? '🥉 3' : `${idx + 1}`}
                    </td>
                    <td style={{ padding: '0.6rem 0.5rem' }}>
                      <strong>{entry.display_name}</strong>
                    </td>
                    <td style={{ padding: '0.6rem 0.5rem', color: 'var(--accent-text)', fontWeight: 600 }}>
                      {entry.score}
                    </td>
                    <td style={{ padding: '0.6rem 0.5rem' }}>
                      {entry.distinct_members_helped} {entry.distinct_members_helped === 1 ? 'member' : 'members'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      <div className="room-footer"><p>Models run on your friends’ hardware. Your chats stay in your installation.</p>
        <button className="btn btn-secondary" onClick={async () => {
          if (!confirm('Are you sure you want to leave this room?')) return
          try { await onLeaveRoom() } catch (err) { setLeaveError(err instanceof Error ? err.message : 'Could not leave room') }
        }}>Leave room</button>
        {leaveError && <div role="alert" className="alert alert-error">{leaveError}</div>}
      </div>
    </div>
  )
}
