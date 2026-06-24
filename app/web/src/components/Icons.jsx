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
  Dashboard: (p) => (
    <Svg {...p}>
      <rect x="3" y="3" width="7" height="9" rx="1.5" />
      <rect x="14" y="3" width="7" height="5" rx="1.5" />
      <rect x="14" y="12" width="7" height="9" rx="1.5" />
      <rect x="3" y="16" width="7" height="5" rx="1.5" />
    </Svg>
  ),
  // Database cluster: a stacked cylinder (classic DB symbol) for PXC nodes.
  Database: (p) => (
    <Svg {...p}>
      <ellipse cx="12" cy="5" rx="7" ry="2.6" />
      <path d="M5 5v6c0 1.45 3.13 2.6 7 2.6s7-1.15 7-2.6V5" />
      <path d="M5 11v6c0 1.45 3.13 2.6 7 2.6s7-1.15 7-2.6v-6" />
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
  Sliders: (p) => (
    <Svg {...p}>
      <line x1="4" y1="8" x2="20" y2="8" />
      <circle cx="9" cy="8" r="2.4" fill="var(--surface)" />
      <line x1="4" y1="16" x2="20" y2="16" />
      <circle cx="15" cy="16" r="2.4" fill="var(--surface)" />
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
  Line: (p) => (
    <Svg {...p}>
      <line x1="4" y1="12" x2="20" y2="12" />
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
}
