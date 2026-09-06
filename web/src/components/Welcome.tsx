import React, { useState, useEffect, useRef } from 'react'
import QRCode from 'qrcode'
import jsQR from 'jsqr'

interface WelcomeProps {
  displayName: string
  onUpdateProfile: (name: string) => Promise<void>
  onHostRoom: (name: string) => Promise<string>
  onJoinRoom: (code: string) => Promise<void>
}

export const Welcome: React.FC<WelcomeProps> = ({
  displayName,
  onUpdateProfile,
  onHostRoom,
  onJoinRoom,
}) => {
  const [activeTab, setActiveTab] = useState<'host' | 'join'>('host')
  const [name, setName] = useState(displayName)
  const [roomName, setRoomName] = useState('')
  const [joinCode, setJoinCode] = useState('')
  const [hostedCode, setHostedCode] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [copied, setCopied] = useState(false)
  const [showQr, setShowQr] = useState(false)
  const [qrUrl, setQrUrl] = useState('')
  const [scanning, setScanning] = useState(false)
  const [scannerError, setScannerError] = useState('')

  const videoRef = useRef<HTMLVideoElement | null>(null)
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  const animFrameRef = useRef<number | null>(null)
  const streamRef = useRef<MediaStream | null>(null)
  const cameraInputRef = useRef<HTMLInputElement | null>(null)

  const stopScanner = () => {
    if (animFrameRef.current) {
      cancelAnimationFrame(animFrameRef.current)
      animFrameRef.current = null
    }
    if (streamRef.current) {
      streamRef.current.getTracks().forEach((track) => track.stop())
      streamRef.current = null
    }
    setScanning(false)
  }

  const tickScan = () => {
    if (videoRef.current && videoRef.current.readyState === videoRef.current.HAVE_ENOUGH_DATA) {
      const canvas = canvasRef.current || document.createElement('canvas')
      canvas.width = videoRef.current.videoWidth
      canvas.height = videoRef.current.videoHeight
      const ctx = canvas.getContext('2d')
      if (ctx) {
        ctx.drawImage(videoRef.current, 0, 0, canvas.width, canvas.height)
        const imageData = ctx.getImageData(0, 0, canvas.width, canvas.height)
        const code = jsQR(imageData.data, imageData.width, imageData.height, {
          inversionAttempts: 'attemptBoth',
        })
        if (code && code.data) {
          setJoinCode(code.data.trim())
          stopScanner()
          return
        }
      }
    }
    animFrameRef.current = requestAnimationFrame(tickScan)
  }

  const startScanner = async () => {
    setScannerError('')
    setScanning(true)
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: 'environment' },
      })
      streamRef.current = stream
      if (videoRef.current) {
        videoRef.current.srcObject = stream
        videoRef.current.setAttribute('playsinline', 'true')
        await videoRef.current.play()
        animFrameRef.current = requestAnimationFrame(tickScan)
      }
    } catch (err: any) {
      setScannerError(err.message || 'Camera permission denied or camera not accessible')
    }
  }

  const handleScanClick = () => {
    if (scanning) {
      stopScanner()
      return
    }
    setScannerError('')
    if (typeof navigator !== 'undefined' && navigator.mediaDevices && typeof navigator.mediaDevices.getUserMedia === 'function') {
      startScanner()
    } else {
      // Plain HTTP / iOS WebKit doesn't expose getUserMedia: trigger native camera snapshot directly
      cameraInputRef.current?.click()
    }
  }

  const handleImageCapture = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    setScannerError('')
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
          setScannerError('Could not process captured image.')
          return
        }
        ctx.drawImage(img, 0, 0, width, height)
        const imageData = ctx.getImageData(0, 0, width, height)
        const code = jsQR(imageData.data, imageData.width, imageData.height, {
          inversionAttempts: 'attemptBoth',
        })
        if (code && code.data) {
          setJoinCode(code.data.trim())
          stopScanner()
          setScannerError('')
        } else {
          setScannerError('Could not detect a QR code in the image. Please try taking a closer photo or paste the code.')
        }
      }
      img.onerror = () => {
        setScannerError('Failed to load captured photo.')
      }
      img.src = event.target?.result as string
    }
    reader.readAsDataURL(file)
    e.target.value = ''
  }

  useEffect(() => {
    return () => {
      stopScanner()
    }
  }, [])

  useEffect(() => {
    if (hostedCode) {
      QRCode.toDataURL(hostedCode, {
        width: 260,
        margin: 2,
        color: { dark: '#000000', light: '#ffffff' },
      })
        .then(setQrUrl)
        .catch(() => setQrUrl(''))
    }
  }, [hostedCode])

  const handleProfileSave = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!name.trim()) return
    try {
      setLoading(true)
      setError('')
      await onUpdateProfile(name.trim())
    } catch (err: any) {
      setError(err.message || 'Failed to update profile')
    } finally {
      setLoading(false)
    }
  }

  const handleHost = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!roomName.trim()) return
    try {
      setLoading(true)
      setError('')
      const code = await onHostRoom(roomName.trim())
      setHostedCode(code)
    } catch (err: any) {
      setError(err.message || 'Failed to host room')
    } finally {
      setLoading(false)
    }
  }

  const handleJoin = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!joinCode.trim()) return
    try {
      setLoading(true)
      setError('')
      await onJoinRoom(joinCode.trim())
    } catch (err: any) {
      setError(err.message || 'Failed to join room')
    } finally {
      setLoading(false)
    }
  }

  const copyCode = () => {
    navigator.clipboard.writeText(hostedCode)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const handleDownloadFile = () => {
    const blob = new Blob([hostedCode], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'woolwire-invite.txt'
    a.click()
    URL.revokeObjectURL(url)
  }

  const handleFileUpload = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = (event) => {
      const text = event.target?.result as string
      if (text) {
        setJoinCode(text.trim())
      }
    }
    reader.readAsText(file)
  }

  return (
    <div className="card">
      <h2>Welcome to Woolwire</h2>
      <p style={{ color: 'var(--text-secondary)', marginBottom: '1.5rem' }}>
        A private room for shared intelligence. Connect with your friends to share local language models.
      </p>

      {error && <div className="alert alert-error">{error}</div>}

      {!displayName ? (
        <form onSubmit={handleProfileSave}>
          <div className="form-group">
            <label htmlFor="display-name">Choose your display name</label>
            <input
              id="display-name"
              type="text"
              className="form-control"
              placeholder="e.g. Alice"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>
          <button type="submit" className="btn btn-primary" disabled={loading}>
            {loading ? 'Saving...' : 'Continue'}
          </button>
        </form>
      ) : (
        <div>
          <div className="tabs">
            <button
              className={`tab ${activeTab === 'host' ? 'active' : ''}`}
              onClick={() => { setActiveTab('host'); setError('') }}
            >
              Host a Room
            </button>
            <button
              className={`tab ${activeTab === 'join' ? 'active' : ''}`}
              onClick={() => { setActiveTab('join'); setError('') }}
            >
              Join a Room
            </button>
          </div>

          {activeTab === 'host' && (
            <div>
              {hostedCode ? (
                <div>
                  <div className="alert alert-success">Room created successfully! Share this code with friends:</div>
                  <div className="code-box">{hostedCode}</div>
                  <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', marginBottom: '1rem' }}>
                    <button className="btn btn-primary" onClick={copyCode}>
                      {copied ? 'Copied!' : 'Copy Code'}
                    </button>
                    {qrUrl && (
                      <button className="btn btn-secondary" onClick={() => setShowQr(!showQr)}>
                        {showQr ? 'Hide QR Code' : 'Show QR Code'}
                      </button>
                    )}
                    <button className="btn btn-secondary" onClick={handleDownloadFile}>
                      Save File
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
              ) : (
                <form onSubmit={handleHost}>
                  <div className="form-group">
                    <label htmlFor="room-name">Room Name</label>
                    <input
                      id="room-name"
                      type="text"
                      className="form-control"
                      placeholder="e.g. Technical Friends"
                      value={roomName}
                      onChange={(e) => setRoomName(e.target.value)}
                      required
                    />
                  </div>
                  <button type="submit" className="btn btn-primary" disabled={loading}>
                    {loading ? 'Creating...' : 'Create & Host Room'}
                  </button>
                </form>
              )}
            </div>
          )}

          {activeTab === 'join' && (
            <form onSubmit={handleJoin}>
              <div className="form-group">
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem', flexWrap: 'wrap', gap: '0.5rem' }}>
                  <label htmlFor="join-code" style={{ marginBottom: 0 }}>Join Code</label>
                  <div style={{ display: 'flex', gap: '0.4rem', alignItems: 'center' }}>
                    <button
                      type="button"
                      className="btn btn-secondary"
                      style={{ padding: '0.2rem 0.6rem', fontSize: '0.8rem' }}
                      onClick={handleScanClick}
                    >
                      {scanning ? 'Stop Camera' : '📷 Scan QR Code'}
                    </button>
                    <input
                      ref={cameraInputRef}
                      type="file"
                      accept="image/*"
                      capture="environment"
                      style={{ display: 'none' }}
                      onChange={handleImageCapture}
                    />
                    <label className="btn btn-secondary" style={{ padding: '0.2rem 0.6rem', fontSize: '0.8rem', cursor: 'pointer' }}>
                      Import File
                      <input
                        type="file"
                        accept=".txt,.woolwire"
                        style={{ display: 'none' }}
                        onChange={handleFileUpload}
                      />
                    </label>
                  </div>
                </div>

                {scannerError && !scanning && (
                  <div className="alert alert-error" style={{ marginBottom: '0.75rem', fontSize: '0.85rem' }}>
                    {scannerError}
                  </div>
                )}

                {scanning && (
                  <div style={{ marginBottom: '1rem', padding: '1rem', backgroundColor: 'var(--bg-primary)', borderRadius: 'var(--radius)', border: '1px solid var(--accent-primary)', textAlign: 'center' }}>
                    <div style={{ position: 'relative', width: '100%', maxWidth: '320px', margin: '0 auto', overflow: 'hidden', borderRadius: '8px', background: '#000' }}>
                      <video
                        ref={videoRef}
                        style={{ width: '100%', height: 'auto', display: 'block' }}
                        playsInline
                        muted
                      />
                      <div style={{ position: 'absolute', top: '15%', left: '15%', right: '15%', bottom: '15%', border: '2px dashed var(--accent-primary)', pointerEvents: 'none', borderRadius: '8px' }} />
                    </div>
                    <p style={{ fontSize: '0.8rem', color: 'var(--text-secondary)', marginTop: '0.5rem' }}>
                      Point your camera at a Woolwire invitation QR code
                    </p>
                    <div style={{ display: 'flex', gap: '0.5rem', justifyContent: 'center', marginTop: '0.5rem', flexWrap: 'wrap' }}>
                      <button
                        type="button"
                        className="btn btn-secondary"
                        style={{ padding: '0.2rem 0.6rem', fontSize: '0.8rem' }}
                        onClick={() => cameraInputRef.current?.click()}
                      >
                        📸 Take Photo / Pick Image
                      </button>
                      <button
                        type="button"
                        className="btn btn-secondary"
                        style={{ padding: '0.2rem 0.6rem', fontSize: '0.8rem' }}
                        onClick={stopScanner}
                      >
                        Close
                      </button>
                    </div>
                    {scannerError && <div className="alert alert-error" style={{ marginTop: '0.5rem' }}>{scannerError}</div>}
                    <canvas ref={canvasRef} style={{ display: 'none' }} />
                  </div>
                )}

                <textarea
                  id="join-code"
                  className="form-control"
                  rows={4}
                  placeholder="Paste the invitation code here, scan a QR code, or import a saved invite file..."
                  value={joinCode}
                  onChange={(e) => setJoinCode(e.target.value)}
                  required
                />
              </div>
              <button type="submit" className="btn btn-primary" disabled={loading}>
                {loading ? 'Connecting...' : 'Join Room'}
              </button>
            </form>
          )}
        </div>
      )}
    </div>
  )
}
