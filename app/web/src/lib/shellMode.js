import { useCallback, useEffect, useState } from 'react'

// shellMode.js — which shell this browser shows: the desktop (every page a window
// over a desktop, a Start menu, a taskbar — desktop/Desktop.jsx) or the classic one
// (a sidebar and a tab strip). A per-browser convenience, like a remembered filter:
// the desktop is the default, and Classic is the way back for whoever wants it.

const KEY = 'dbcanvas-shell'
export const SHELL_MODES = [
  { id: 'desktop', label: 'Desktop' },
  { id: 'classic', label: 'Classic' },
]

function read() {
  try { return localStorage.getItem(KEY) === 'classic' ? 'classic' : 'desktop' } catch { return 'desktop' }
}

export function useShellMode() {
  const [mode, setMode] = useState(read)
  useEffect(() => {
    const on = () => setMode(read())
    addEventListener('dbcanvas:shell', on)
    addEventListener('storage', on)
    return () => { removeEventListener('dbcanvas:shell', on); removeEventListener('storage', on) }
  }, [])
  const set = useCallback((m) => {
    try { localStorage.setItem(KEY, m) } catch { /* the choice still applies to this page */ }
    setMode(m)
    dispatchEvent(new Event('dbcanvas:shell'))
  }, [])
  return [mode, set]
}
