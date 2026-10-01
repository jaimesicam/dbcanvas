import { useCallback, useEffect, useRef, useState } from 'react'
import { Icon } from './Icons.jsx'
import { Button, inputCls } from './ui.jsx'

// Dialog — the app's own confirm and prompt, in place of window.confirm/prompt.
//
// The browser's dialogs are unstyled, block the page (a shared session's socket and
// timers included), cannot say which action is destructive, and look like a website
// asking rather than DBCanvas. These are drawn like every other dialog here.
//
//   const [dialog, ask] = useDialog()
//   …
//   if (await ask.confirm({ title: 'End the session?', body: '…', confirmLabel: 'End session', danger: true })) …
//   const name = await ask.prompt({ title: 'Save query', label: 'Name' })   // null when cancelled
//   …
//   return <>{…}{dialog}</>
//
// Enter confirms, Escape or a click outside cancels. The smoke checks refuse a
// window.confirm/alert/prompt anywhere under src/.

export function useDialog() {
  const [req, setReq] = useState(null) // { kind, opts, resolve }
  const open = useCallback((kind, opts) => new Promise((resolve) => setReq({ kind, opts, resolve })), [])
  const done = useCallback((value) => {
    setReq((r) => { r?.resolve(value); return null })
  }, [])
  const ask = useRef(null)
  if (!ask.current) ask.current = { confirm: (o) => open('confirm', o), prompt: (o) => open('prompt', o) }
  const element = req ? <DialogBox kind={req.kind} opts={req.opts} onDone={done} /> : null
  return [element, ask.current]
}

// Exported for the render checks.
export function DialogBox({ kind, opts, onDone }) {
  const {
    title, body, confirmLabel = kind === 'prompt' ? 'Save' : 'Confirm', cancelLabel = 'Cancel', danger = false,
    label, placeholder, defaultValue = '', maxLength = 200, icon,
  } = opts || {}
  const [value, setValue] = useState(defaultValue)
  const inputRef = useRef(null)
  const okRef = useRef(null)
  const cancel = () => onDone(kind === 'prompt' ? null : false)
  const ok = () => {
    if (kind === 'prompt') {
      const v = value.trim()
      if (!v) return
      onDone(v)
    } else onDone(true)
  }

  useEffect(() => {
    // Destructive confirms focus Cancel, so a stray Enter does not end anything.
    if (kind === 'prompt') inputRef.current?.select()
    else if (!danger) okRef.current?.focus()
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); cancel() } }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/40 p-4 animate-fade-in" onMouseDown={cancel}>
      <form
        role={kind === 'prompt' ? 'dialog' : 'alertdialog'} aria-modal="true" aria-label={title}
        className="w-full max-w-sm rounded-2xl border bg-surface p-5 shadow-xl"
        onMouseDown={(e) => e.stopPropagation()}
        onSubmit={(e) => { e.preventDefault(); ok() }}
      >
        <div className="flex items-start gap-3">
          <span className={`mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-full ${danger ? 'bg-danger/15 text-danger' : 'bg-primary/15 text-primary'}`}>
            {icon || (danger ? <Icon.StatusWarn size={16} /> : kind === 'prompt' ? <Icon.Pencil size={15} /> : <Icon.Help size={16} />)}
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="text-base font-semibold">{title}</h2>
            {body && <div className="mt-1 text-sm text-muted">{body}</div>}
            {kind === 'prompt' && (
              <label className="mt-3 block">
                {label && <span className="mb-1 block text-xs font-medium text-muted">{label}</span>}
                <input ref={inputRef} className={inputCls} value={value} onChange={(e) => setValue(e.target.value)}
                  placeholder={placeholder} maxLength={maxLength} autoFocus />
              </label>
            )}
          </div>
        </div>
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={cancel} autoFocus={danger}>{cancelLabel}</Button>
          <Button ref={okRef} type="submit" variant={danger ? 'danger' : 'primary'} disabled={kind === 'prompt' && !value.trim()}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </div>
  )
}
