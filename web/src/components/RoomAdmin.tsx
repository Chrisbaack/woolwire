import React, { useState, useEffect } from 'react'
import QRCode from 'qrcode'

interface Member {
  member_id?: string
  MemberID?: string
  display_name?: string
  DisplayName?: string
  device_public?: string
  DevicePublic?: string
  status?: string
  Status?: string
}

interface RoomAdminProps {
  invitationCode: string
  onRotateInvitation: () => Promise<string>
  onRemoveMember: (id: string, rotate: boolean) => Promise<void>
}

export const RoomAdmin: React.FC<RoomAdminProps> = ({
  invitationCode,
  onRotateInvitation,
  onRemoveMember,
}) => {
  const [activeCode, setActiveCode] = useState(invitationCode)
  const [members, setMembers] = useState<Member[]>([])
  const [copied, setCopied] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [showQr, setShowQr] = useState(false)
  const [qrUrl, setQrUrl] = useState('')

  useEffect(() => {
    setActiveCode(invitationCode)
  }, [invitationCode])

  useEffect(() => {
    if (activeCode) {
      QRCode.toDataURL(activeCode, {
        width: 260,
        margin: 2,
        color: { dark: '#000000', light: '#ffffff' },
      })
        .then(setQrUrl)
        .catch(() => setQrUrl(''))
    }
  }, [activeCode])

  const fetchMembers = async () => {
    try {
      const res = await fetch('/api/v1/room-admin/members')
      if (res.ok) {
        const data = await res.json()
        setMembers(data || [])
      }
    } catch {
      // ignore
    }
  }

  useEffect(() => {
    fetchMembers()
    const interval = setInterval(fetchMembers, 5000)
    return () => clearInterval(interval)
  }, [])

  const handleCopy = () => {
    navigator.clipboard.writeText(activeCode)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const handleDownloadFile = () => {
    const blob = new Blob([activeCode], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'woolwire-invite.txt'
    a.click()
    URL.revokeObjectURL(url)
  }

  const handleRotate = async () => {
    if (!confirm('Rotate invitation code? Existing members will stay connected, but the old code cannot be used to join.')) {
      return
    }
    try {
      setLoading(true)
      setError('')
      const newCode = await onRotateInvitation()
      setActiveCode(newCode)
    } catch (err: any) {
      setError(err.message || 'Failed to rotate invitation')
    } finally {
      setLoading(false)
    }
  }

  const handleRemove = async (id: string, name: string) => {
    if (!confirm(`Remove member "${name}" from the room? By default, the invitation code will also be rotated.`)) {
      return
    }
    try {
      setLoading(true)
      setError('')
      await onRemoveMember(id, true)
      await fetchMembers()
    } catch (err: any) {
      setError(err.message || 'Failed to remove member')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="card">
      <h2>Room Administration (Creator)</h2>

      {error && <div className="alert alert-error">{error}</div>}

      <div className="form-group">
        <label>Active Invitation Code</label>
        <div className="code-box">{activeCode}</div>
        <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
          <button className="btn btn-secondary" onClick={handleCopy}>
            {copied ? 'Copied!' : 'Copy Code'}
          </button>
          {qrUrl && (
            <button className="btn btn-secondary" onClick={() => setShowQr(!showQr)}>
              {showQr ? 'Hide QR Code' : 'Scan QR Code'}
            </button>
          )}
          <button className="btn btn-secondary" onClick={handleDownloadFile}>
            Save File
          </button>
          <button className="btn btn-danger" onClick={handleRotate} disabled={loading}>
            {loading ? 'Rotating...' : 'Rotate Invitation'}
          </button>
        </div>

        {showQr && qrUrl && (
          <div style={{ marginTop: '1rem', padding: '1rem', background: '#ffffff', borderRadius: '8px', display: 'inline-block', textAlign: 'center' }}>
            <img src={qrUrl} alt="Invitation QR Code" style={{ display: 'block', maxWidth: '240px', margin: '0 auto' }} />
            <div style={{ color: '#333333', fontSize: '0.8rem', marginTop: '0.5rem' }}>
              Scan from a phone camera or another device to join instantly
            </div>
          </div>
        )}
      </div>

      <h3 style={{ fontSize: '1rem', marginTop: '1.5rem', marginBottom: '0.75rem' }}>Manage Members</h3>
      <div className="member-list">
        {members.length === 0 ? (
          <div style={{ color: 'var(--text-secondary)', fontSize: '0.85rem' }}>
            No other members have joined this room yet. Share your invitation code above to invite friends.
          </div>
        ) : (
          members.map((m) => {
            const memberId = m.member_id || m.MemberID || ''
            const displayName = m.display_name || m.DisplayName || 'Unknown Member'
            const devicePublic = m.device_public || m.DevicePublic || ''
            const status = m.status || m.Status || 'unknown'
            const shortId = devicePublic ? `${devicePublic.slice(0, 12)}...` : (memberId ? `${memberId.slice(0, 12)}...` : 'N/A')

            return (
              <div key={memberId || devicePublic || Math.random().toString()} className="member-item">
                <div className="member-info">
                  <span
                    className="member-status"
                    style={{
                      backgroundColor: status === 'admitted' ? 'var(--accent-success)' : 'var(--accent-danger)'
                    }}
                  />
                  <div>
                    <strong>{displayName}</strong>
                    <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                      ID: {shortId}
                    </div>
                  </div>
                </div>
                {status === 'admitted' && memberId && (
                  <button
                    className="btn btn-danger"
                    style={{ padding: '0.4rem 0.8rem', fontSize: '0.85rem' }}
                    onClick={() => handleRemove(memberId, displayName)}
                    disabled={loading}
                  >
                    Remove
                  </button>
                )}
              </div>
            )
          })
        )}
      </div>
    </div>
  )
}
