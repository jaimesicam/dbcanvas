// Tooltip — the hover/focus explanation attached to a control.
//
// DBCanvas asks for a lot of decisions before anything is deployed: an OS family, a
// major and a minor version, whether to export a port, whether to mint a certificate,
// which PMM node monitors this cluster. The inline `hint` under a field says what the
// value *is*; there is rarely room under an input to also say what it is *for*, what
// happens if you leave it alone, and when you would want to change it. That is what
// these carry.
//
// Two mechanics matter here and both come from where they are used:
//
//   - The bubble is portalled to <body>. The properties panel is `overflow-auto` and
//     the canvas clips at its own bounds, so a tooltip rendered in place would be cut
//     off by its own container — which is exactly where the longest ones live.
//   - Position is computed from the trigger's rect at open time and flipped when the
//     preferred side does not fit. The panel can be docked (right edge), floated
//     anywhere, or scrolled halfway down; there is no static side that always works.
//
// A tooltip closes on scroll and on any pointerdown rather than trying to follow the
// page: the rect it was placed against is stale the moment either happens.
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from './Icons.jsx'

// Long enough that a pointer crossing a dense panel does not strobe every control it
// passes over, short enough to feel like the answer was already there.
const OPEN_DELAY = 140
const GAP = 8      // between the trigger and the bubble
const MARGIN = 6   // keeps the bubble off the viewport edge

// place picks a final top/left for a measured bubble. Vertical is a preference that
// flips when it does not fit; horizontal is centred on the trigger and then clamped,
// which is what keeps a tooltip on a field at the very right edge of a docked panel
// fully on screen. Exported because this flip-and-clamp is the only real logic in the
// file, and the one part a render check can reach.
export function place(rect, bubble, prefer) {
  const vw = window.innerWidth
  const vh = window.innerHeight
  const fitsAbove = rect.top - bubble.height - GAP >= MARGIN
  const fitsBelow = rect.bottom + bubble.height + GAP <= vh - MARGIN
  const above = prefer === 'top' ? fitsAbove || !fitsBelow : !(fitsBelow || !fitsAbove)
  let top = above ? rect.top - bubble.height - GAP : rect.bottom + GAP
  let left = rect.left + rect.width / 2 - bubble.width / 2
  left = Math.max(MARGIN, Math.min(left, vw - bubble.width - MARGIN))
  top = Math.max(MARGIN, Math.min(top, vh - bubble.height - MARGIN))
  return { top, left }
}

// `display` is a prop rather than something to pass through className because the two
// would be the same Tailwind specificity and which one won would come down to the order
// Tailwind happened to emit them in. It matters: the wrapper is inline-flex by default
// so a "?" sits on the label's baseline, but in a vertical list (the node palette, the
// context menu) an inline-level box picks up line-height gaps and breaks the spacing.
export function Tooltip({ content, children, placement = 'top', className = '', display = 'inline-flex' }) {
  const [rect, setRect] = useState(null)      // trigger rect while open; null = closed
  const [pos, setPos] = useState(null)        // final placement, once measured
  const wrapRef = useRef(null)
  const bubbleRef = useRef(null)
  const timer = useRef(null)
  const id = useId()

  const close = useCallback(() => {
    clearTimeout(timer.current)
    setRect(null)
    setPos(null)
  }, [])

  const open = useCallback((immediate) => {
    const el = wrapRef.current
    if (!el || !content) return
    clearTimeout(timer.current)
    const show = () => setRect(el.getBoundingClientRect())
    if (immediate) show()
    else timer.current = setTimeout(show, OPEN_DELAY)
  }, [content])

  // Measure the bubble once it exists, then place it. Two passes rather than an
  // estimate: the text wraps, so the height is not knowable in advance.
  useLayoutEffect(() => {
    if (!rect || !bubbleRef.current) return
    const b = bubbleRef.current.getBoundingClientRect()
    setPos(place(rect, b, placement))
  }, [rect, placement])

  // Anything that moves the trigger out from under the bubble dismisses it. Scroll is
  // captured because the panel it lives in scrolls, not the window.
  useEffect(() => {
    if (!rect) return
    const onKey = (e) => { if (e.key === 'Escape') close() }
    addEventListener('scroll', close, true)
    addEventListener('resize', close)
    addEventListener('pointerdown', close, true)
    addEventListener('keydown', onKey)
    return () => {
      removeEventListener('scroll', close, true)
      removeEventListener('resize', close)
      removeEventListener('pointerdown', close, true)
      removeEventListener('keydown', onKey)
    }
  }, [rect, close])

  useEffect(() => () => clearTimeout(timer.current), [])

  if (!content) return children

  return (
    <>
      <span
        ref={wrapRef}
        className={`${display} ${className}`}
        aria-describedby={rect ? id : undefined}
        onPointerEnter={(e) => open(e.pointerType === 'touch')}
        onPointerLeave={close}
        onFocusCapture={() => open(true)}
        onBlurCapture={close}
      >
        {children}
      </span>
      {rect && createPortal(
        <div
          ref={bubbleRef}
          id={id}
          role="tooltip"
          className="pointer-events-none fixed z-[100] max-w-[19rem] rounded-lg border bg-surface px-2.5 py-1.5 text-xs leading-relaxed text-fg shadow-xl"
          // Placed off-screen for the measuring pass so it is never seen mid-flight.
          style={pos ? { top: pos.top, left: pos.left } : { top: -9999, left: -9999 }}
        >
          {content}
        </div>,
        document.body,
      )}
    </>
  )
}

// Help is the common case: the small "?" beside a label that carries the explanation.
//
// It is a <button> because a tooltip nobody can reach from the keyboard helps only half
// the people using this, and a focusable element is the one thing every browser and
// screen reader agrees to announce an aria-describedby on. It swallows its own click:
// Field renders it inside a <label>, where an unhandled click would be forwarded to the
// input — toggling the very checkbox the reader was asking about.
export function Help({ text, size = 13, className = '' }) {
  if (!text) return null
  return (
    <Tooltip content={text}>
      <button
        type="button"
        aria-label="What is this?"
        onClick={(e) => { e.preventDefault(); e.stopPropagation() }}
        className={`inline-flex shrink-0 cursor-help items-center justify-center rounded-full text-muted/70 transition hover:text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-primary/40 ${className}`}
      >
        <Icon.Help size={size} />
      </button>
    </Tooltip>
  )
}

// Hint wraps arbitrary content (a value in a deployed node's panel, an icon button, a
// badge) so the whole thing is the trigger. Use it where there is no label to hang a
// Help next to.
export function Hint({ text, children, placement = 'top', className = '', display }) {
  if (!text) return children
  return <Tooltip content={text} placement={placement} className={className} display={display}>{children}</Tooltip>
}
