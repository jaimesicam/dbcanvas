// Reusable Tailwind/theme primitives.
import { useEffect, useState } from 'react'

export const inputCls =
  'w-full rounded-lg border bg-bg px-3 py-2 text-sm text-fg outline-none ' +
  'transition focus:ring-2 focus:ring-primary/30 focus:border-primary placeholder:text-muted'

export function Card({ title, subtitle, action, className = '', children }) {
  const hasHeader = title || subtitle || action
  return (
    <div className={`rounded-xl border bg-surface ${className}`}>
      {hasHeader && (
        <div className="flex items-start justify-between gap-3 border-b px-4 py-3">
          <div>
            {title && <h3 className="text-sm font-semibold text-fg">{title}</h3>}
            {subtitle && <p className="text-xs text-muted">{subtitle}</p>}
          </div>
          {action}
        </div>
      )}
      <div className="p-4">{children}</div>
    </div>
  )
}

const BTN_VARIANTS = {
  primary: 'bg-primary text-primary-fg hover:opacity-90',
  ghost: 'text-fg hover:bg-surface2',
  outline: 'border text-fg hover:bg-surface2',
  danger: 'bg-danger text-white hover:opacity-90',
  subtle: 'bg-surface2 text-fg hover:opacity-80',
}
const BTN_SIZES = {
  sm: 'text-xs px-2.5 py-1.5 gap-1',
  md: 'text-sm px-3.5 py-2 gap-1.5',
  lg: 'text-base px-5 py-2.5 gap-2',
}

export function Button({
  variant = 'primary',
  size = 'md',
  className = '',
  children,
  ...rest
}) {
  return (
    <button
      className={`inline-flex items-center justify-center rounded-lg font-medium transition ` +
        `active:scale-[.97] disabled:opacity-50 disabled:pointer-events-none ` +
        `${BTN_VARIANTS[variant] || BTN_VARIANTS.primary} ${BTN_SIZES[size] || BTN_SIZES.md} ${className}`}
      {...rest}
    >
      {children}
    </button>
  )
}

const BADGE_TONES = {
  muted: 'bg-muted/15 text-muted',
  primary: 'bg-primary/15 text-primary',
  success: 'bg-success/15 text-success',
  warning: 'bg-warning/15 text-warning',
  danger: 'bg-danger/15 text-danger',
}

export function Badge({ tone = 'muted', children }) {
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${BADGE_TONES[tone] || BADGE_TONES.muted}`}>
      {children}
    </span>
  )
}

export function Toggle({ checked, onChange, label }) {
  return (
    <label className="inline-flex items-center gap-2 cursor-pointer select-none">
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        onClick={() => onChange(!checked)}
        className={`relative h-6 w-11 rounded-full transition ${checked ? 'bg-primary' : 'bg-surface2'}`}
      >
        <span
          className={`absolute top-0.5 h-5 w-5 rounded-full bg-white shadow transition-all ${checked ? 'left-[22px]' : 'left-0.5'}`}
        />
      </button>
      {label && <span className="text-sm text-fg">{label}</span>}
    </label>
  )
}

// ConfirmButton requires a second click (within a few seconds) to fire its
// action — an in-app replacement for window.confirm().
export function ConfirmButton({ onConfirm, children, confirmLabel = 'Confirm?', ...props }) {
  const [armed, setArmed] = useState(false)
  useEffect(() => {
    if (!armed) return
    const t = setTimeout(() => setArmed(false), 2500)
    return () => clearTimeout(t)
  }, [armed])
  return (
    <Button
      {...props}
      variant={armed ? 'danger' : props.variant}
      onClick={(e) => {
        e.stopPropagation()
        if (armed) { setArmed(false); onConfirm() } else { setArmed(true) }
      }}
    >
      {armed ? confirmLabel : children}
    </Button>
  )
}

export function Field({ label, hint, children }) {
  return (
    <label className="block space-y-1">
      {label && <span className="block text-xs font-medium text-muted">{label}</span>}
      {children}
      {hint && <span className="block text-xs text-muted">{hint}</span>}
    </label>
  )
}
