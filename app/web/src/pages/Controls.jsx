import { useState } from 'react'
import { Card, Button, Badge, Toggle, Field, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'

export default function Controls() {
  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
      <ValidatedForm />
      <SlidersCard />
      <SegmentedCard />
      <TagInput />
      <ReorderList />
      <Dropzone />
      <AccordionCard />
      <AsyncStepper />
      <RatingTooltip />
    </div>
  )
}

function ValidatedForm() {
  const [email, setEmail] = useState('')
  const valid = /^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)
  const touched = email.length > 0
  return (
    <Card title="Inline validation">
      <div className="space-y-3">
        <Field label="Email" hint={touched && !valid ? 'Enter a valid email address.' : 'We never share it.'}>
          <input
            className={`${inputCls} ${touched ? (valid ? 'border-success focus:ring-success/30' : 'border-danger focus:ring-danger/30') : ''}`}
            placeholder="you@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </Field>
        <Button disabled={!valid} className="w-full">
          {valid ? 'Looks good' : 'Continue'}
        </Button>
      </div>
    </Card>
  )
}

function SlidersCard() {
  const [scale, setScale] = useState(100)
  const [opacity, setOpacity] = useState(100)
  return (
    <Card title="Range sliders" subtitle="Live preview">
      <div className="space-y-4">
        <div className="flex items-center justify-center rounded-lg bg-surface2 py-6">
          <div
            className="h-16 w-16 rounded-xl bg-primary transition-transform"
            style={{ transform: `scale(${scale / 100})`, opacity: opacity / 100 }}
          />
        </div>
        <label className="block text-xs text-muted">
          Scale {scale}%
          <input type="range" min="40" max="150" value={scale} onChange={(e) => setScale(+e.target.value)} className="mt-1 w-full accent-[var(--primary)]" />
        </label>
        <label className="block text-xs text-muted">
          Opacity {opacity}%
          <input type="range" min="10" max="100" value={opacity} onChange={(e) => setOpacity(+e.target.value)} className="mt-1 w-full accent-[var(--primary)]" />
        </label>
      </div>
    </Card>
  )
}

function SegmentedCard() {
  const [seg, setSeg] = useState('day')
  const [checks, setChecks] = useState({ a: true, b: false, c: true })
  const [on, setOn] = useState(true)
  const segs = ['day', 'week', 'month']
  return (
    <Card title="Segmented & checkboxes">
      <div className="space-y-4">
        <div className="grid grid-cols-3 gap-1 rounded-lg bg-surface2 p-1">
          {segs.map((s) => (
            <button
              key={s}
              onClick={() => setSeg(s)}
              className={`rounded-md py-1.5 text-sm capitalize transition ${seg === s ? 'bg-surface text-fg shadow' : 'text-muted'}`}
            >
              {s}
            </button>
          ))}
        </div>
        <div className="space-y-2">
          {['a', 'b', 'c'].map((k) => (
            <label key={k} className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={checks[k]}
                onChange={(e) => setChecks({ ...checks, [k]: e.target.checked })}
                className="h-4 w-4 accent-[var(--primary)]"
              />
              Option {k.toUpperCase()}
            </label>
          ))}
        </div>
        <Toggle checked={on} onChange={setOn} label="Notifications" />
      </div>
    </Card>
  )
}

function TagInput() {
  const [tags, setTags] = useState(['react', 'go', 'sqlite'])
  const [val, setVal] = useState('')
  function onKey(e) {
    if (e.key === 'Enter' && val.trim()) {
      e.preventDefault()
      if (!tags.includes(val.trim())) setTags([...tags, val.trim()])
      setVal('')
    } else if (e.key === 'Backspace' && !val && tags.length) {
      setTags(tags.slice(0, -1))
    }
  }
  return (
    <Card title="Tag input" subtitle="Enter to add · Backspace to remove">
      <div className="flex flex-wrap items-center gap-1.5 rounded-lg border bg-bg px-2 py-2">
        {tags.map((t) => (
          <span key={t} className="inline-flex items-center gap-1 rounded-md bg-primary/15 px-2 py-0.5 text-xs text-primary">
            {t}
            <button onClick={() => setTags(tags.filter((x) => x !== t))} className="hover:text-fg">✕</button>
          </span>
        ))}
        <input
          value={val}
          onChange={(e) => setVal(e.target.value)}
          onKeyDown={onKey}
          placeholder="Add tag…"
          className="min-w-[80px] flex-1 bg-transparent text-sm outline-none placeholder:text-muted"
        />
      </div>
    </Card>
  )
}

