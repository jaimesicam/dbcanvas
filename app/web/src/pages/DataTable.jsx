import { useMemo, useState } from 'react'
import { Card, Badge, Button, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'

const NAMES = [
  'Ada Lovelace', 'Alan Turing', 'Grace Hopper', 'Linus Torvalds', 'Margaret Hamilton',
  'Dennis Ritchie', 'Katherine Johnson', 'Ken Thompson', 'Barbara Liskov', 'Tim Berners-Lee',
  'Radia Perlman', 'Donald Knuth', 'Hedy Lamarr', 'John Carmack', 'Joan Clarke',
  'Guido van Rossum', 'Anita Borg', 'Brendan Eich',
]
const ROLES = ['admin', 'editor', 'viewer']
const STATUSES = ['approved', 'pending', 'disabled']
const STATUS_TONE = { approved: 'success', pending: 'warning', disabled: 'muted' }

const PEOPLE = NAMES.map((name, i) => {
  const handle = name.toLowerCase().replace(/[^a-z]+/g, '.')
  return {
    id: i + 1,
    name,
    email: `${handle}@dbcanvas.dev`,
    role: ROLES[i % ROLES.length],
    status: STATUSES[i % STATUSES.length],
    score: ((i * 37) % 100) + 1,
  }
})

const PAGE_SIZE = 7

export default function DataTable() {
  const [sort, setSort] = useState({ key: 'name', dir: 'asc' })
  const [search, setSearch] = useState('')
  const [role, setRole] = useState('all')
  const [selected, setSelected] = useState(() => new Set())
  const [page, setPage] = useState(1)

  const filtered = useMemo(() => {
    let rows = PEOPLE.filter((p) => {
      const q = search.toLowerCase()
      const match = p.name.toLowerCase().includes(q) || p.email.toLowerCase().includes(q)
      const roleOk = role === 'all' || p.role === role
      return match && roleOk
    })
    rows = [...rows].sort((a, b) => {
      const av = a[sort.key]
      const bv = b[sort.key]
      const cmp = typeof av === 'number' ? av - bv : String(av).localeCompare(String(bv))
      return sort.dir === 'asc' ? cmp : -cmp
    })
    return rows
  }, [search, role, sort])

  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE))
  const safePage = Math.min(page, pageCount)
  const pageRows = filtered.slice((safePage - 1) * PAGE_SIZE, safePage * PAGE_SIZE)

  function toggleSort(key) {
    setSort((s) => (s.key === key ? { key, dir: s.dir === 'asc' ? 'desc' : 'asc' } : { key, dir: 'asc' }))
  }

  function toggleRow(id) {
    setSelected((prev) => {
      const next = new Set(prev)
      next.has(id) ? next.delete(id) : next.add(id)
      return next
    })
  }

  const pageIds = pageRows.map((r) => r.id)
  const allOnPage = pageIds.length > 0 && pageIds.every((id) => selected.has(id))
  function toggleAllOnPage() {
    setSelected((prev) => {
      const next = new Set(prev)
      if (allOnPage) pageIds.forEach((id) => next.delete(id))
      else pageIds.forEach((id) => next.add(id))
      return next
    })
  }

  const SortHead = ({ k, children }) => (
    <th className="px-3 py-2 text-left">
      <button onClick={() => toggleSort(k)} className="inline-flex items-center gap-1 font-medium text-muted hover:text-fg">
        {children}
        {sort.key === k && <span className={`transition ${sort.dir === 'desc' ? 'rotate-180' : ''}`}><Icon.Chevron size={14} /></span>}
      </button>
    </th>
  )

  return (
    <Card
      title="People"
      subtitle={`${filtered.length} records`}
      action={
        <div className="flex items-center gap-2">
          <div className="relative">
            <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-muted"><Icon.Search size={15} /></span>
            <input
              value={search}
              onChange={(e) => {
                setSearch(e.target.value)
                setPage(1)
              }}
              placeholder="Search…"
              className={`${inputCls} w-44 pl-8`}
            />
          </div>
          <select
            value={role}
            onChange={(e) => {
              setRole(e.target.value)
              setPage(1)
            }}
            className={`${inputCls} w-32`}
          >
            <option value="all">All roles</option>
            {ROLES.map((r) => (
              <option key={r} value={r}>{r}</option>
            ))}
          </select>
        </div>
      }
    >
      {selected.size > 0 && (
        <div className="mb-3 flex items-center justify-between rounded-lg bg-primary/10 px-3 py-2 text-sm text-primary">
          <span>{selected.size} selected</span>
          <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>Clear</Button>
        </div>
      )}

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="border-b text-xs">
            <tr>
              <th className="w-10 px-3 py-2">
                <input type="checkbox" checked={allOnPage} onChange={toggleAllOnPage} className="h-4 w-4 accent-[var(--primary)]" />
              </th>
              <SortHead k="name">Name</SortHead>
              <SortHead k="role">Role</SortHead>
              <SortHead k="status">Status</SortHead>
              <SortHead k="score">Score</SortHead>
            </tr>
          </thead>
          <tbody>
            {pageRows.map((p) => (
              <tr key={p.id} className={`border-b transition hover:bg-surface2 ${selected.has(p.id) ? 'bg-primary/5' : ''}`}>
                <td className="px-3 py-2">
                  <input type="checkbox" checked={selected.has(p.id)} onChange={() => toggleRow(p.id)} className="h-4 w-4 accent-[var(--primary)]" />
                </td>
                <td className="px-3 py-2">
                  <div className="font-medium text-fg">{p.name}</div>
                  <div className="text-xs text-muted">{p.email}</div>
                </td>
                <td className="px-3 py-2 capitalize text-muted">{p.role}</td>
                <td className="px-3 py-2"><Badge tone={STATUS_TONE[p.status]}>{p.status}</Badge></td>
                <td className="px-3 py-2">
                  <div className="flex items-center gap-2">
                    <div className="h-1.5 w-20 overflow-hidden rounded-full bg-surface2">
                      <div className="h-full rounded-full bg-primary" style={{ width: `${p.score}%` }} />
                    </div>
                    <span className="text-xs text-muted">{p.score}</span>
                  </div>
                </td>
              </tr>
            ))}
            {pageRows.length === 0 && (
              <tr>
                <td colSpan={5} className="px-3 py-8 text-center text-muted">No matching records</td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="mt-3 flex items-center justify-between">
        <span className="text-xs text-muted">
          Page {safePage} of {pageCount}
        </span>
        <div className="flex gap-1">
          {Array.from({ length: pageCount }, (_, i) => i + 1).map((p) => (
            <button
              key={p}
              onClick={() => setPage(p)}
              className={`h-8 w-8 rounded-lg text-sm transition ${p === safePage ? 'bg-primary text-primary-fg' : 'text-muted hover:bg-surface2'}`}
            >
              {p}
            </button>
          ))}
        </div>
      </div>
    </Card>
  )
}
