import { useTheme, THEMES } from '../theme/ThemeProvider.jsx'
import { Icon } from '../components/Icons.jsx'
import { ENGINE_TONE } from '../lib/engines.js'

export function Splash() {
  return (
    <div className="flex h-full items-center justify-center bg-bg">
      <div className="h-10 w-10 animate-spin rounded-full border-2 border-surface2 border-t-primary" />
    </div>
  )
}

function ThemeSwatches() {
  const { theme, setTheme } = useTheme()
  return (
    <div className="absolute right-4 top-4 flex gap-1.5">
      {THEMES.map((t) => (
        <button
          key={t.id}
          title={t.label}
          onClick={() => setTheme(t.id)}
          className={`h-5 w-5 rounded-full border-2 transition ${theme === t.id ? 'border-fg scale-110' : 'border-transparent'}`}
          style={{ background: t.swatch }}
        />
      ))}
    </div>
  )
}

// The engines DBCanvas deploys, in their colours (lib/engines.js) — the same four the
// Dashboard, the canvas and a stack's folder use, so the first screen already speaks the
// app's colour language.
const LOGIN_ENGINES = [['MySQL', 'mysql'], ['PostgreSQL', 'postgres'], ['MongoDB', 'mongodb'], ['Valkey', 'valkey']]

// StackSketch is the login panel's illustration: a small stack drawn the way the canvas
// draws one — a three-member cluster behind a proxy, an application on the proxy — in
// the engine colours. Decorative (aria-hidden): it says what the app is for faster than
// a sentence, and the sentence is there as well.
function StackSketch() {
  const node = (x, y, tone, w = 92) => (
    <g transform={`translate(${x} ${y})`}>
      <rect width={w} height="34" rx="8" fill="var(--surface)" stroke="var(--border)" />
      <rect width={w} height="3" rx="1.5" fill={tone} />
      <rect x="9" y="10" width="16" height="16" rx="4" fill={`color-mix(in srgb, ${tone} 18%, var(--surface))`} stroke={`color-mix(in srgb, ${tone} 40%, transparent)`} />
      <rect x="32" y="13" width={w - 52} height="4" rx="2" fill="var(--fg)" opacity=".55" />
      <rect x="32" y="21" width={(w - 52) * 0.6} height="3" rx="1.5" fill="var(--muted)" opacity=".5" />
      <circle cx={w - 11} cy="17" r="3" fill="var(--status-ok)" />
    </g>
  )
  return (
    <svg viewBox="0 0 340 210" className="w-full max-w-[360px]" aria-hidden="true">
      <rect x="6" y="6" width="318" height="62" rx="12" fill={`color-mix(in srgb, ${ENGINE_TONE.mysql} 8%, transparent)`}
        stroke={ENGINE_TONE.mysql} strokeDasharray="5 4" strokeWidth="1.5" />
      {node(14, 24, ENGINE_TONE.mysql, 96)}{node(117, 24, ENGINE_TONE.mysql, 96)}{node(220, 24, ENGINE_TONE.mysql, 96)}
      <path d="M165 68 V104" stroke="var(--muted)" strokeWidth="2" fill="none" />
      <path d="M165 68 V104" stroke="var(--surface)" strokeWidth="2" fill="none" className="stk-flow" />
      {node(110, 104, 'var(--muted)', 110)}
      <path d="M165 138 V168" stroke="var(--muted)" strokeWidth="2" fill="none" />
      <path d="M165 138 V168" stroke="var(--surface)" strokeWidth="2" fill="none" className="stk-flow" />
      {node(110, 168, 'var(--node-3)', 110)}
    </svg>
  )
}

export function Shell({ title, subtitle, children }) {
  return (
    <div className="desk-wallpaper relative flex h-full items-center justify-center gap-16 overflow-auto p-4">
      <ThemeSwatches />
      {/* What DBCanvas is, beside the form — only where there is room for it; on a
          phone or a narrow window the form is the whole screen, as it was. */}
      <div className="hidden max-w-sm animate-fade-in flex-col gap-6 lg:flex">
        <div>
          <div className="flex items-center gap-2.5">
            <span className="flex h-9 w-9 items-center justify-center rounded-xl bg-primary text-primary-fg"><Icon.Brand size={22} /></span>
            <span className="text-2xl font-semibold tracking-tight text-fg">DBCanvas</span>
          </div>
          <p className="mt-3 text-[15px] leading-relaxed text-muted">
            Design a database topology on a canvas, deploy it as real containers, then load it, break it and read what it did.
          </p>
        </div>
        <StackSketch />
        <div className="flex flex-wrap gap-1.5">
          {LOGIN_ENGINES.map(([label, id]) => (
            <span key={id} className="ui-badge inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs text-fg" style={{ borderColor: `color-mix(in srgb, ${ENGINE_TONE[id]} 35%, transparent)` }}>
              <span className="h-2 w-2 rounded-full" style={{ background: ENGINE_TONE[id] }} />{label}
            </span>
          ))}
        </div>
      </div>
      <div className="w-full max-w-md animate-fade-in rounded-2xl border bg-surface p-6 shadow-xl">
        <div className="mb-5 flex items-center gap-3">
          <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-primary text-primary-fg">
            <Icon.Brand size={24} />
          </div>
          <div>
            <h1 className="text-lg font-semibold text-fg">{title}</h1>
            {subtitle && <p className="text-sm text-muted">{subtitle}</p>}
          </div>
        </div>
        {children}
        {/* GPLv3 §5d: an interactive interface shows where its licence is — before sign-in too. */}
        <p className="mt-5 text-center text-[11px] text-muted">
          Free software under the GPL-3.0, with no warranty ·{' '}
          <a href="/licenses/" target="_blank" rel="noreferrer" className="hover:text-fg hover:underline">Licences and source</a>
        </p>
      </div>
    </div>
  )
}

// SoloSignInScreen is what a browser sees without a session when DBCanvas runs as the
// dbcanvas command: there are no passwords, and the command opens a one-time sign-in link.
export function SoloSignInScreen() {
  return (
    <Shell title="Open DBCanvas from the terminal" subtitle="This browser is not signed in">
      <p className="text-sm leading-relaxed text-fg">
        Run <code className="rounded bg-surface2 px-1.5 py-0.5 font-mono text-[13px]">dbcanvas</code> in a terminal on this computer. It opens DBCanvas here, signed in.
      </p>
      <p className="mt-3 text-sm leading-relaxed text-muted">
        Each link it opens works once and only for a minute, so a link from your history will not sign anyone in.
      </p>
    </Shell>
  )
}