function ReorderList() {
  const [items, setItems] = useState(['Design', 'Develop', 'Review', 'Ship'])
  const [drag, setDrag] = useState(-1)
  function onDrop(i) {
    if (drag < 0 || drag === i) return
    const next = [...items]
    const [moved] = next.splice(drag, 1)
    next.splice(i, 0, moved)
    setItems(next)
    setDrag(-1)
  }
  return (
    <Card title="Drag to reorder">
      <ul className="space-y-2">
        {items.map((it, i) => (
          <li
            key={it}
            draggable
            onDragStart={() => setDrag(i)}
            onDragOver={(e) => e.preventDefault()}
            onDrop={() => onDrop(i)}
            className={`flex cursor-grab items-center gap-2 rounded-lg border bg-bg px-3 py-2 text-sm transition active:cursor-grabbing ${drag === i ? 'opacity-40' : ''}`}
          >
            <span className="text-muted"><Icon.Drag size={16} /></span>
            {it}
          </li>
        ))}
      </ul>
    </Card>
  )
}

function Dropzone() {
  const [over, setOver] = useState(false)
  const [files, setFiles] = useState([])
  return (
    <Card title="File dropzone" subtitle="Nothing uploads">
      <div
        onDragOver={(e) => {
          e.preventDefault()
          setOver(true)
        }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => {
          e.preventDefault()
          setOver(false)
          setFiles(Array.from(e.dataTransfer.files).map((f) => f.name))
        }}
        className={`flex h-28 flex-col items-center justify-center rounded-lg border-2 border-dashed text-sm transition ${
          over ? 'border-primary bg-primary/10 text-primary' : 'text-muted'
        }`}
      >
        <Icon.Plus size={20} />
        Drop files here
      </div>
      {files.length > 0 && (
        <ul className="mt-3 space-y-1 text-xs text-muted">
          {files.map((f, i) => (
            <li key={i} className="truncate">• {f}</li>
          ))}
        </ul>
      )}
    </Card>
  )
}

function AccordionCard() {
  const [open, setOpen] = useState(0)
  const items = [
    { q: 'What is this lab?', a: 'A from-scratch showcase of UI interactions built without component libraries.' },
    { q: 'Does it store data?', a: 'Only users and sessions, in SQLite. The widgets use local synthetic data.' },
    { q: 'How are themes done?', a: 'CSS variables redefined per [data-theme], surfaced to Tailwind via @theme inline.' },
  ]
  return (
    <Card title="Accordion">
      <div className="space-y-2">
        {items.map((it, i) => {
          const on = open === i
          return (
            <div key={i} className="overflow-hidden rounded-lg border">
              <button
                onClick={() => setOpen(on ? -1 : i)}
                className="flex w-full items-center justify-between px-3 py-2 text-sm font-medium hover:bg-surface2"
              >
                {it.q}
                <span className={`transition-transform ${on ? 'rotate-180' : ''}`}><Icon.Chevron size={16} /></span>
              </button>
              <div className={`grid transition-all duration-300 ${on ? 'grid-rows-[1fr]' : 'grid-rows-[0fr]'}`}>
                <div className="overflow-hidden">
                  <p className="px-3 pb-3 text-sm text-muted">{it.a}</p>
                </div>
              </div>
            </div>
          )
        })}
      </div>
    </Card>
  )
}

function AsyncStepper() {
  const [count, setCount] = useState(1)
  const [busy, setBusy] = useState(false)
  function submit() {
    setBusy(true)
    setTimeout(() => setBusy(false), 1400)
  }
  return (
    <Card title="Stepper & async button">
      <div className="space-y-4">
        <div className="flex items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => setCount((c) => Math.max(0, c - 1))}>−</Button>
          <span className="w-10 text-center text-lg font-semibold">{count}</span>
          <Button variant="outline" size="sm" onClick={() => setCount((c) => c + 1)}>+</Button>
        </div>
        <Button className="w-full" disabled={busy} onClick={submit}>
          {busy ? (
            <>
              <span className="h-4 w-4 animate-spin rounded-full border-2 border-primary-fg/40 border-t-primary-fg" />
              Processing…
            </>
          ) : (
            'Submit order'
          )}
        </Button>
      </div>
    </Card>
  )
}

function RatingTooltip() {
  const [rating, setRating] = useState(3)
  const [hover, setHover] = useState(0)
  return (
    <Card title="Rating & tooltip">
      <div className="space-y-4">
        <div className="flex items-center gap-1">
          {[1, 2, 3, 4, 5].map((n) => (
            <button
              key={n}
              onMouseEnter={() => setHover(n)}
              onMouseLeave={() => setHover(0)}
              onClick={() => setRating(n)}
              className={`text-2xl transition ${(hover || rating) >= n ? 'text-warning' : 'text-surface2'}`}
            >
              ★
            </button>
          ))}
          <span className="ml-2 text-sm text-muted">{hover || rating}/5</span>
        </div>
        <div className="group relative inline-block">
          <Button variant="outline" size="sm">Hover me</Button>
          <span className="pointer-events-none absolute -top-9 left-1/2 -translate-x-1/2 whitespace-nowrap rounded-md bg-fg px-2 py-1 text-xs text-bg opacity-0 transition group-hover:opacity-100">
            Pure-CSS tooltip
          </span>
        </div>
      </div>
    </Card>
  )
}
