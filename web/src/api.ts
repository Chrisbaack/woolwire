// api wraps fetch for the local Woolwire API.
//
// Every request carries X-Woolwire-Request. The server requires it (or a
// matching Origin) on state-changing requests: the session cookie is
// same-site to anything else served from 127.0.0.1, so another local web app
// could otherwise rotate invitations or remove members with a simple
// cross-origin POST. A cross-origin page cannot set a custom header without a
// preflight, and the preflight is refused.
export async function api(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers)
  headers.set('X-Woolwire-Request', '1')

  // JSON routes reject other content types, so a body implies JSON unless the
  // caller said otherwise.
  if (init.body !== undefined && init.body !== null && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  return fetch(path, { ...init, headers, credentials: 'same-origin' })
}

// apiJSON performs a request and decodes the response, throwing the server's
// message on failure so callers can surface it directly.
export async function apiJSON<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await api(path, init)
  if (!res.ok) {
    const message = await res.text()
    throw new Error(message.trim() || `Request to ${path} failed with ${res.status}`)
  }
  return (await res.json()) as T
}
