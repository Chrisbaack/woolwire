import React, { useState, useEffect, useRef } from 'react'
import { api } from '../api.ts'
import { readSSEStream } from '../sse.ts'

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
  // Regenerating an answer or editing a question stores the new take beside
  // the old one. These describe where this message sits among those takes.
  ParentID?: string
  VariantIndex?: number
  VariantCount?: number
  SiblingIDs?: string[]
}

export interface GenerationStats {
  request_id?: string
  ttft_ms?: number
  total_ms?: number
  completion_tokens?: number
  prompt_tokens_est?: number
  tokens_per_second?: number
}

interface MessageContentProps {
  content: string
  isStreaming?: boolean
}

export const MessageContent: React.FC<MessageContentProps> = ({ content, isStreaming }) => {
  const [copiedCodeIdx, setCopiedCodeIdx] = useState<number | null>(null)

  // Parse <think>...</think> if present
  let thinkContent: string | null = null
  let mainContent = content
  let isCurrentlyThinking = false

  const thinkStart = content.indexOf('<think>')
  if (thinkStart !== -1) {
    const thinkEnd = content.indexOf('</think>')
    if (thinkEnd !== -1) {
      thinkContent = content.slice(thinkStart + 7, thinkEnd).trim()
      mainContent = (content.slice(0, thinkStart) + content.slice(thinkEnd + 8)).trim()
    } else {
      thinkContent = content.slice(thinkStart + 7).trim()
      mainContent = content.slice(0, thinkStart).trim()
      isCurrentlyThinking = Boolean(isStreaming)
    }
  }

  const [thinkExpanded, setThinkExpanded] = useState<boolean>(Boolean(isStreaming) || !mainContent)

  // Parse markdown code blocks ```lang\ncode\n```
  const renderTextWithCodeBlocks = (text: string) => {
    const parts = text.split(/(```[\s\S]*?```)/g)
    let codeIndex = 0

    return parts.map((part, idx) => {
      if (part.startsWith('```') && part.endsWith('```')) {
        const currentIdx = codeIndex++
        const match = part.match(/^```([a-zA-Z0-9_-]*)\n([\s\S]*?)```$/)
        const lang = match ? match[1] || 'code' : 'code'
        const codeText = match ? match[2] : part.slice(3, -3)

        const handleCopy = () => {
          navigator.clipboard.writeText(codeText)
          setCopiedCodeIdx(currentIdx)
          setTimeout(() => setCopiedCodeIdx(null), 2000)
        }

        return (
          <div
            key={idx}
            style={{
              margin: '0.6rem 0',
              borderRadius: '6px',
              backgroundColor: '#111827',
              border: '1px solid #374151',
              overflow: 'hidden',
            }}
          >
            <div
              style={{
                display: 'flex',
                justifyContent: 'space-between',
                alignItems: 'center',
                padding: '0.3rem 0.75rem',
                backgroundColor: '#1f2937',
                borderBottom: '1px solid #374151',
                fontSize: '0.75rem',
                color: '#9ca3af',
              }}
            >
              <span>{lang}</span>
              <button
                type="button"
                onClick={handleCopy}
                style={{
                  background: 'none',
                  border: 'none',
                  color: copiedCodeIdx === currentIdx ? '#10b981' : '#9ca3af',
                  cursor: 'pointer',
                  fontSize: '0.75rem',
                  padding: '0.1rem 0.4rem',
                  borderRadius: '3px',
                }}
              >
                {copiedCodeIdx === currentIdx ? '✓ Copied' : 'Copy'}
              </button>
            </div>
            <pre
              style={{
                margin: 0,
                padding: '0.75rem',
                overflowX: 'auto',
                fontSize: '0.85rem',
                lineHeight: '1.45',
                color: '#f3f4f6',
                fontFamily: 'monospace',
              }}
            >
              <code>{codeText}</code>
            </pre>
          </div>
        )
      }

      return (
        <span key={idx} style={{ whiteSpace: 'pre-wrap' }}>
          {part}
        </span>
      )
    })
  }

  return (
    <div>
      {thinkContent && (
        <div
          style={{
            marginBottom: '0.75rem',
            borderRadius: '6px',
            border: '1px solid #4b5563',
            backgroundColor: 'rgba(31, 41, 55, 0.6)',
            overflow: 'hidden',
          }}
        >
          <div
            onClick={() => setThinkExpanded(!thinkExpanded)}
            style={{
              padding: '0.4rem 0.75rem',
              display: 'flex',
              justifyContent: 'space-between',
              alignItems: 'center',
              cursor: 'pointer',
              fontSize: '0.8rem',
              color: '#9ca3af',
              backgroundColor: 'rgba(55, 65, 81, 0.4)',
              userSelect: 'none',
            }}
          >
            <span style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
              <span>🧠</span>
              <span>{isCurrentlyThinking ? 'Thinking...' : 'Reasoning Process'}</span>
            </span>
            <span style={{ fontSize: '0.75rem', color: 'var(--accent-text)' }}>
              {thinkExpanded ? 'Hide' : 'Show'}
            </span>
          </div>
          {thinkExpanded && (
            <div
              style={{
                padding: '0.6rem 0.75rem',
                fontSize: '0.8rem',
                color: '#d1d5db',
                fontStyle: 'italic',
                borderTop: '1px solid #374151',
                whiteSpace: 'pre-wrap',
                maxHeight: '260px',
                overflowY: 'auto',
                lineHeight: '1.4',
              }}
            >
              {thinkContent}
              {isCurrentlyThinking && <span className="cursor-blink">|</span>}
            </div>
          )}
        </div>
      )}

      {mainContent ? (
        <div style={{ fontSize: '0.95rem' }}>{renderTextWithCodeBlocks(mainContent)}</div>
      ) : isStreaming ? (
        <div style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', fontStyle: 'italic', display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
          <span>{isCurrentlyThinking ? 'Thinking...' : 'Formulating answer...'}</span>
          <span className="cursor-blink">|</span>
        </div>
      ) : thinkContent && !mainContent ? (
        <div style={{ fontSize: '0.85rem', color: 'var(--text-secondary)', fontStyle: 'italic', marginTop: '0.4rem', opacity: 0.85 }}>
          (Model completed reasoning but produced no response text. This typically happens when the model exceeds its maximum token budget during reasoning.)
        </div>
      ) : null}
    </div>
  )
}

