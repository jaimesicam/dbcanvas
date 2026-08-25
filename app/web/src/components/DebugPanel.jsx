import { createContext, useContext, useEffect, useMemo, useRef } from 'react'
import { Icon } from './Icons.jsx'

// The panel chrome the two debugger-shaped pages share — the Operator Debugger and the Core Dump
// Analyzer. It lived inside OperatorDebugger.jsx until there was a second page with the same
// problem; nothing about it is specific to either.
//
// Every panel can be maximized and docked back again. Three columns is the right default and the
// wrong one the moment you actually need something: a 400-frame stack, a struct four levels deep,
// a C++ signature that runs past the middle column. Maximizing covers the workspace with the one
// panel you are reading; docking back puts it where it was.
//
// It is done by *covering* rather than by re-laying-out: the maximized panel becomes
// `absolute inset-0` over the grid, and its siblings stay mounted underneath. That is not a
// shortcut — a panel that unmounts loses what you expanded in it, and coming back from full screen
// to a collapsed variables tree would defeat the point.
//
// PanelMaximize is a context so the panels (which are also rendered on their own by the smoke
// suite) do not each need the state threaded through them. Without a provider there is simply no
// maximize button.
const MaximizeContext = createContext(null)

export function PanelMaximize({ value, onChange, children }) {
  const ctx = useMemo(() => ({
    maxId: value,
    toggle: (id) => onChange(value === id ? null : id),
  }), [value, onChange])
  return <MaximizeContext.Provider value={ctx}>{children}</MaximizeContext.Provider>
}

export function useMaximize(id) {
  const ctx = useContext(MaximizeContext)
  return {
    maxed: !!id && ctx?.maxId === id,
    toggle: ctx && id ? () => ctx.toggle(id) : null,
  }
}

// The class that lifts a panel over the workspace. Its sizing classes are dropped with it — a
// maximized panel that kept `max-h-52` would be a tall panel in an empty page.
export const MAXED_CLS = 'absolute inset-0 z-30 shadow-2xl'

export function MaxButton({ maxed, onClick }) {
  const Ico = maxed ? Icon.Minimize : Icon.Maximize
  return (
    <button onClick={onClick} title={maxed ? 'Dock back (Esc)' : 'Maximize'}
      aria-label={maxed ? 'Dock back' : 'Maximize'}
      className="shrink-0 rounded p-1 text-muted transition hover:bg-surface2 hover:text-fg">
      <Ico size={14} />
    </button>
  )
}

// Panel is Card's look with a body that can be told to scroll. Card's body is a plain padded div,
// so a list inside it grows the page instead of scrolling in place — which in a three-column
// debugger means the toolbar you need scrolls off the top the moment something long arrives.
export function Panel({ id, title, action, className = '', bodyClass = 'p-3', children }) {
  const { maxed, toggle } = useMaximize(id)
  return (
    <div className={`flex min-h-0 flex-col rounded-xl border bg-surface ${maxed ? MAXED_CLS : className}`}>
      {(title || action || toggle) && (
        <div className="flex shrink-0 items-center justify-between gap-2 border-b px-3 py-2">
          <h3 className="min-w-0 truncate text-sm font-semibold text-fg">{title}</h3>
          <div className="flex shrink-0 items-center gap-2">
            {action}
            {toggle && <MaxButton maxed={maxed} onClick={toggle} />}
          </div>
        </div>
      )}
      <div className={`min-h-0 flex-1 ${bodyClass}`}>{children}</div>
    </div>
  )
}

// EventLog is the running commentary both pages keep: what the session did, and what the tool on
// the other end said about itself. In this particular pair of debuggers the interesting events are
// not all yours — the operator is managing a real cluster while you read, and gdb says what it
// thinks of the symbol table once, at load, where nobody reading a stack would see it.
const LOG_CLS = {
  error: 'text-danger',
  warn: 'text-warning',
  output: 'text-muted',
  info: 'text-muted',
}

export function EventLog({ lines, id = 'log', title = 'Session log' }) {
  const box = useRef(null)
  useEffect(() => { if (box.current) box.current.scrollTop = box.current.scrollHeight }, [lines.length])
  return (
    <Panel id={id} title={title} className="h-36 shrink-0" bodyClass="overflow-auto p-3">
      <div ref={box} className="space-y-0.5 font-mono text-[10px] leading-snug">
        {lines.length === 0 && <p className="text-muted">Nothing yet.</p>}
        {lines.map((ln, i) => (
          <div key={i} className={LOG_CLS[ln.kind] || 'text-muted'}>
            <span className="opacity-60">{new Date(ln.at).toLocaleTimeString()} </span>{ln.text}
          </div>
        ))}
      </div>
    </Panel>
  )
}
