import React, { useState, useEffect, useRef } from 'react'
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
}

interface Conversation {
  ID: string
  Title: string
  NoSave: boolean
  CreatedAt: number
  UpdatedAt: number
}

interface Message {
  ID: string
  ConversationID: string
  Role: string
  Content: string
  HostMemberID: string
  ModelID: string
  CreatedAt: number
}

interface MyChatsProps {
  initialModel?: ModelAd | null
}

export const MyChats: React.FC<MyChatsProps> = ({ initialModel }) => {
  const [conversations, setConversations] = useState<Conversation[]>([])
  const [activeConvId, setActiveConvId] = useState<string | null>(null)
  const [messages, setMessages] = useState<Message[]>([])
  const [models, setModels] = useState<ModelAd[]>([])
  const [selectedModelKey, setSelectedModelKey] = useState<string>('')
  const [inputContent, setInputContent] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [streamingText, setStreamingText] = useState('')
  const [streamingRequestId, setStreamingRequestId] = useState('')
  const [streamingHostId, setStreamingHostId] = useState('')
  const [errorMsg, setErrorMsg] = useState('')
  const [newTitle, setNewTitle] = useState('')
  const [newNoSave, setNewNoSave] = useState(false)
  const [showNewModal, setShowNewModal] = useState(false)
  const [modelNameMap, setModelNameMap] = useState<Record<string, string>>(() => {
    try {
      return JSON.parse(localStorage.getItem('woolwire_model_names') || '{}')
    } catch {
      return {}
    }
  })

  const messagesEndRef = useRef<HTMLDivElement>(null)

  const fetchConversations = async () => {
    try {
      const res = await api('/api/v1/chats')
      if (res.ok) {
        const data = await res.json()
        setConversations(data || [])
        if (!activeConvId && data && data.length > 0) {
          setActiveConvId(data[0].ID)
        }
      }
    } catch {
      // ignore
    }
  }

  const fetchCatalog = async () => {
    try {
      const res = await api('/api/v1/catalog')
      if (res.ok) {
        const data: ModelAd[] = await res.json()
        setModels(data || [])
        if (!selectedModelKey && data && data.length > 0) {
          if (initialModel) {
            setSelectedModelKey(`${initialModel.host_member_id}/${initialModel.model_id}`)
          } else {
            setSelectedModelKey(`${data[0].host_member_id}/${data[0].model_id}`)
          }
        }
        if (Array.isArray(data)) {
          setModelNameMap((prev) => {
            const next = { ...prev }
            for (const mod of data) {
              if (mod.model_id && mod.name) {
                next[mod.model_id] = mod.name
              }
            }
            try {
              localStorage.setItem('woolwire_model_names', JSON.stringify(next))
            } catch {}
            return next
          })
        }
      }
      try {
        const resLocal = await api('/api/v1/hosted-models')
        if (resLocal.ok) {
          const localModels = await resLocal.json()
          if (Array.isArray(localModels)) {
            setModelNameMap((prev) => {
              const next = { ...prev }
              for (const lm of localModels) {
                const id = lm.id || lm.ID
                const name = lm.name || lm.Name
                if (id && name) {
                  next[id] = name
                }
              }
              try {
                localStorage.setItem('woolwire_model_names', JSON.stringify(next))
              } catch {}
              return next
            })
          }
        }
      } catch {}
    } catch {
      // ignore
    }
  }

  const fetchChatMessages = async (id: string) => {
    try {
      const res = await api(`/api/v1/chats/${id}`)
      if (res.ok) {
        const data = await res.json()
        setMessages(data.messages || [])
      }
    } catch {
      // ignore
    }
  }

  useEffect(() => {
    fetchConversations()
    fetchCatalog()
  }, [])

  useEffect(() => {
    if (activeConvId) {
      fetchChatMessages(activeConvId)
    } else {
      setMessages([])
    }
  }, [activeConvId])

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages, streamingText])

  const handleCreateChat = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      const res = await api('/api/v1/chats', {
        method: 'POST',
          body: JSON.stringify({
          title: newTitle.trim() || 'New Chat',
          no_save: newNoSave,
        }),
      })
      if (res.ok) {
        const conv = await res.json()
        setConversations([conv, ...conversations])
        setActiveConvId(conv.ID)
        setShowNewModal(false)
        setNewTitle('')
        setNewNoSave(false)
      }
    } catch (err: any) {
      setErrorMsg(err.message || 'Failed to create chat')
    }
  }

  const handleDeleteChat = async (id: string) => {
    if (!confirm('Delete this conversation?')) return
    try {
      const res = await api(`/api/v1/chats/${id}`, { method: 'DELETE' })
      if (res.ok) {
        const updated = conversations.filter((c) => c.ID !== id)
        setConversations(updated)
        if (activeConvId === id) {
          setActiveConvId(updated.length > 0 ? updated[0].ID : null)
        }
      }
    } catch {
      // ignore
    }
  }

  const handleSendMessage = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!inputContent.trim() || !activeConvId || streaming) return

    const [hostId, modelId] = selectedModelKey.split('/')
    if (!hostId || !modelId) {
      setErrorMsg('Please select a valid model from the catalog')
      return
    }

    const currentContent = inputContent.trim()
    setInputContent('')
    setErrorMsg('')
    setStreaming(true)
    setStreamingText('')
    setStreamingHostId(hostId)

    const activeConv = conversations.find((c) => c.ID === activeConvId)
    const isNoSave = activeConv?.NoSave || false

    // Optimistically show user message in UI
    const tempUserMsg: Message = {
      ID: 'temp-user',
      ConversationID: activeConvId,
      Role: 'user',
      Content: currentContent,
      HostMemberID: hostId,
      ModelID: modelId,
      CreatedAt: Math.floor(Date.now() / 1000),
    }
    setMessages((prev) => [...prev, tempUserMsg])

    try {
      const res = await api(`/api/v1/chats/${activeConvId}/message`, {
        method: 'POST',
          body: JSON.stringify({
          content: currentContent,
          host_member_id: hostId,
          model_id: modelId,
        }),
      })

      if (!res.ok) {
        const errText = await res.text()
        throw new Error(errText || `Inference error: ${res.status}`)
      }

      const reader = res.body?.getReader()
      const decoder = new TextDecoder()
      let accumulated = ''

      if (reader) {
        while (true) {
          const { done, value } = await reader.read()
          if (done) break

          const chunk = decoder.decode(value, { stream: true })
          const lines = chunk.split('\n')
          for (const line of lines) {
            if (line.startsWith('data: ')) {
              const dataStr = line.replace('data: ', '').trim()
              if (dataStr === '[DONE]') {
                break
              }
              try {
                const parsed = JSON.parse(dataStr)
                if (parsed.delta) {
                  accumulated += parsed.delta
                  setStreamingText(accumulated)
                }
                if (parsed.request_id) {
                  setStreamingRequestId(parsed.request_id)
                }
              } catch {
                // ignore
              }
            } else if (line.startsWith('event: error')) {
              // explicit error event
              setErrorMsg('Host error or queue interrupted')
            }
          }
        }
      }

      setStreaming(false)
      setStreamingText('')
      setStreamingRequestId('')

      // Reload messages from store if not in no_save mode
      if (!isNoSave) {
        await fetchChatMessages(activeConvId)
      } else {
        // Keep in memory for active view
        const asstMsg: Message = {
          ID: 'temp-asst',
          ConversationID: activeConvId,
          Role: 'assistant',
          Content: accumulated,
          HostMemberID: hostId,
          ModelID: modelId,
          CreatedAt: Math.floor(Date.now() / 1000),
        }
        setMessages((prev) => [...prev, asstMsg])
      }
    } catch (err: any) {
      setErrorMsg(err.message || 'Error communicating with model host')
      setStreaming(false)
    }
  }

  const handleCancel = async () => {
    if (!activeConvId || !streamingRequestId || !streamingHostId) return
    try {
      await api(`/api/v1/chats/${activeConvId}/cancel`, {
        method: 'POST',
          body: JSON.stringify({
          request_id: streamingRequestId,
          host_member_id: streamingHostId,
        }),
      })
      setStreaming(false)
      setErrorMsg('Inference cancelled by user')
    } catch {
      // ignore
    }
  }

  const activeConv = conversations.find((c) => c.ID === activeConvId)
  const selectedModel = models.find(
    (m) => `${m.host_member_id}/${m.model_id}` === selectedModelKey
  )

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '260px 1fr', gap: '1.5rem', minHeight: '520px' }}>
      {/* Sidebar */}
      <div className="card" style={{ padding: '1rem', display: 'flex', flexDirection: 'column' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <h3 style={{ fontSize: '1rem' }}>My Chats</h3>
          <button className="btn btn-primary" style={{ padding: '0.4rem 0.75rem', fontSize: '0.85rem' }} onClick={() => setShowNewModal(true)}>
            + New
          </button>
        </div>

        <div style={{ overflowY: 'auto', flex: 1, display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          {conversations.length === 0 ? (
            <p style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', textAlign: 'center', marginTop: '2rem' }}>
              No conversations yet
            </p>
          ) : (
            conversations.map((c) => (
              <div
                key={c.ID}
                onClick={() => setActiveConvId(c.ID)}
                style={{
                  padding: '0.6rem 0.75rem',
                  borderRadius: 'var(--radius)',
                  backgroundColor: activeConvId === c.ID ? 'var(--bg-primary)' : 'transparent',
                  border: '1px solid',
                  borderColor: activeConvId === c.ID ? 'var(--accent-primary)' : 'transparent',
                  cursor: 'pointer',
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                }}
              >
                <div style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  <div style={{ fontSize: '0.9rem', fontWeight: 500 }}>{c.Title}</div>
                  {c.NoSave && (
                    <span style={{ fontSize: '0.7rem', color: 'var(--accent-danger)' }}>
                      [no-save]
                    </span>
                  )}
                </div>
                <button
                  onClick={(e) => {
                    e.stopPropagation()
                    handleDeleteChat(c.ID)
                  }}
                  style={{
                    background: 'none',
                    border: 'none',
                    color: 'var(--text-secondary)',
                    cursor: 'pointer',
                    fontSize: '1rem',
                  }}
                >
                  &times;
                </button>
              </div>
            ))
          )}
        </div>
      </div>

      {/* Chat Area */}
      <div className="card" style={{ padding: '1rem', display: 'flex', flexDirection: 'column', height: '100%' }}>
        {activeConv ? (
          <>
            {/* Chat Header */}
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', paddingBottom: '0.75rem', borderBottom: '1px solid var(--border-color)', marginBottom: '1rem' }}>
              <div>
                <h3 style={{ fontSize: '1.1rem' }}>{activeConv.Title}</h3>
                {activeConv.NoSave && (
                  <span style={{ fontSize: '0.75rem', color: 'var(--accent-danger)' }}>
                    Privacy mode: prompts are not saved locally or remotely
                  </span>
                )}
              </div>

              <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <label style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>Model:</label>
                <select
                  className="form-control"
                  style={{ width: 'auto', padding: '0.35rem 0.75rem', fontSize: '0.85rem' }}
                  value={selectedModelKey}
                  onChange={(e) => setSelectedModelKey(e.target.value)}
                  disabled={streaming}
                >
                  {models.length === 0 ? (
                    <option value="">No models available</option>
                  ) : (
                    models.map((m) => (
                      <option key={`${m.host_member_id}/${m.model_id}`} value={`${m.host_member_id}/${m.model_id}`}>
                        {m.name} ({m.host_display_name})
                      </option>
                    ))
                  )}
                </select>
              </div>
            </div>

            {errorMsg && <div className="alert alert-error">{errorMsg}</div>}

            {/* Messages Feed */}
            <div style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: '1rem', paddingRight: '0.5rem', marginBottom: '1rem', maxHeight: '400px' }}>
              {messages.length === 0 && !streaming && (
                <div style={{ textAlign: 'center', color: 'var(--text-secondary)', marginTop: '4rem', fontSize: '0.9rem' }}>
                  Ask anything to begin chatting with <strong>{selectedModel?.name || 'the selected model'}</strong>.
                </div>
              )}

              {messages.map((m, idx) => (
                <div
                  key={m.ID || idx}
                  style={{
                    alignSelf: m.Role === 'user' ? 'flex-end' : 'flex-start',
                    maxWidth: '80%',
                    backgroundColor: m.Role === 'user' ? 'var(--accent-primary)' : 'var(--bg-primary)',
                    color: 'white',
                    padding: '0.75rem 1rem',
                    borderRadius: 'var(--radius)',
                    border: m.Role === 'assistant' ? '1px solid var(--border-color)' : 'none',
                  }}
                >
                  {m.Role === 'assistant' && (
                    <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginBottom: '0.3rem' }}>
                      Hosted by {models.find((mod) => mod.host_member_id === m.HostMemberID)?.host_display_name || 'Peer'} ({modelNameMap[m.ModelID] || models.find((mod) => mod.model_id === m.ModelID)?.name || m.ModelID})
                    </div>
                  )}
                  <div style={{ whiteSpace: 'pre-wrap', fontSize: '0.95rem' }}>{m.Content}</div>
                </div>
              ))}

              {streaming && (
                <div
                  style={{
                    alignSelf: 'flex-start',
                    maxWidth: '80%',
                    backgroundColor: 'var(--bg-primary)',
                    color: 'white',
                    padding: '0.75rem 1rem',
                    borderRadius: 'var(--radius)',
                    border: '1px solid var(--border-color)',
                  }}
                >
                  <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginBottom: '0.3rem' }}>
                    Generating response...
                  </div>
                  <div style={{ whiteSpace: 'pre-wrap', fontSize: '0.95rem' }}>
                    {streamingText}
                    <span className="cursor-blink">|</span>
                  </div>
                </div>
              )}
              <div ref={messagesEndRef} />
            </div>

            {/* Input Form */}
            <form onSubmit={handleSendMessage} style={{ display: 'flex', gap: '0.75rem', marginTop: 'auto' }}>
              <input
                type="text"
                className="form-control"
                placeholder={streaming ? 'Generating...' : 'Type a message...'}
                value={inputContent}
                onChange={(e) => setInputContent(e.target.value)}
                disabled={streaming || !selectedModelKey}
              />
              {streaming ? (
                <button type="button" className="btn btn-secondary" onClick={handleCancel}>
                  Cancel
                </button>
              ) : (
                <button type="submit" className="btn btn-primary" disabled={!inputContent.trim() || !selectedModelKey}>
                  Send
                </button>
              )}
            </form>
          </>
        ) : (
          <div style={{ textAlign: 'center', margin: 'auto', color: 'var(--text-secondary)' }}>
            Select or create a conversation to start chatting
          </div>
        )}
      </div>

      {/* New Conversation Modal */}
      {showNewModal && (
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
          <div className="card" style={{ width: '400px' }}>
            <h2>New Conversation</h2>
            <form onSubmit={handleCreateChat}>
              <div className="form-group">
                <label>Conversation Title</label>
                <input
                  type="text"
                  className="form-control"
                  placeholder="e.g. Code Review"
                  value={newTitle}
                  onChange={(e) => setNewTitle(e.target.value)}
                />
              </div>

              <div className="form-group" style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <input
                  type="checkbox"
                  id="noSaveCheckbox"
                  checked={newNoSave}
                  onChange={(e) => setNewNoSave(e.target.checked)}
                />
                <label htmlFor="noSaveCheckbox" style={{ marginBottom: 0, cursor: 'pointer' }}>
                  <strong>No-Save Mode</strong> (Never write prompts to disk)
                </label>
              </div>

              <div style={{ display: 'flex', gap: '0.75rem', justifyContent: 'flex-end', marginTop: '1.5rem' }}>
                <button type="button" className="btn btn-secondary" onClick={() => setShowNewModal(false)}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary">
                  Create Chat
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  )
}
