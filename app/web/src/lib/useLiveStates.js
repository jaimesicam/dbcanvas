import { useCallback, useEffect, useRef, useState } from 'react'
import { stackApi } from './stackApi.js'
import { usePolling } from './usePolling.jsx'

// useLiveStates.js — the data behind the canvas's Live view (components/LiveOverlay.jsx).
//
// The server answers with what a node is doing at this instant: CPU and memory as they are,
// network and block I/O as counters since the container started. A counter is not something
// anyone can read, so this turns each pair of samples into a rate, and keeps a short history of
// CPU and memory for the popup's trend line.

export const LIVE_INTERVALS = [2000, 5000, 10000]
const HISTORY = 40

export function useLiveStates(stackId, enabled, intervalMs) {
  const [live, setLive] = useState({})
  const [error, setError] = useState('')
  const prev = useRef({}) // nodeId -> { t, netRx, netTx, blkRead, blkWrite, cpu: [], mem: [] }
  // A poll takes about as long as a container's stats sample (~1-2s), so at the 2s interval a slow
  // one could still be running when the next is due — that tick is skipped, not stacked.
  const inflight = useRef(false)

  // Switching off forgets everything, so switching on again does not draw a rate across the gap.
  useEffect(() => {
    if (!enabled) {
      prev.current = {}
      setLive({})
      setError('')
    }
  }, [enabled])

  const poll = useCallback(async () => {
    if (inflight.current) return
    inflight.current = true
    try {
      const res = await stackApi.live(stackId)
      const now = Date.now()
      const out = {}
      const seen = {}
      for (const [id, n] of Object.entries(res?.nodes || {})) {
        const p = prev.current[id]
        const dt = p ? (now - p.t) / 1000 : 0
        // A counter that went backwards is a restarted container: no rate this time round.
        const rate = (k) => (p && dt > 0 && n[k] >= p[k] ? (n[k] - p[k]) / dt : null)
        const cpu = [...(p?.cpu || []), n.cpuPercent || 0].slice(-HISTORY)
        const mem = [...(p?.mem || []), n.memPercent || 0].slice(-HISTORY)
        seen[id] = {
          t: now, netRx: n.netRx, netTx: n.netTx, blkRead: n.blkRead, blkWrite: n.blkWrite,
          readOps: n.readOps, writeOps: n.writeOps, cpu, mem,
        }
        out[id] = {
          ...n, netIn: rate('netRx'), netOut: rate('netTx'), diskRead: rate('blkRead'), diskWrite: rate('blkWrite'),
          readIops: n.readOps != null ? rate('readOps') : null, writeIops: n.writeOps != null ? rate('writeOps') : null,
          // I/O wait only where the kernel measures it per container (PSI); the host's iowait is
          // not the node's, so without PSI there is none to show.
          iowait: n.ioPressure ?? null, cpuHist: cpu, memHist: mem,
        }
      }
      prev.current = seen
      setLive(out)
      setError('')
    } catch (e) {
      setError(e?.message || 'live state unavailable')
    } finally {
      inflight.current = false
    }
  }, [stackId])

  usePolling(poll, enabled ? intervalMs : 0, { enabled })
  return { live, error }
}
