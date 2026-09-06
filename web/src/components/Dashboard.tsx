import React, { useState, useEffect } from 'react'
import { api } from '../api.ts'

interface ModelAd {
  room_id: string
  host_member_id: string
  model_id: string
  name: string
  context_limit: number
  availability: string
  queue_estimate: number
  host_display_name: string
  is_managed: boolean
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

  const fetchCatalog = async () => {
    try {
      const res = await api('/api/v1/catalog')
      if (res.ok) {
        const data = await res.json()
        setCatalog(data || [])
      }
    } catch {
      // ignore
    } finally {
      setLoading(false)
    }
  }

  const fetchLeaderboard = async () => {
    try {
      const res = await api('/api/v1/contributions/leaderboard')
      if (res.ok) {
        const data = await res.json()
        setLeaderboard(data.leaderboard || [])
      }
    } catch {
      // ignore
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

  return (
    <div>
      <div className="card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <div>
            <h2>{roomName}</h2>
            <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
              <span className="badge">{role.toUpperCase()}</span>
              <span style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
                Signed in as <strong>{displayName}</strong>
              </span>
            </div>
          </div>
          <button
            className="btn btn-secondary"
            onClick={() => {
              if (confirm('Are you sure you want to leave this room?')) {
                onLeaveRoom()
              }
            }}
          >
            Leave Room
          </button>
        </div>

        <p style={{ fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
          Direct peer-to-peer encrypted mesh connected. Models advertised below are hosted directly on member hardware.
        </p>
      </div>

      <div className="card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <h2>Available Models ({catalog.length})</h2>
          <button className="btn btn-secondary" style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }} onClick={fetchCatalog}>
            Refresh
          </button>
        </div>

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
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: '1rem' }}>
            {catalog.map((m) => (
              <div
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
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '0.5rem' }}>
                    <h3 style={{ fontSize: '1.1rem', fontWeight: 600 }}>{m.name}</h3>
                    <span
                      className="badge"
                      style={{
                        backgroundColor: m.availability === 'ready' ? 'var(--accent-success)' : 'var(--border-color)',
                        fontSize: '0.7rem',
                      }}
                    >
                      {m.availability}
                    </span>
                  </div>

                  <div style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', marginBottom: '0.75rem' }}>
                    Hosted by <strong>{m.host_display_name}</strong>
                  </div>

                  <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', marginBottom: '1rem' }}>
                    <span style={{ fontSize: '0.75rem', backgroundColor: 'var(--bg-secondary)', padding: '0.2rem 0.5rem', borderRadius: '4px' }}>
                      Context: {m.context_limit.toLocaleString()}
                    </span>
                    <span style={{ fontSize: '0.75rem', backgroundColor: 'var(--bg-secondary)', padding: '0.2rem 0.5rem', borderRadius: '4px' }}>
                      Queue: {m.queue_estimate}
                    </span>
                    {m.is_managed && (
                      <span style={{ fontSize: '0.75rem', backgroundColor: 'var(--accent-primary)', padding: '0.2rem 0.5rem', borderRadius: '4px' }}>
                        Managed
                      </span>
                    )}
                  </div>
                </div>

                <button
                  className="btn btn-primary"
                  style={{ width: '100%', fontSize: '0.9rem', padding: '0.5rem' }}
                  onClick={() => onSelectModelForChat(m)}
                >
                  Chat with Model
                </button>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Hosting Leaderboard (30-day leaderboard) */}
      <div className="card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.75rem' }}>
          <div>
            <h2>Hosting Leaderboard</h2>
            <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', marginTop: '0.25rem' }}>
              Rolling 30-day leaderboard of members hosting local compute for the room.
            </p>
          </div>
          <button className="btn btn-secondary" style={{ padding: '0.35rem 0.75rem', fontSize: '0.85rem' }} onClick={fetchLeaderboard}>
            Refresh
          </button>
        </div>

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
                  <th style={{ padding: '0.6rem 0.5rem' }}>Requests Served</th>
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
                    <td style={{ padding: '0.6rem 0.5rem', color: 'var(--accent-primary)', fontWeight: 600 }}>
                      {entry.score} {entry.score === 1 ? 'request' : 'requests'}
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
    </div>
  )
}
