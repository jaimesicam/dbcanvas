// guest.js — how a shared-session guest's browser talks to DBCanvas.
//
// A guest opens /join/<token> and stays on that path for the whole session; the page
// is the ordinary app. What differs is that every request the app makes is marked as
// a guest's (app/share.go): the X-DBCanvas-Guest header on fetch and XHR, ?guest=1 on
// sockets, event streams and download links, which cannot carry a header. The server
// then resolves the guest cookie to the host they act as.
//
// It is marked per request rather than by the cookie alone because a colleague who
// also has an account here keeps their own session in their own tabs: only the tab
// on /join/ is a guest.
//
// Installed once, before anything renders, by patching the transports rather than the
// twenty API modules that use them — a module added later is covered without knowing.

export const GUEST_HEADER = 'X-DBCanvas-Guest'

// joinToken is the share link's token when this tab is a guest's, else ''.
export function joinToken() {
  const p = location.pathname
  return p.startsWith('/join/') ? decodeURIComponent(p.slice('/join/'.length).split('/')[0] || '') : ''
}

export const isGuestMode = () => !!joinToken()

function sameOriginApi(u) {
  try {
    const url = new URL(u, location.href)
    return url.origin === location.origin && url.pathname.startsWith('/api/')
  } catch {
    return false
  }
}

// guestURL adds ?guest=1 to a same-origin API URL, keeping ws:/wss: as they are.
export function guestURL(u) {
  try {
    const url = new URL(u, location.href)
    if (url.pathname.startsWith('/api/') && (url.host === location.host)) url.searchParams.set('guest', '1')
    return url.toString()
  } catch {
    return u
  }
}

let installed = false

export function installGuestTransport() {
  if (installed || typeof window === 'undefined') return
  installed = true

  const origFetch = window.fetch.bind(window)
  window.fetch = (input, init = {}) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input?.url
    if (url && sameOriginApi(url)) {
      const headers = new Headers(init.headers || (input instanceof Request ? input.headers : undefined))
      headers.set(GUEST_HEADER, '1')
      init = { ...init, headers }
    }
    return origFetch(input, init)
  }

  const open = XMLHttpRequest.prototype.open
  const send = XMLHttpRequest.prototype.send
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    this.__dbcGuest = sameOriginApi(String(url))
    return open.call(this, method, url, ...rest)
  }
  XMLHttpRequest.prototype.send = function (body) {
    if (this.__dbcGuest) this.setRequestHeader(GUEST_HEADER, '1')
    return send.call(this, body)
  }

  const WS = window.WebSocket
  window.WebSocket = class GuestWebSocket extends WS {
    constructor(url, protocols) { super(guestURL(String(url)), protocols) }
  }
  const ES = window.EventSource
  if (ES) {
    window.EventSource = class GuestEventSource extends ES {
      constructor(url, opts) { super(guestURL(String(url)), opts) }
    }
  }

  // Download links and window.open cannot carry a header.
  document.addEventListener('click', (e) => {
    const a = e.target?.closest?.('a[href]')
    if (a && sameOriginApi(a.getAttribute('href'))) a.setAttribute('href', guestURL(a.getAttribute('href')))
  }, true)
  const wopen = window.open
  window.open = (url, ...rest) => wopen.call(window, url && sameOriginApi(String(url)) ? guestURL(String(url)) : url, ...rest)
}