// Message actions sit under the bubble and should read as quiet affordances,
// not as primary buttons competing with the composer.
const msgActionStyle: React.CSSProperties = {
  background: 'none',
  border: 'none',
  color: 'var(--text-secondary)',
  cursor: 'pointer',
  fontSize: '0.72rem',
  padding: '0.15rem 0.4rem',
  borderRadius: '3px',
  lineHeight: 1.4,
}

interface MyChatsProps {
  initialModel?: ModelAd | null
  /** False while another tab is showing. The component stays mounted so a
      generation in flight keeps streaming; only cosmetic work is skipped. */
  visible?: boolean
}

export const MyChats: React.FC<MyChatsProps> = ({ initialModel, visible = true }) => {
  const [conversations, setConversations] = useState<Conversation[]>([])
  const [activeConvId, setActiveConvId] = useState<string | null>(null)
  const [messages, setMessages] = useState<Message[]>([])
  const [models, setModels] = useState<ModelAd[]>([])
  // The chosen model outlives both a reload and a catalog that briefly comes
  // back empty. Losing it used to blank the dropdown and disable the composer.
  const [selectedModelKey, setSelectedModelKey] = useState<string>(() => {
    try {
      return localStorage.getItem('woolwire_selected_model') || ''
    } catch {
      return ''
    }
  })
  const [inputContent, setInputContent] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [streamingText, setStreamingText] = useState('')
  const [streamingRequestId, setStreamingRequestId] = useState('')
  const [streamingHostId, setStreamingHostId] = useState('')
  const [errorMsg, setErrorMsg] = useState('')
  const [msgStats, setMsgStats] = useState<Record<string, GenerationStats>>(() => {
    try {
      return JSON.parse(sessionStorage.getItem('woolwire_msg_stats') || '{}')
    } catch {
      return {}
    }
  })
  const [liveStats, setLiveStats] = useState<{ ttft_ms?: number; tps?: number; tokens?: number }>({})
  const [newTitle, setNewTitle] = useState('')
  const [newNoSave, setNewNoSave] = useState(false)
  const [showNewModal, setShowNewModal] = useState(false)
  // Which message is open for editing, and which was just copied.
  const [editingId, setEditingId] = useState<string | null>(null)
  const [editDraft, setEditDraft] = useState('')
  const [copiedMsgId, setCopiedMsgId] = useState<string | null>(null)
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
        // Only fill an empty selection. A catalog refresh that momentarily
        // returns nothing (or drops the chosen host for one poll) must not
        // reset what the user picked -- that is what silently deselected the
        // model and disabled the composer on returning to this tab.
        if (data && data.length > 0) {
          setSelectedModelKey(current => current || (initialModel
            ? `${initialModel.host_member_id}/${initialModel.model_id}`
            : `${data[0].host_member_id}/${data[0].model_id}`))
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
    let stopped = false
    let timer: number | undefined
    const refresh = async () => {
      await fetchCatalog()
      if (!stopped) timer = window.setTimeout(refresh, 15000)
    }
    void refresh()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [])

  useEffect(() => {
    if (activeConvId) {
      fetchChatMessages(activeConvId)
    } else {
      setMessages([])
    }
  }, [activeConvId])

  // Remember the selection across reloads and tab switches.
  useEffect(() => {
    try {
      if (selectedModelKey) localStorage.setItem('woolwire_selected_model', selectedModelKey)
    } catch {}
  }, [selectedModelKey])

  // A model picked from The Meadow applies even though this component is now
  // kept mounted, where before it only took effect on a fresh mount.
  useEffect(() => {
    if (initialModel?.host_member_id && initialModel?.model_id) {
      setSelectedModelKey(`${initialModel.host_member_id}/${initialModel.model_id}`)
    }
  }, [initialModel])

  useEffect(() => {
    // Scrolling a hidden pane fights the visible one for the viewport.
    if (!visible) return
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages, streamingText, visible])

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

  // runGeneration owns one streaming exchange with the backend. Sending,
  // regenerating, and editing differ only in the endpoint they call and the
  // optimistic message they show, so they all funnel through here.
  const runGeneration = async (
    url: string,
    payload: Record<string, unknown>,
    opts: {
      hostId: string
      modelId: string
      optimistic?: Message
      /** Replaces the transcript before streaming starts. Editing or
          regenerating moves to a new branch, so the turns being superseded
          have to leave the screen immediately -- otherwise the old thread
          stays put and the new answer looks appended to it. */
      baseMessages?: Message[]
    } = { hostId: '', modelId: '' }
  ) => {
    const { hostId, modelId, optimistic, baseMessages } = opts
    // Captured for rollback: a failed request must not leave the transcript
    // truncated to a branch that was never created.
    const snapshot = messages
    setErrorMsg('')
    setStreaming(true)
    setStreamingText('')
    setStreamingHostId(hostId)
    setLiveStats({})

    const startTime = performance.now()
    let firstTokenTime: number | null = null
    let tokenCount = 0
    let lastRequestId = ''
    let serverStats: GenerationStats | null = null

    const activeConv = conversations.find((c) => c.ID === activeConvId)
    const isNoSave = activeConv?.NoSave || false

    if (baseMessages) {
      setMessages(optimistic ? [...baseMessages, optimistic] : baseMessages)
    } else if (optimistic) {
      setMessages((prev) => [...prev, optimistic])
    }

    try {
      const res = await api(url, { method: 'POST', body: JSON.stringify(payload) })

      if (!res.ok) {
        const errText = await res.text()
        throw new Error(errText || `Inference error: ${res.status}`)
      }

      const reader = res.body?.getReader()
      let accumulated = ''

      if (reader) {
        await readSSEStream(reader, ({ event, data }) => {
          if (event === 'error') {
            let msg = 'Host error or queue interrupted'
            if (data) {
              try {
                const parsed = JSON.parse(data)
                msg = parsed.error || parsed.message || data
              } catch {
                msg = data
              }
            }
            setErrorMsg(msg)
            return
          }

          if (event === 'stats') {
            try {
              serverStats = JSON.parse(data)
            } catch {}
            return
          }

          if (data === '[DONE]') {
            return
          }

          try {
            const parsed = JSON.parse(data)
            if (parsed.delta) {
              accumulated += parsed.delta
              if (!firstTokenTime) {
                firstTokenTime = performance.now()
                setLiveStats((prev) => ({ ...prev, ttft_ms: Math.round(firstTokenTime! - startTime) }))
              }
              tokenCount++
              const now = performance.now()
              const elapsedGen = (now - firstTokenTime) / 1000
              const tps = elapsedGen > 0 ? Number((tokenCount / elapsedGen).toFixed(1)) : 0
              setLiveStats({
                ttft_ms: Math.round(firstTokenTime - startTime),
                tokens: tokenCount,
                tps: tps,
              })
              setStreamingText(accumulated)
            }
            if (parsed.request_id) {
              lastRequestId = parsed.request_id
              setStreamingRequestId(parsed.request_id)
            }
          } catch {
            // ignore unparseable frame
          }
        })
      }

      setStreaming(false)
      setStreamingText('')
      setStreamingRequestId('')
      setLiveStats({})

      const endTime = performance.now()
      const measuredTtft = firstTokenTime ? Math.round(firstTokenTime - startTime) : 0
      const measuredTotal = Math.round(endTime - startTime)
      const measuredGenDur = firstTokenTime ? (endTime - firstTokenTime) / 1000 : 0
      const measuredTps = measuredGenDur > 0 ? Number((tokenCount / measuredGenDur).toFixed(1)) : 0

      const sStats = serverStats as GenerationStats | null
      const finalStats: GenerationStats = {
        request_id: sStats?.request_id || lastRequestId,
        ttft_ms: sStats?.ttft_ms ?? measuredTtft,
        total_ms: sStats?.total_ms ?? measuredTotal,
        completion_tokens: sStats?.completion_tokens ?? tokenCount,
        tokens_per_second: sStats?.tokens_per_second
          ? Number(sStats.tokens_per_second.toFixed(1))
          : measuredTps,
        prompt_tokens_est: sStats?.prompt_tokens_est,
      }

      // Reload messages from store if not in no_save mode
      if (!isNoSave) {
        try {
          const res = await api(`/api/v1/chats/${activeConvId}`)
          if (res.ok) {
            const data = await res.json()
            const freshMsgs: Message[] = data.messages || []
            setMessages(freshMsgs)
            if (freshMsgs.length > 0) {
              const lastAsst = freshMsgs[freshMsgs.length - 1]
              if (lastAsst && lastAsst.Role === 'assistant') {
                setMsgStats((prev) => {
                  const next = { ...prev, [lastAsst.ID]: finalStats }
                  try {
                    sessionStorage.setItem('woolwire_msg_stats', JSON.stringify(next))
                  } catch {}
                  return next
                })
              }
            }
          }
        } catch {}
      } else {
        // Keep in memory for active view
        const asstId = `temp-asst-${Date.now()}`
        const asstMsg: Message = {
          ID: asstId,
          ConversationID: activeConvId!,
          Role: 'assistant',
          Content: accumulated,
          HostMemberID: hostId,
          ModelID: modelId,
          CreatedAt: Math.floor(Date.now() / 1000),
        }
        setMessages((prev) => [...prev, asstMsg])
        setMsgStats((prev) => {
          const next = { ...prev, [asstId]: finalStats }
          try {
            sessionStorage.setItem('woolwire_msg_stats', JSON.stringify(next))
          } catch {}
          return next
        })
      }
    } catch (err: any) {
      setErrorMsg(err.message || 'Error communicating with model host')
      setStreaming(false)
      setLiveStats({})
      // Put back what the turn previewed: the branch it would have created
      // does not exist, so the superseded messages belong back on screen.
      if (baseMessages) {
        setMessages(snapshot)
      } else if (optimistic) {
        setMessages((prev) => prev.filter((m) => m.ID !== optimistic.ID))
      }
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

    await runGeneration(
      `/api/v1/chats/${activeConvId}/message`,
      { content: currentContent, host_member_id: hostId, model_id: modelId },
      {
        hostId,
        modelId,
        optimistic: {
          ID: 'temp-user',
          ConversationID: activeConvId,
          Role: 'user',
          Content: currentContent,
          HostMemberID: hostId,
          ModelID: modelId,
          CreatedAt: Math.floor(Date.now() / 1000),
        },
      }
    )
  }

  // Ask for another answer to the same question. The previous one is kept and
  // reachable through the variant arrows.
  const handleRegenerate = async (m: Message) => {
    if (!activeConvId || streaming) return
    const [selHost, selModel] = selectedModelKey.split('/')
    const hostId = selHost || m.HostMemberID
    const modelId = selModel || m.ModelID
    // The answer being replaced, and anything that followed it, leave the
    // screen while the new one streams into their place.
    const idx = messages.findIndex((x) => x.ID === m.ID)
    await runGeneration(
      `/api/v1/chats/${activeConvId}/messages/${m.ID}/regenerate`,
      { host_member_id: hostId, model_id: modelId },
      { hostId, modelId, baseMessages: idx >= 0 ? messages.slice(0, idx) : undefined }
    )
  }

  // Re-ask a question with different wording. The original wording and the
  // exchange that followed it stay on their own branch.
  const handleSubmitEdit = async (m: Message, content: string) => {
    if (!activeConvId || streaming) return
    const trimmed = content.trim()
    if (!trimmed || trimmed === m.Content) {
      setEditingId(null)
      return
    }
    const [selHost, selModel] = selectedModelKey.split('/')
    const hostId = selHost || m.HostMemberID
    const modelId = selModel || m.ModelID
    setEditingId(null)
    // Switch to the new branch straight away: the original question and
    // everything it led to are replaced by the edited question, which then
    // streams its own answer.
    const idx = messages.findIndex((x) => x.ID === m.ID)
    await runGeneration(
      `/api/v1/chats/${activeConvId}/messages/${m.ID}/edit`,
      { content: trimmed, host_member_id: hostId, model_id: modelId },
      {
        hostId,
        modelId,
        baseMessages: idx >= 0 ? messages.slice(0, idx) : undefined,
        optimistic: {
          ID: 'temp-user',
          ConversationID: activeConvId,
          Role: 'user',
          Content: trimmed,
          HostMemberID: hostId,
          ModelID: modelId,
          CreatedAt: Math.floor(Date.now() / 1000),
        },
      }
    )
  }

  // Page between takes. Switching brings back everything that followed that
  // take, so each branch keeps its own continuation.
  const handleSelectVariant = async (m: Message, direction: -1 | 1) => {
    if (!activeConvId || streaming) return
    const siblings = m.SiblingIDs || []
    const current = (m.VariantIndex || 1) - 1
    const target = siblings[current + direction]
    if (!target) return
    try {
      const res = await api(`/api/v1/chats/${activeConvId}/messages/${target}/select`, { method: 'POST' })
      if (!res.ok) throw new Error('Could not switch to that version')
      await fetchChatMessages(activeConvId)
    } catch (err: any) {
      setErrorMsg(err.message || 'Could not switch to that version')
    }
  }

  const handleCopyMessage = async (m: Message) => {
    try {
      await navigator.clipboard.writeText(m.Content)
      setCopiedMsgId(m.ID)
      setTimeout(() => setCopiedMsgId((id) => (id === m.ID ? null : id)), 2000)
    } catch {
      setErrorMsg('Clipboard unavailable in this browser context.')
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

  const approxTokens = React.useMemo(() => {
    let chars = 0
    for (const m of messages) {
      chars += (m.Content || '').length + 8
    }
    return Math.round(chars / 4)
  }, [messages])
  const contextLimit = selectedModel?.context_limit || 4096
  const contextPct = Math.round(Math.min(100, (approxTokens / contextLimit) * 100))

  return (
    <div className="conversation-layout">
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
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', paddingBottom: '0.75rem', borderBottom: '1px solid var(--border-color)', marginBottom: '1rem', flexWrap: 'wrap', gap: '0.5rem' }}>
              <div>
                <h3 style={{ fontSize: '1.1rem' }}>{activeConv.Title}</h3>
                {activeConv.NoSave && (
                  <span style={{ fontSize: '0.75rem', color: 'var(--accent-danger)' }}>
                    Privacy mode: prompts are not saved locally or remotely
                  </span>
                )}
              </div>

              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
                {selectedModel && (
                  <div
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: '0.4rem',
                      backgroundColor: 'var(--bg-primary)',
                      padding: '0.3rem 0.6rem',
                      borderRadius: 'var(--radius)',
                      border: '1px solid var(--border-color)',
                      fontSize: '0.75rem',
                    }}
                    title={`Current conversation estimate: ${approxTokens.toLocaleString()} tokens / ${selectedModel.context_limit.toLocaleString()} max context limit`}
                  >
                    <span style={{ color: 'var(--text-secondary)' }}>Context:</span>
                    <strong>{approxTokens.toLocaleString()} / {selectedModel.context_limit.toLocaleString()}</strong>
                    <div style={{ width: '48px', height: '6px', backgroundColor: 'var(--bg-secondary)', borderRadius: '3px', overflow: 'hidden' }}>
                      <div
                        style={{
                          height: '100%',
                          width: `${Math.min(100, contextPct)}%`,
                          backgroundColor: contextPct > 80 ? 'var(--accent-danger)' : contextPct > 60 ? '#f59e0b' : 'var(--accent-success)',
                          transition: 'width 0.3s ease',
                        }}
                      />
                    </div>
                    <span style={{ color: contextPct > 80 ? 'var(--accent-danger)' : 'var(--text-secondary)' }}>{contextPct}%</span>
                  </div>
                )}

                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                  <label style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>Model:</label>
                  <select
                    className="form-control"
                    style={{ width: 'auto', padding: '0.35rem 0.75rem', fontSize: '0.85rem' }}
                    value={selectedModelKey}
                    onChange={(e) => setSelectedModelKey(e.target.value)}
                    disabled={streaming}
                  >
                    {models.length === 0 && !selectedModelKey && (
                      <option value="">No models available</option>
                    )}
                    {/* The chosen model is kept in the list even while the
                        catalog cannot see it, so a host that blinks out shows
                        as unavailable instead of silently deselecting. */}
                    {selectedModelKey && !selectedModel && (
                      <option value={selectedModelKey}>
                        {modelNameMap[selectedModelKey.split('/')[1]] || selectedModelKey.split('/')[1]} (unavailable)
                      </option>
                    )}
                    {models.map((m) => (
                      <option key={`${m.host_member_id}/${m.model_id}`} value={`${m.host_member_id}/${m.model_id}`}>
                        {m.name} ({m.host_display_name})
                      </option>
                    ))}
                  </select>
                </div>
              </div>
            </div>

            {errorMsg && <div className="alert alert-error">{errorMsg}</div>}

            {/* Messages Feed */}
            <div className="message-feed" style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: '1rem', paddingRight: '0.5rem', marginBottom: '1rem', minHeight: 0 }}>
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
                    maxWidth: '85%',
                    backgroundColor: m.Role === 'user' ? 'var(--accent-primary)' : 'var(--bg-primary)',
                    color: 'white',
                    padding: '0.75rem 1rem',
                    borderRadius: 'var(--radius)',
                    border: m.Role === 'assistant' ? '1px solid var(--border-color)' : 'none',
                  }}
                >
                  {m.Role === 'assistant' && (
                    <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginBottom: '0.4rem', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                      <span>
                        Hosted by <strong>{models.find((mod) => mod.host_member_id === m.HostMemberID)?.host_display_name || 'Peer'}</strong> ({modelNameMap[m.ModelID] || models.find((mod) => mod.model_id === m.ModelID)?.name || m.ModelID})
                      </span>
                    </div>
                  )}

                  {m.Role === 'user' && editingId === m.ID ? (
                    <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                      <textarea
                        className="form-control"
                        style={{ fontSize: '0.95rem', minHeight: '4.5rem', resize: 'vertical' }}
                        value={editDraft}
                        autoFocus
                        onChange={(e) => setEditDraft(e.target.value)}
                        onKeyDown={(e) => {
                          if (e.key === 'Escape') setEditingId(null)
                          if (e.key === 'Enter' && !e.shiftKey) {
                            e.preventDefault()
                            void handleSubmitEdit(m, editDraft)
                          }
                        }}
                      />
                      <div style={{ display: 'flex', gap: '0.5rem', justifyContent: 'flex-end' }}>
                        <button type="button" className="btn btn-secondary" style={msgActionStyle} onClick={() => setEditingId(null)}>
                          Cancel
                        </button>
                        <button
                          type="button"
                          className="btn btn-primary"
                          style={msgActionStyle}
                          disabled={!editDraft.trim() || streaming}
                          onClick={() => void handleSubmitEdit(m, editDraft)}
                        >
                          Save &amp; resend
                        </button>
                      </div>
                    </div>
                  ) : m.Role === 'user' ? (
                    <div style={{ whiteSpace: 'pre-wrap', fontSize: '0.95rem' }}>{m.Content}</div>
                  ) : (
                    <MessageContent content={m.Content} />
                  )}

                  {/* Per-message actions. Variant arrows appear only where
                      there is more than one take to page between. */}
                  {editingId !== m.ID && !m.ID.startsWith('temp-') && (
                    <div
                      style={{
                        marginTop: '0.5rem',
                        display: 'flex',
                        alignItems: 'center',
                        gap: '0.35rem',
                        flexWrap: 'wrap',
                        justifyContent: m.Role === 'user' ? 'flex-end' : 'flex-start',
                      }}
                    >
                      {(m.VariantCount || 1) > 1 && (
                        <span style={{ display: 'inline-flex', alignItems: 'center', gap: '0.15rem', fontSize: '0.72rem', color: 'var(--text-secondary)' }}>
                          <button
                            type="button"
                            style={msgActionStyle}
                            aria-label="Previous version"
                            disabled={streaming || (m.VariantIndex || 1) <= 1}
                            onClick={() => void handleSelectVariant(m, -1)}
                          >
                            ‹
                          </button>
                          <span aria-live="polite">{m.VariantIndex} / {m.VariantCount}</span>
                          <button
                            type="button"
                            style={msgActionStyle}
                            aria-label="Next version"
                            disabled={streaming || (m.VariantIndex || 1) >= (m.VariantCount || 1)}
                            onClick={() => void handleSelectVariant(m, 1)}
                          >
                            ›
                          </button>
                        </span>
                      )}
                      <button type="button" style={msgActionStyle} onClick={() => void handleCopyMessage(m)}>
                        {copiedMsgId === m.ID ? '✓ Copied' : 'Copy'}
                      </button>
                      {m.Role === 'user' && !activeConv?.NoSave && (
                        <button
                          type="button"
                          style={msgActionStyle}
                          disabled={streaming}
                          onClick={() => { setEditingId(m.ID); setEditDraft(m.Content) }}
                        >
                          Edit
                        </button>
                      )}
                      {m.Role === 'assistant' && !activeConv?.NoSave && (
                        <button
                          type="button"
                          style={msgActionStyle}
                          disabled={streaming}
                          title="Generate another answer, keeping this one"
                          onClick={() => void handleRegenerate(m)}
                        >
                          Regenerate
                        </button>
                      )}
                    </div>
                  )}

                  {/* Generation Stats HUD */}
                  {m.Role === 'assistant' && msgStats[m.ID] && (
                    <div
                      style={{
                        marginTop: '0.6rem',
                        paddingTop: '0.4rem',
                        borderTop: '1px solid rgba(255, 255, 255, 0.1)',
                        display: 'flex',
                        flexWrap: 'wrap',
                        gap: '0.75rem',
                        fontSize: '0.72rem',
                        color: 'var(--text-secondary)',
                        alignItems: 'center',
                      }}
                    >
                      {msgStats[m.ID].tokens_per_second !== undefined && msgStats[m.ID].tokens_per_second! > 0 && (
                        <span>⚡ <strong>{msgStats[m.ID].tokens_per_second}</strong> tok/s</span>
                      )}
                      {msgStats[m.ID].ttft_ms !== undefined && msgStats[m.ID].ttft_ms! > 0 && (
                        <span>⏱️ TTFT: <strong>{msgStats[m.ID].ttft_ms}</strong>ms</span>
                      )}
                      {msgStats[m.ID].completion_tokens !== undefined && msgStats[m.ID].completion_tokens! > 0 && (
                        <span>🔢 <strong>{msgStats[m.ID].completion_tokens}</strong> tokens</span>
                      )}
                      {msgStats[m.ID].total_ms !== undefined && msgStats[m.ID].total_ms! > 0 && (
                        <span>⏳ <strong>{(msgStats[m.ID].total_ms! / 1000).toFixed(2)}</strong>s</span>
                      )}
                    </div>
                  )}
                </div>
              ))}

              {streaming && (
                <div
                  style={{
                    alignSelf: 'flex-start',
                    maxWidth: '85%',
                    backgroundColor: 'var(--bg-primary)',
                    color: 'white',
                    padding: '0.75rem 1rem',
                    borderRadius: 'var(--radius)',
                    border: '1px solid var(--border-color)',
                  }}
                >
                  <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)', marginBottom: '0.4rem', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <span>Generating response...</span>
                    {liveStats.tps ? (
                      <span style={{ color: 'var(--accent-text)', fontWeight: 600 }}>
                        ⚡ {liveStats.tps} tok/s {liveStats.ttft_ms ? `• TTFT ${liveStats.ttft_ms}ms` : ''}
                      </span>
                    ) : null}
                  </div>
                  <MessageContent content={streamingText} isStreaming={true} />
                  {(!streamingText.includes('<think>') || (streamingText.includes('</think>') && streamingText.indexOf('</think>') + 8 < streamingText.length)) && (
                    <span className="cursor-blink">|</span>
                  )}
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
                // Only an in-flight generation blocks typing. A missing model
                // is a reason the message cannot be sent yet, not a reason to
                // stop someone composing it.
                disabled={streaming}
              />
              {streaming ? (
                <button type="button" className="btn btn-secondary" onClick={handleCancel}>
                  Cancel
                </button>
              ) : (
                <button
                  type="submit"
                  className="btn btn-primary"
                  disabled={!inputContent.trim() || !selectedModelKey}
                  title={!selectedModelKey ? 'Choose a model first' : undefined}
                >
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
          <div className="card" style={{ width: '400px', maxWidth: 'calc(100vw - 2rem)', maxHeight: '90dvh', overflowY: 'auto' }}>
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
