import { record, EventType, IncrementalSource } from 'rrweb'

// mirrorCanvas.js — canvases inside iframes, for the mirror (session/Mirror.jsx).
//
// rrweb samples canvases a few times a second, but only those in the top-level page:
// its sampler walks `document.querySelectorAll('canvas')` and never looks inside an
// iframe. A VNC desktop is noVNC drawing into a canvas inside a browser window's
// iframe, so the mirror carried its picture only in a full snapshot — a viewer saw
// whatever was on the desktop when they joined or reloaded, and then nothing more.
//
// This is the same sampler for the canvases rrweb skips: every frame interval it
// walks the same-origin iframes, encodes each canvas that changed, and hands back the
// very event rrweb's own sampler would have emitted (clear, then draw the image), so
// the replay draws it with no help. A canvas whose picture has not changed sends
// nothing, which is most of the time for a desktop nobody is using.

const CANVAS_2D = 0 // rrweb's CanvasContext['2D']

// iframeCanvases is every canvas inside a same-origin iframe, at any depth, that is
// not in a blocked (private) part of the page.
function iframeCanvases(doc, blocked, out = []) {
  for (const f of doc.querySelectorAll('iframe')) {
    if (blocked && f.closest(blocked)) continue
    let inner = null
    try { inner = f.contentDocument } catch { /* cross-origin: rrweb could not record it either */ }
    if (!inner) continue
    inner.querySelectorAll('canvas').forEach((c) => out.push(c))
    iframeCanvases(inner, null, out)
  }
  return out
}

const toBase64 = (buf) => {
  const bytes = new Uint8Array(buf)
  let s = ''
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000))
  return btoa(s)
}

// startIframeCanvasSampler calls emit(event) with a canvas-mutation event for each
// iframe canvas whose picture changed, at most fps times a second, until the
// returned stop() is called. Call resync() after a full snapshot, so the next round
// sends every canvas again rather than only the ones that changed.
export function startIframeCanvasSampler(emit, { fps = 3, blocked = null, type = 'image/webp', quality = 0.6 } = {}) {
  let last = new WeakMap() // canvas -> the last picture sent
  const busy = new WeakSet()
  let gen = 0
  const tick = () => {
    for (const canvas of iframeCanvases(document, blocked)) {
      if (busy.has(canvas) || !canvas.width || !canvas.height) continue
      const id = record.mirror.getId(canvas)
      if (id < 0) continue // not in the recording (yet)
      busy.add(canvas)
      const g = gen
      canvas.toBlob(async (blob) => {
        try {
          if (!blob || g !== gen) return
          const base64 = toBase64(await blob.arrayBuffer())
          if (last.get(canvas) === base64) return
          last.set(canvas, base64)
          emit({
            type: EventType.IncrementalSnapshot,
            timestamp: Date.now(),
            data: {
              source: IncrementalSource.CanvasMutation,
              id,
              type: CANVAS_2D,
              commands: [
                { property: 'clearRect', args: [0, 0, canvas.width, canvas.height] },
                {
                  property: 'drawImage',
                  args: [{ rr_type: 'ImageBitmap', args: [{ rr_type: 'Blob', data: [{ rr_type: 'ArrayBuffer', base64 }], type: blob.type }] }, 0, 0],
                },
              ],
            },
          })
        } catch { /* a canvas that went away mid-encode */ } finally {
          busy.delete(canvas)
        }
      }, type, quality)
    }
  }
  const t = setInterval(tick, Math.round(1000 / fps))
  return {
    stop: () => { clearInterval(t); gen++ },
    // After a snapshot every canvas gets a fresh id and the viewer a fresh replay.
    resync: () => { gen++; last = new WeakMap() },
  }
}
