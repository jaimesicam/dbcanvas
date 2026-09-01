// Hand-written inline SVG icons. All use stroke=currentColor, round caps/joins,
// 24x24 viewBox, and accept a `size` prop (default 18).

function Svg({ size = 18, children, fill = 'none', ...rest }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill={fill}
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      {...rest}
    >
      {children}
    </svg>
  )
}

export const Icon = {
  // The DBCanvas mark: a canvas on an easel, and what is painted on it is databases.
  //
  // The name is the brief — a canvas you arrange databases on — so the mark is a stretched
  // canvas with two cylinders set on it like dabs of paint. Two, offset: a level pair reads
  // as a pair of eyes above the easel legs, which at 96px is all anyone sees. Drawn on its
  // own rather than through Svg because it sits on the filled primary badge.
  Brand: ({ size = 20, ...rest }) => (
    <svg
      width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"
      role="img" aria-label="DBCanvas" {...rest}
    >
      {/* the stretched canvas, and the easel it stands on */}
      <rect x="3" y="3.2" width="18" height="13.6" rx="2.2" />
      <path d="M8.4 20.8 12 17l3.6 3.8" />
      {/* two databases set on it like dabs of paint */}
      <ellipse cx="9.3" cy="7.4" rx="1.95" ry="0.82" />
      <path d="M7.35 7.4v2.3c0 .63.87 1.15 1.95 1.15s1.95-.52 1.95-1.15V7.4" />
      <ellipse cx="14.6" cy="11.3" rx="1.95" ry="0.82" />
      <path d="M12.65 11.3v2.3c0 .63.87 1.15 1.95 1.15s1.95-.52 1.95-1.15V11.3" />
    </svg>
  ),

  Dashboard: (p) => (
    <Svg {...p}>
      <rect x="3" y="3" width="7" height="9" rx="1.5" />
      <rect x="14" y="3" width="7" height="5" rx="1.5" />
      <rect x="14" y="12" width="7" height="9" rx="1.5" />
      <rect x="3" y="16" width="7" height="5" rx="1.5" />
    </Svg>
  ),
  // Database cluster: three database cylinders (PXC frame + nodes).
  Database: (p) => (
    <Svg {...p}>
      {/* top member */}
      <ellipse cx="12" cy="4" rx="3" ry="1.3" />
      <path d="M9 4v5c0 .8 1.34 1.3 3 1.3s3-.5 3-1.3V4" />
      {/* bottom-left member */}
      <ellipse cx="6" cy="12" rx="3" ry="1.3" />
      <path d="M3 12v5c0 .8 1.34 1.3 3 1.3s3-.5 3-1.3v-5" />
      {/* bottom-right member */}
      <ellipse cx="18" cy="12" rx="3" ry="1.3" />
      <path d="M15 12v5c0 .8 1.34 1.3 3 1.3s3-.5 3-1.3v-5" />
    </Svg>
  ),
  // Monitoring: a metrics panel with a live activity/pulse trace.
  Monitor: (p) => (
    <Svg {...p}>
      <rect x="3" y="4" width="18" height="14" rx="2" />
      <path d="M6 13h2l1.5-4 2.5 7 1.5-3H18" />
      <line x1="9" y1="21" x2="15" y2="21" />
      <line x1="12" y1="18" x2="12" y2="21" />
    </Svg>
  ),
  // Labs: a lab flask, for the experimental hands-on scenarios feature.
  Flask: (p) => (
    <Svg {...p}>
      <path d="M9 3h6" />
      <path d="M10 3v6.5L4.5 19a2 2 0 0 0 1.7 3h11.6a2 2 0 0 0 1.7-3L14 9.5V3" />
      <path d="M7.5 15h9" />
    </Svg>
  ),
  Sliders: (p) => (
    <Svg {...p}>
      <line x1="4" y1="8" x2="20" y2="8" />
      <circle cx="9" cy="8" r="2.4" fill="var(--surface)" />
      <line x1="4" y1="16" x2="20" y2="16" />
      <circle cx="15" cy="16" r="2.4" fill="var(--surface)" />
    </Svg>
  ),
  // Eye / EyeOff: reveal or hide a masked secret.
  Eye: (p) => (
    <Svg {...p}>
      <path d="M2 12s3.6-6 10-6 10 6 10 6-3.6 6-10 6-10-6-10-6Z" />
      <circle cx="12" cy="12" r="2.8" />
    </Svg>
  ),
  EyeOff: (p) => (
    <Svg {...p}>
      <path d="M4 4l16 16" />
      <path d="M9.6 5.4A10.5 10.5 0 0 1 12 6c6.4 0 10 6 10 6a17.6 17.6 0 0 1-3.4 3.9" />
      <path d="M6.4 7.6A17.4 17.4 0 0 0 2 12s3.6 6 10 6a10.9 10.9 0 0 0 3.2-.5" />
      <path d="M9.9 9.9a2.8 2.8 0 0 0 3.9 3.9" />
    </Svg>
  ),
  // Vault: a safe door — dial + handle spokes (OpenBao secrets manager).
  Vault: (p) => (
    <Svg {...p}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <circle cx="12" cy="12" r="3.6" />
      <path d="M12 6.6v1.8M12 15.6v1.8M6.6 12h1.8M15.6 12h1.8" />
    </Svg>
  ),
  // Settings: an eight-toothed gear around a hub.
  Settings: (p) => (
    <Svg {...p}>
      <circle cx="12" cy="12" r="3.2" />
      <path d="M12 2.6v2.6M12 18.8v2.6M21.4 12h-2.6M5.2 12H2.6M18.6 5.4l-1.8 1.8M7.2 16.8l-1.8 1.8M18.6 18.6l-1.8-1.8M7.2 7.2 5.4 5.4" />
    </Svg>
  ),
  // ProxySQL: a proxy/router box routing a client (left) to three cluster
  // backends (right) — the MySQL proxy fronting a PXC cluster.
  ProxySQL: (p) => (
    <Svg {...p}>
      <circle cx="3.2" cy="12" r="1.5" />
      <line x1="4.7" y1="12" x2="8" y2="12" />
      <rect x="8" y="8.5" width="5.5" height="7" rx="1.5" />
      <line x1="13.5" y1="10.5" x2="17.4" y2="5.5" />
      <line x1="13.5" y1="12" x2="17.8" y2="12" />
      <line x1="13.5" y1="13.5" x2="17.4" y2="18.5" />
      <circle cx="19" cy="5" r="1.6" />
      <circle cx="19.4" cy="12" r="1.6" />
      <circle cx="19" cy="19" r="1.6" />
    </Svg>
  ),
  Nodes: (p) => (
    <Svg {...p}>
      <circle cx="6" cy="6" r="2.5" />
      <circle cx="18" cy="6" r="2.5" />
      <circle cx="12" cy="18" r="2.5" />
      <line x1="8.2" y1="7.3" x2="10.4" y2="15.8" />
      <line x1="15.8" y1="7.3" x2="13.6" y2="15.8" />
    </Svg>
  ),
  Frame: (p) => (
    <Svg {...p}>
      <path d="M4 8V5a1 1 0 0 1 1-1h3" />
      <path d="M20 8V5a1 1 0 0 0-1-1h-3" />
      <path d="M4 16v3a1 1 0 0 0 1 1h3" />
      <path d="M20 16v3a1 1 0 0 1-1 1h-3" />
    </Svg>
  ),
  Grid: (p) => (
    <Svg {...p}>
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
      <rect x="14" y="14" width="7" height="7" rx="1" />
    </Svg>
  ),
  Table: (p) => (
    <Svg {...p}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <line x1="3" y1="9" x2="21" y2="9" />
      <line x1="3" y1="14" x2="21" y2="14" />
      <line x1="11" y1="9" x2="11" y2="20" />
    </Svg>
  ),
  Kanban: (p) => (
    <Svg {...p}>
      <rect x="3" y="3" width="5" height="18" rx="1.5" />
      <rect x="9.5" y="3" width="5" height="12" rx="1.5" />
      <rect x="16" y="3" width="5" height="15" rx="1.5" />
    </Svg>
  ),
  Sun: (p) => (
    <Svg {...p}>
      <circle cx="12" cy="12" r="4" />
      <line x1="12" y1="2" x2="12" y2="4" />
      <line x1="12" y1="20" x2="12" y2="22" />
      <line x1="2" y1="12" x2="4" y2="12" />
      <line x1="20" y1="12" x2="22" y2="12" />
      <line x1="4.9" y1="4.9" x2="6.3" y2="6.3" />
      <line x1="17.7" y1="17.7" x2="19.1" y2="19.1" />
      <line x1="4.9" y1="19.1" x2="6.3" y2="17.7" />
      <line x1="17.7" y1="6.3" x2="19.1" y2="4.9" />
    </Svg>
  ),
  Search: (p) => (
    <Svg {...p}>
      <circle cx="11" cy="11" r="7" />
      <line x1="16.2" y1="16.2" x2="21" y2="21" />
    </Svg>
  ),
  Bell: (p) => (
    <Svg {...p}>
      <path d="M6 9a6 6 0 0 1 12 0c0 5 2 6 2 6H4s2-1 2-6" />
      <path d="M10 19a2 2 0 0 0 4 0" />
    </Svg>
  ),
  Plus: (p) => (
    <Svg {...p}>
      <line x1="12" y1="5" x2="12" y2="19" />
      <line x1="5" y1="12" x2="19" y2="12" />
    </Svg>
  ),
  Trash: (p) => (
    <Svg {...p}>
      <line x1="4" y1="6" x2="20" y2="6" />
      <path d="M9 6V4h6v2" />
      <path d="M6 6l1 14a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-14" />
      <line x1="10" y1="10" x2="10" y2="17" />
      <line x1="14" y1="10" x2="14" y2="17" />
    </Svg>
  ),
  Check: (p) => (
    <Svg {...p}>
      <polyline points="4 12 9 17 20 6" />
    </Svg>
  ),
  Chevron: (p) => (
    <Svg {...p}>
      <polyline points="6 9 12 15 18 9" />
    </Svg>
  ),
  // The four verdict glyphs. A status is never colour alone — these ship beside
  // the colour and the word, so the reading survives a greyscale print and a
  // colour-blind reader. Their silhouettes are deliberately different at small
  // sizes: a circle, a triangle, an octagon, a bare dot.
  StatusOk: (p) => (
    <Svg {...p}>
      <circle cx="12" cy="12" r="9" />
      <polyline points="8 12 11 15 16 9" />
    </Svg>
  ),
  StatusWarn: (p) => (
    <Svg {...p}>
      <path d="M12 3.5 21 19H3z" />
      <line x1="12" y1="9.5" x2="12" y2="14" />
      <line x1="12" y1="16.5" x2="12" y2="16.5" />
    </Svg>
  ),
  StatusCrit: (p) => (
    <Svg {...p}>
      <path d="M8.4 3h7.2L21 8.4v7.2L15.6 21H8.4L3 15.6V8.4z" />
      <line x1="12" y1="8" x2="12" y2="12.5" />
      <line x1="12" y1="16" x2="12" y2="16" />
    </Svg>
  ),
  // The tooltip affordance. A question mark rather than an "i": these answer "what do
  // I do with this?", which is a question, and it keeps them visually distinct from
  // StatusInfo below, which reports severity.
  Help: (p) => (
    <Svg {...p}>
      <circle cx="12" cy="12" r="9" />
      <path d="M9.6 9.3a2.5 2.5 0 1 1 3.2 2.6c-.6.2-.9.7-.9 1.3v.5" />
      <line x1="12" y1="16.7" x2="12" y2="16.7" />
    </Svg>
  ),
  StatusInfo: (p) => (
    <Svg {...p}>
      <circle cx="12" cy="12" r="9" />
      <line x1="12" y1="11" x2="12" y2="16.5" />
      <line x1="12" y1="7.8" x2="12" y2="7.8" />
    </Svg>
  ),
  Drag: (p) => (
    <Svg {...p}>
      <circle cx="9" cy="6" r="1.3" fill="currentColor" stroke="none" />
      <circle cx="15" cy="6" r="1.3" fill="currentColor" stroke="none" />
      <circle cx="9" cy="12" r="1.3" fill="currentColor" stroke="none" />
      <circle cx="15" cy="12" r="1.3" fill="currentColor" stroke="none" />
      <circle cx="9" cy="18" r="1.3" fill="currentColor" stroke="none" />
      <circle cx="15" cy="18" r="1.3" fill="currentColor" stroke="none" />
    </Svg>
  ),
  Arrow: (p) => (
    <Svg {...p}>
      <line x1="4" y1="12" x2="20" y2="12" />
      <polyline points="14 6 20 12 14 18" />
    </Svg>
  ),
  ArrowLeft: (p) => (
    <Svg {...p}>
      <line x1="20" y1="12" x2="4" y2="12" />
      <polyline points="10 6 4 12 10 18" />
    </Svg>
  ),
  Both: (p) => (
    <Svg {...p}>
      <line x1="5" y1="12" x2="19" y2="12" />
      <polyline points="9 7 4 12 9 17" />
      <polyline points="15 7 20 12 15 17" />
    </Svg>
  ),
  // Packet Inspector: rows of captured traffic (a packet list, shortening as it goes) with
  // a lens over them. A plain magnifier is already Search, and a plain waveform is already
  // Monitor — the pairing is what distinguishes this one at sidebar size.
  Packet: (p) => (
    <Svg {...p}>
      <line x1="3" y1="4.5" x2="18" y2="4.5" />
      <line x1="3" y1="9" x2="13.5" y2="9" />
      <line x1="3" y1="13.5" x2="8.5" y2="13.5" />
      <circle cx="15.5" cy="14.5" r="5" />
      <line x1="19.2" y1="18.2" x2="21.5" y2="20.5" />
    </Svg>
  ),
  // Log Summary: log lines with a warning over them — the page reads several nodes'
  // logs and classifies every event, so severity is the subject, not the text. The mark
  // sits top-right and is angular on purpose: Packet is lines plus a round lens at the
  // bottom-right, and the two sit four rows apart in the same sidebar.
  Logs: (p) => (
    <Svg {...p}>
      <path d="M17 3.2 21.8 11.4h-9.6z" />
      <path d="M17 6.2v2.2" />
      <path d="M17 9.9v0.01" />
      <path d="M3 6h6.5" />
      <path d="M3 11h6.5" />
      <path d="M3 16h18" />
      <path d="M3 20.8h9" />
    </Svg>
  ),
  Move: (p) => (
    <Svg {...p}>
      <polyline points="9 5 12 2 15 5" />
      <polyline points="9 19 12 22 15 19" />
      <polyline points="5 9 2 12 5 15" />
      <polyline points="19 9 22 12 19 15" />
      <line x1="12" y1="2" x2="12" y2="22" />
      <line x1="2" y1="12" x2="22" y2="12" />
    </Svg>
  ),
  Mineral: (p) => (
    <Svg {...p}>
      <polygon points="12 3 19 8 16 20 8 20 5 8" />
      <line x1="5" y1="8" x2="19" y2="8" />
      <line x1="12" y1="3" x2="12" y2="20" />
    </Svg>
  ),
  Unit: (p) => (
    <Svg {...p}>
      <rect x="4" y="4" width="16" height="16" rx="2" />
      <line x1="4" y1="10" x2="20" y2="10" />
      <line x1="10" y1="10" x2="10" y2="20" />
    </Svg>
  ),
  Users: (p) => (
    <Svg {...p}>
      <circle cx="9" cy="8" r="3" />
      <path d="M3 20c0-3.3 2.7-5 6-5s6 1.7 6 5" />
      <path d="M16 5.5a3 3 0 0 1 0 5.5" />
      <path d="M21 20c0-2.6-1.4-4.2-3.5-4.8" />
    </Svg>
  ),
  Logout: (p) => (
    <Svg {...p}>
      <path d="M14 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4" />
      <polyline points="9 8 13 12 9 16" />
      <line x1="3" y1="12" x2="13" y2="12" />
    </Svg>
  ),
  Server: (p) => (
    <Svg {...p}>
      <rect x="3" y="4" width="18" height="7" rx="1.5" />
      <rect x="3" y="13" width="18" height="7" rx="1.5" />
      <circle cx="7" cy="7.5" r="0.7" fill="currentColor" stroke="none" />
      <circle cx="7" cy="16.5" r="0.7" fill="currentColor" stroke="none" />
      <line x1="15" y1="7.5" x2="18" y2="7.5" />
      <line x1="15" y1="16.5" x2="18" y2="16.5" />
    </Svg>
  ),
  Copy: (p) => (
    <Svg {...p}>
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M5 15V5a2 2 0 0 1 2-2h8" />
    </Svg>
  ),
  External: (p) => (
    <Svg {...p}>
      <path d="M14 4h6v6" />
      <path d="M20 4 10 14" />
      <path d="M19 13v6a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h6" />
    </Svg>
  ),
  // Object storage (S3 bucket) — used for the SeaweedFS node.
  Bucket: (p) => (
    <Svg {...p}>
      <path d="M5 7h14l-1.3 12.1a1 1 0 0 1-1 .9H7.3a1 1 0 0 1-1-.9L5 7Z" />
      <ellipse cx="12" cy="7" rx="7" ry="2.4" />
    </Svg>
  ),
  // File Manager: a directory entry, a plain file, a symlink, and the dialog's
  // close affordance.
  Folder: (p) => (
    <Svg {...p}>
      <path d="M3 7.5A1.5 1.5 0 0 1 4.5 6h4l2 2.5h7A1.5 1.5 0 0 1 19 10v7.5a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 3 17.5v-10Z" />
    </Svg>
  ),
  File: (p) => (
    <Svg {...p}>
      <path d="M6 3.5h7L18.5 9v11.5a1 1 0 0 1-1 1h-11a1 1 0 0 1-1-1v-16a1 1 0 0 1 1-1Z" />
      <path d="M13 3.5V9h5.5" />
    </Svg>
  ),
  Link: (p) => (
    <Svg {...p}>
      <path d="M10 13.5a3.5 3.5 0 0 0 5 0l3-3a3.5 3.5 0 0 0-5-5l-1.5 1.5" />
      <path d="M14 10.5a3.5 3.5 0 0 0-5 0l-3 3a3.5 3.5 0 0 0 5 5L12.5 17" />
    </Svg>
  ),
  Close: (p) => (
    <Svg {...p}>
      <path d="M6 6l12 12M18 6L6 18" />
    </Svg>
  ),
  // The Operator Debugger's own set. A debugger's controls are a vocabulary people
  // already know from every IDE, so these are deliberately the familiar shapes rather
  // than something novel: continue is a play triangle, pause two bars, and the three
  // step marks are an arc over a dot (over), into a dot (down), and out of one (up).
  // The dot is the current line in all three, which is what makes them a set.
  Bug: (p) => (
    <Svg {...p}>
      <rect x="7" y="7.5" width="10" height="12" rx="5" />
      <path d="M9.2 6.2a2.8 2.8 0 0 1 5.6 0" />
      <path d="M7 11H3.5M7 15.5H4M17 11h3.5M17 15.5H20" />
      <path d="M8.2 8.2 6 5.6M15.8 8.2 18 5.6" />
      <path d="M12 11v6" />
    </Svg>
  ),
  Play: (p) => (
    <Svg {...p}>
      <path d="M7 4.5 19 12 7 19.5Z" />
    </Svg>
  ),
  Pause: (p) => (
    <Svg {...p}>
      <path d="M9 5v14M15 5v14" />
    </Svg>
  ),
  StepOver: (p) => (
    <Svg {...p}>
      <path d="M5 14a7 7 0 0 1 14 0" />
      <polyline points="15.5 13.5 19 14 19.5 10.5" />
      <circle cx="12" cy="19" r="1.8" />
    </Svg>
  ),
  StepInto: (p) => (
    <Svg {...p}>
      <path d="M12 3.5v9" />
      <polyline points="8.5 9 12 12.5 15.5 9" />
      <circle cx="12" cy="18.5" r="1.8" />
    </Svg>
  ),
  // Edit in place — the debugger's variables can be written, not only read.
  Pencil: (p) => (
    <Svg {...p}>
      <path d="M4 20.5h4.2L20 8.7a2.1 2.1 0 0 0-3-3L5.2 17.5 4 20.5Z" />
      <path d="M14.8 6.9 17.4 9.5" />
    </Svg>
  ),
  // Maximize / Restore for the debugger's panels: arrows out of the corners, then into them.
  // Two marks rather than one that flips, because the button says what will happen next and
  // "the arrows point the way the panel is about to go" is the whole of the idea.
  Maximize: (p) => (
    <Svg {...p}>
      <polyline points="14 4 20 4 20 10" />
      <polyline points="10 20 4 20 4 14" />
      <path d="M20 4l-7 7" />
      <path d="M4 20l7-7" />
    </Svg>
  ),
  Minimize: (p) => (
    <Svg {...p}>
      <polyline points="20 10 14 10 14 4" />
      <polyline points="4 14 10 14 10 20" />
      <path d="M14 10l6-6" />
      <path d="M10 14l-6 6" />
    </Svg>
  ),
  StepOut: (p) => (
    <Svg {...p}>
      <path d="M12 12.5v-9" />
      <polyline points="8.5 7 12 3.5 15.5 7" />
      <circle cx="12" cy="18.5" r="1.8" />
    </Svg>
  ),
}
