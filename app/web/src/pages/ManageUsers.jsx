import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api.js'
import { useAuth } from '../auth/AuthProvider.jsx'
import { Card, Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'

const STATUS_TONE = { approved: 'success', pending: 'warning', rejected: 'danger', disabled: 'muted' }

function initials(name) {
  return (name || '?').trim().slice(0, 2).toUpperCase()
}

function fmtDate(iso) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d)) return '—'
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

export default function ManageUsers() {
  const { user: me } = useAuth()
  const [users, setUsers] = useState([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState(null)

  const load = useCallback(async () => {
    setError('')
    try {
      const list = await api.listUsers()
      setUsers(list)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  async function act(fn, id) {
    setBusyId(id)
    setError('')
    try {
      await fn()
      await load()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusyId(null)
    }
  }

  const setStatus = (id, action) => act(() => api.setUserStatus(id, action), id)
  const remove = (id) => act(() => api.deleteUser(id), id)

  const pendingCount = users.filter((u) => u.status === 'pending').length

  return (
    <Card
      title="Manage users"
      subtitle={`${pendingCount} awaiting approval`}
      action={
        <Button variant="outline" size="sm" onClick={load}>
          Refresh
        </Button>
      }
    >
      {error && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}

      {loading ? (
        <div className="py-10 text-center text-muted">Loading…</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b text-xs text-muted">
              <tr>
                <th className="px-3 py-2 text-left font-medium">User</th>
                <th className="px-3 py-2 text-left font-medium">Role</th>
                <th className="px-3 py-2 text-left font-medium">Status</th>
                <th className="px-3 py-2 text-left font-medium">Registered</th>
                <th className="px-3 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => {
                const isYou = u.id === me?.id
                const busy = busyId === u.id
                return (
                  <tr key={u.id} className={`border-b ${u.status === 'pending' ? 'bg-warning/5' : ''}`}>
                    <td className="px-3 py-2.5">
                      <div className="flex items-center gap-2.5">
                        <span className="flex h-8 w-8 items-center justify-center rounded-full bg-primary/15 text-xs font-semibold text-primary">
                          {initials(u.username)}
                        </span>
                        <span className="font-medium text-fg">
                          {u.username}
                          {isYou && <span className="ml-1 text-xs text-muted">(you)</span>}
                        </span>
                      </div>
                    </td>
                    <td className="px-3 py-2.5">
                      <Badge tone={u.role === 'admin' ? 'primary' : 'muted'}>{u.role}</Badge>
                    </td>
                    <td className="px-3 py-2.5">
                      <Badge tone={STATUS_TONE[u.status]}>{u.status}</Badge>
                    </td>
                    <td className="px-3 py-2.5 text-muted">{fmtDate(u.createdAt)}</td>
                    <td className="px-3 py-2.5">
                      <div className="flex items-center justify-end gap-1.5">
                        {u.status === 'pending' && (
                          <>
                            <Button size="sm" variant="primary" disabled={busy} onClick={() => setStatus(u.id, 'approve')}>
                              Approve
                            </Button>
                            <Button size="sm" variant="outline" disabled={busy} onClick={() => setStatus(u.id, 'reject')}>
                              Reject
                            </Button>
                          </>
                        )}
                        {u.status === 'approved' && !isYou && (
                          <Button size="sm" variant="outline" disabled={busy} onClick={() => setStatus(u.id, 'disable')}>
                            Disable
                          </Button>
                        )}
                        {(u.status === 'disabled' || u.status === 'rejected') && (
                          <Button size="sm" variant="outline" disabled={busy} onClick={() => setStatus(u.id, 'approve')}>
                            Re-approve
                          </Button>
                        )}
                        {!isYou && (
                          <Button size="sm" variant="ghost" disabled={busy} onClick={() => remove(u.id)} title="Delete user">
                            <Icon.Trash size={16} />
                          </Button>
                        )}
                      </div>
                    </td>
                  </tr>
                )
              })}
              {users.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-3 py-8 text-center text-muted">No users</td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}
