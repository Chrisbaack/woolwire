import React, { useState, useEffect, useRef } from 'react'
import { api } from '../api.ts'

interface Channel {
  id: string
  room_id: string
  name: string
  description: string
  created_at: number
}

interface MaterializedMessage {
  id: string
  room_id: string
  channel_id: string
  author_member_id: string
  author_display_name: string
  author_seq: number
  content: string
  timestamp: number
  edited: boolean
  deleted: boolean
  tombstoned: boolean
  tombstone_reason?: string
  replicated_status: string
}

interface CommunityProps {
  currentMemberID?: string
  role?: string
}

export const Community: React.FC<CommunityProps> = ({ currentMemberID, role }) => {
  const [channels, setChannels] = useState<Channel[]>([])
  const [activeChannelId, setActiveChannelId] = useState<string | null>(null)
  const [messages, setMessages] = useState<MaterializedMessage[]>([])
  const [inputContent, setInputContent] = useState('')
  const [editingMessageId, setEditingMessageId] = useState<string | null>(null)
  const [editContent, setEditContent] = useState('')

  const [newChanName, setNewChanName] = useState('')
  const [newChanDesc, setNewChanDesc] = useState('')
  const [showNewChanModal, setShowNewChanModal] = useState(false)
  const [isSyncing, setIsSyncing] = useState(false)

  const messagesEndRef = useRef<HTMLDivElement>(null)

  const fetchChannels = async () => {
    try {
      const res = await api('/api/v1/community/channels')
      if (res.ok) {
        const data: Channel[] = await res.json()
        setChannels(data || [])
        setActiveChannelId((prev) => {
          if (!prev && data && data.length > 0) {
            return data[0].id
          }
          if (prev && data && !data.some((c) => c.id === prev) && data.length > 0) {
            return data[0].id
          }
          return prev
        })
      }
    } catch {
      // ignore
    }
  }

  const fetchMessages = async (channelId: string) => {
    try {
      const res = await api(`/api/v1/community/channels/${channelId}/messages`)
      if (res.ok) {
        const data = await res.json()
        setMessages(data || [])
      }
    } catch {
      // ignore
    }
  }

  useEffect(() => {
    fetchChannels()
  }, [])

  // Poll channels periodically so newly created channels appear without refreshing
  useEffect(() => {
    const chanInterval = setInterval(() => {
      fetchChannels()
    }, 4000)
    return () => clearInterval(chanInterval)
  }, [])

  // Whenever active channel changes, immediately fetch messages and poll every 2s for real-time updates
  useEffect(() => {
    if (!activeChannelId) return
    fetchMessages(activeChannelId)

    const msgInterval = setInterval(() => {
      fetchMessages(activeChannelId)
    }, 2000)

    return () => clearInterval(msgInterval)
  }, [activeChannelId])

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages])

  const handleCreateChannel = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!newChanName.trim()) return
    try {
      const res = await api('/api/v1/community/channels', {
        method: 'POST',
          body: JSON.stringify({
          name: newChanName.trim(),
          description: newChanDesc.trim(),
        }),
      })
      if (res.ok) {
        const ch = await res.json()
        setChannels([...channels, ch])
        setActiveChannelId(ch.id)
        setShowNewChanModal(false)
        setNewChanName('')
        setNewChanDesc('')
      }
    } catch {
      // ignore
    }
  }

  const handleSendMessage = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!inputContent.trim() || !activeChannelId) return

    const content = inputContent.trim()
    setInputContent('')

    try {
      const res = await api(`/api/v1/community/channels/${activeChannelId}/messages`, {
        method: 'POST',
          body: JSON.stringify({ content }),
      })
      if (res.ok) {
        await fetchMessages(activeChannelId)
      }
    } catch {
      // ignore
    }
  }

  const handleSaveEdit = async (msgId: string) => {
    if (!editContent.trim() || !activeChannelId) return
    try {
      const res = await api(`/api/v1/community/messages/${msgId}`, {
        method: 'PUT',
          body: JSON.stringify({
          content: editContent.trim(),
          channel_id: activeChannelId,
        }),
      })
      if (res.ok) {
        setEditingMessageId(null)
        setEditContent('')
        await fetchMessages(activeChannelId)
      }
    } catch {
      // ignore
    }
  }

  const handleDeleteMessage = async (msgId: string) => {
    if (!confirm('Delete this message?')) return
    try {
      const res = await api(`/api/v1/community/messages/${msgId}`, {
        method: 'DELETE',
          body: JSON.stringify({ channel_id: activeChannelId }),
      })
      if (res.ok) {
        await fetchMessages(activeChannelId!)
      }
    } catch {
      // ignore
    }
  }

  const handleModerateMessage = async (msgId: string) => {
    const reason = prompt('Enter moderation reason to tombstone this message:', 'Inappropriate content')
    if (!reason) return
    try {
      const res = await api(`/api/v1/community/messages/${msgId}/moderate`, {
        method: 'POST',
          body: JSON.stringify({
          channel_id: activeChannelId,
          reason: reason.trim(),
        }),
      })
      if (res.ok) {
        await fetchMessages(activeChannelId!)
      }
    } catch {
      // ignore
    }
  }

  const handleManualSync = async () => {
    setIsSyncing(true)
    try {
      await api('/api/v1/community/sync', { method: 'POST' })
      await fetchChannels()
      if (activeChannelId) {
        await fetchMessages(activeChannelId)
      }
    } catch {
      // ignore
    } finally {
      setIsSyncing(false)
    }
  }

  const activeChannel = channels.find((c) => c.id === activeChannelId)

  return (
    <div className="conversation-layout">
      {/* Channels Sidebar */}
      <div className="card" style={{ padding: '1rem', display: 'flex', flexDirection: 'column' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <h3 style={{ fontSize: '1rem' }}>Channels</h3>
          <button className="btn btn-primary" style={{ padding: '0.4rem 0.6rem', fontSize: '0.8rem' }} onClick={() => setShowNewChanModal(true)}>
            + Add
          </button>
        </div>

        <div style={{ overflowY: 'auto', flex: 1, display: 'flex', flexDirection: 'column', gap: '0.35rem' }}>
          {channels.map((ch) => (
            <div
              key={ch.id}
              role="button"
              tabIndex={0}
              onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setActiveChannelId(ch.id) } }}
              onClick={() => setActiveChannelId(ch.id)}
              style={{
                padding: '0.5rem 0.75rem',
                borderRadius: 'var(--radius)',
                backgroundColor: activeChannelId === ch.id ? 'var(--bg-primary)' : 'transparent',
                border: '1px solid',
                borderColor: activeChannelId === ch.id ? 'var(--accent-primary)' : 'transparent',
                cursor: 'pointer',
                fontWeight: activeChannelId === ch.id ? 600 : 400,
                fontSize: '0.9rem',
              }}
            >
              # {ch.name}
            </div>
          ))}
        </div>

        <div style={{ marginTop: 'auto', borderTop: '1px solid var(--border-color)', paddingTop: '0.75rem' }}>
          <button
            className="btn btn-secondary"
            style={{ width: '100%', fontSize: '0.8rem', padding: '0.4rem' }}
            onClick={handleManualSync}
            disabled={isSyncing}
          >
            {isSyncing ? 'Syncing...' : 'Sync Channels'}
          </button>
        </div>
      </div>

      {/* Message View */}
      <div className="card" style={{ padding: '1rem', display: 'flex', flexDirection: 'column', height: '100%' }}>
        {activeChannel ? (
          <>
            {/* Channel Header */}
            <div style={{ paddingBottom: '0.75rem', borderBottom: '1px solid var(--border-color)', marginBottom: '1rem' }}>
              <h3 style={{ fontSize: '1.1rem' }}>#{activeChannel.name}</h3>
              {activeChannel.description && (
                <div style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', marginTop: '0.2rem' }}>
                  {activeChannel.description}
                </div>
              )}
            </div>

            {/* Messages Feed */}
            <div className="message-feed" style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: '0.75rem', minHeight: 0, paddingRight: '0.5rem', marginBottom: '1rem' }}>
              {messages.length === 0 ? (
                <div style={{ textAlign: 'center', color: 'var(--text-secondary)', marginTop: '4rem', fontSize: '0.9rem' }}>
                  No messages in #{activeChannel.name} yet. Send the first message!
                </div>
              ) : (
                messages.map((m) => (
                  <div
                    key={m.id}
                    style={{
                      padding: '0.6rem 0.75rem',
                      backgroundColor: 'var(--bg-primary)',
                      borderRadius: 'var(--radius)',
                      border: '1px solid var(--border-color)',
                      opacity: m.deleted || m.tombstoned ? 0.7 : 1,
                    }}
                  >
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.25rem' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                        <strong style={{ fontSize: '0.9rem' }}>
                          {m.author_display_name} {currentMemberID && m.author_member_id === currentMemberID && <span style={{ color: 'var(--accent-text)', fontSize: '0.75rem' }}>(You)</span>}
                        </strong>
                        <span style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                          {new Date(m.timestamp * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
                        </span>
                        {m.edited && (
                          <span style={{ fontSize: '0.7rem', color: 'var(--text-secondary)' }}>(edited)</span>
                        )}
                        <span style={{ fontSize: '0.65rem', backgroundColor: 'var(--bg-secondary)', padding: '0.1rem 0.4rem', borderRadius: '4px' }}>
                          {m.replicated_status}
                        </span>
                      </div>

                      {/* Action buttons */}
                      {!m.deleted && !m.tombstoned && (
                        <div style={{ display: 'flex', gap: '0.5rem' }}>
                          {(currentMemberID && m.author_member_id === currentMemberID) && (
                            <>
                              <button
                                onClick={() => {
                                  setEditingMessageId(m.id)
                                  setEditContent(m.content)
                                }}
                                style={{ background: 'none', border: 'none', color: 'var(--text-secondary)', cursor: 'pointer', fontSize: '0.75rem' }}
                              >
                                Edit
                              </button>
                              <button
                                onClick={() => handleDeleteMessage(m.id)}
                                style={{ background: 'none', border: 'none', color: 'var(--accent-danger)', cursor: 'pointer', fontSize: '0.75rem' }}
                              >
                                Delete
                              </button>
                            </>
                          )}
                          {role === 'creator' && (
                            <button
                              onClick={() => handleModerateMessage(m.id)}
                              style={{ background: 'none', border: 'none', color: 'orange', cursor: 'pointer', fontSize: '0.75rem' }}
                            >
                              Tombstone
                            </button>
                          )}
                        </div>
                      )}
                    </div>

                    {editingMessageId === m.id ? (
                      <div style={{ marginTop: '0.5rem' }}>
                        <input
                          type="text"
                          className="form-control"
                          value={editContent}
                          onChange={(e) => setEditContent(e.target.value)}
                          style={{ marginBottom: '0.5rem' }}
                        />
                        <div style={{ display: 'flex', gap: '0.5rem' }}>
                          <button className="btn btn-primary" style={{ padding: '0.3rem 0.6rem', fontSize: '0.8rem' }} onClick={() => handleSaveEdit(m.id)}>
                            Save
                          </button>
                          <button className="btn btn-secondary" style={{ padding: '0.3rem 0.6rem', fontSize: '0.8rem' }} onClick={() => setEditingMessageId(null)}>
                            Cancel
                          </button>
                        </div>
                      </div>
                    ) : (
                      <div style={{ fontSize: '0.95rem', whiteSpace: 'pre-wrap', fontStyle: m.deleted || m.tombstoned ? 'italic' : 'normal' }}>
                        {m.content}
                      </div>
                    )}
                  </div>
                ))
              )}
              <div ref={messagesEndRef} />
            </div>

            {/* Message input */}
            <form onSubmit={handleSendMessage} style={{ display: 'flex', gap: '0.75rem', marginTop: 'auto' }}>
              <input
                type="text"
                className="form-control"
                placeholder={`Message #${activeChannel.name}`}
                value={inputContent}
                onChange={(e) => setInputContent(e.target.value)}
              />
              <button type="submit" className="btn btn-primary" disabled={!inputContent.trim()}>
                Send
              </button>
            </form>
          </>
        ) : (
          <div style={{ margin: 'auto', color: 'var(--text-secondary)' }}>Select a channel</div>
        )}
      </div>

      {/* New Channel Modal */}
      {showNewChanModal && (
        <div
          style={{
            position: 'fixed',
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: 'rgba(0,0,0,0.6)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 100,
          }}
        >
          <div className="card" style={{ width: '400px', maxWidth: 'calc(100vw - 2rem)', maxHeight: '90dvh', overflowY: 'auto' }}>
            <h2>Create Channel</h2>
            <form onSubmit={handleCreateChannel}>
              <div className="form-group">
                <label>Channel Name</label>
                <input
                  type="text"
                  className="form-control"
                  placeholder="e.g. models or off-topic"
                  value={newChanName}
                  onChange={(e) => setNewChanName(e.target.value)}
                  required
                />
              </div>

              <div className="form-group">
                <label>Description (Optional)</label>
                <input
                  type="text"
                  className="form-control"
                  placeholder="What is this channel about?"
                  value={newChanDesc}
                  onChange={(e) => setNewChanDesc(e.target.value)}
                />
              </div>

              <div style={{ display: 'flex', gap: '0.75rem', justifyContent: 'flex-end', marginTop: '1.5rem' }}>
                <button type="button" className="btn btn-secondary" onClick={() => setShowNewChanModal(false)}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary">
                  Create
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  )
}
