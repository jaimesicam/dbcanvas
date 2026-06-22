// Shared geometry + interaction helpers for the node-graph canvases
// (Node Editor and the Database Stack Designer).

export const PORTS = ['top', 'right', 'bottom', 'left']
export const PORT_DIR = { top: [0, -1], right: [1, 0], bottom: [0, 1], left: [-1, 0] }

export const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v))
export const dist = (a, b) => Math.hypot(a.x - b.x, a.y - b.y)

// Port anchor point on a rectangle {x,y,w,h}.
export function portPoint(r, port) {
  switch (port) {
    case 'top': return { x: r.x + r.w / 2, y: r.y }
    case 'right': return { x: r.x + r.w, y: r.y + r.h / 2 }
    case 'bottom': return { x: r.x + r.w / 2, y: r.y + r.h }
    case 'left': return { x: r.x, y: r.y + r.h / 2 }
    default: return { x: r.x, y: r.y }
  }
}

// Cubic bezier that leaves/enters each port perpendicular to the edge, anchored
// to the exact chosen ports (never re-routed to the nearest side).
export function edgePath(p0, port0, p1, port1) {
  const d0 = PORT_DIR[port0]
  const d1 = PORT_DIR[port1]
  const k = clamp(dist(p0, p1) / 2, 40, 170)
  const c0 = { x: p0.x + d0[0] * k, y: p0.y + d0[1] * k }
  const c1 = { x: p1.x + d1[0] * k, y: p1.y + d1[1] * k }
  return `M ${p0.x} ${p0.y} C ${c0.x} ${c0.y} ${c1.x} ${c1.y} ${p1.x} ${p1.y}`
}

// Convert a client point to world coordinates given the canvas wrapper rect and
// the current pan/zoom view {x,y,z}.
export function screenToWorld(rect, view, clientX, clientY) {
  return {
    x: (clientX - rect.left - view.x) / view.z,
    y: (clientY - rect.top - view.y) / view.z,
  }
}

// Cursor-anchored zoom step. Returns the next view {x,y,z}.
export function zoomAt(view, mx, my, deltaY, lo = 0.35, hi = 2.2) {
  const factor = deltaY < 0 ? 1.1 : 1 / 1.1
  const z = clamp(view.z * factor, lo, hi)
  const wx = (mx - view.x) / view.z
  const wy = (my - view.y) / view.z
  return { x: mx - wx * z, y: my - wy * z, z }
}
