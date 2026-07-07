// Stack Designer API wrapper. Same conventions as lib/api.js: same-origin JSON,
// cookies ride along, throws Error with .status on non-2xx.

async function request(method, path, body) {
  const opts = {
    method,
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
  }
  if (body !== undefined) opts.body = JSON.stringify(body)

  const res = await fetch(path, opts)
  let data = null
  const text = await res.text()
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = null
    }
  }
  if (!res.ok) {
    const msg = (data && data.error) || `Request failed (${res.status})`
    const err = new Error(msg)
    err.status = res.status
    throw err
  }
  return data
}

export const TTL_OPTIONS = [
  { id: '2h', label: '2 hours' },
  { id: '4h', label: '4 hours' },
  { id: '8h', label: '8 hours' },
  { id: '24h', label: '24 hours' },
  { id: '2w', label: '2 weeks' },
  { id: 'infinity', label: 'Infinity' },
]

export const stackApi = {
  list: () => request('GET', '/api/stacks'),
  create: (name, ttl, design) => request('POST', '/api/stacks', { name, ttl, design }),
  get: (id) => request('GET', `/api/stacks/${id}`),
  update: (id, name, design) => request('PUT', `/api/stacks/${id}`, { name, design }),
  remove: (id) => request('DELETE', `/api/stacks/${id}`),
  validate: (id) => request('POST', `/api/stacks/${id}/validate`),
  deploy: (id) => request('POST', `/api/stacks/${id}/deploy`),
  destroy: (id) => request('POST', `/api/stacks/${id}/destroy`),
  getNode: (id, nid) => request('GET', `/api/stacks/${id}/nodes/${nid}`),
  nodeAction: (id, nid, action) => request('POST', `/api/stacks/${id}/nodes/${nid}/${action}`),
  pmmCatalog: () => request('GET', '/api/catalog/pmm'),
  pxcCatalog: () => request('GET', '/api/catalog/pxc'),
  proxysqlCatalog: () => request('GET', '/api/catalog/proxysql'),
  psCatalog: () => request('GET', '/api/catalog/ps'),
  psmdbCatalog: () => request('GET', '/api/catalog/psmdb'),
  ppgCatalog: () => request('GET', '/api/catalog/ppg'),
  imagesCatalog: () => request('GET', '/api/catalog/images'),
  pdpsCatalog: () => request('GET', '/api/catalog/pdps'),
}

// PMM node management. `nid` is the design node id.
export function pmmApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    certInfo: () => request('GET', `${base}/pmm/cert`),
    certGenerate: (value, unit) => request('POST', `${base}/pmm/cert`, { value, unit }),
  }
}

// PXC node management.
export function pxcApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    certInfo: () => request('GET', `${base}/pxc/cert`),
    certGenerate: (value, unit) => request('POST', `${base}/pxc/cert`, { value, unit }),
  }
}

// PXC cluster (frame) management. `fid` is the design frame id.
export function frameApi(id, fid) {
  const base = `/api/stacks/${id}/frames/${fid}`
  return {
    // pmmNodeId "" turns monitoring off; a node id registers the cluster with that PMM server.
    setMonitoring: (pmmNodeId) => request('POST', `${base}/pmm`, { pmmNodeId }),
  }
}

// Standalone PostgreSQL node management. `nid` is the design node id.
export function pgApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    // Run an on-demand pgBackRest full backup.
    backup: () => request('POST', `${base}/pg/backup`),
  }
}

// PS MongoDB cluster/replica-set (frame) management. `fid` is the design frame id.
export function mongoApi(id, fid) {
  const base = `/api/stacks/${id}/frames/${fid}`
  return {
    // Run an on-demand Percona Backup for MongoDB (PBM) backup.
    pbmBackup: () => request('POST', `${base}/pbm/backup`),
  }
}

// repmgr cluster (frame) management. `fid` is the design frame id.
export function repmgrApi(id, fid) {
  const base = `/api/stacks/${id}/frames/${fid}`
  return {
    // Run an on-demand Barman cloud backup on the current primary.
    backup: () => request('POST', `${base}/barman/backup`),
  }
}

// Patroni cluster (frame) management. `fid` is the design frame id.
export function patroniApi(id, fid) {
  const base = `/api/stacks/${id}/frames/${fid}`
  return {
    // Run an on-demand pgBackRest full backup on the current leader.
    backup: () => request('POST', `${base}/patroni/backup`),
  }
}

// On-node diagnostic captures (pg_gather for PostgreSQL, pt-stalk for MySQL family).
// `nid` is the design node id. The *DownloadURL helpers return a plain href (the browser
// GETs it directly, sending the session cookie, so the file downloads).
export function diagApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    pgGatherStatus: () => request('GET', `${base}/pggather`),
    pgGatherStart: (database) => request('POST', `${base}/pggather`, { database }),
    pgGatherDownloadURL: () => `${base}/pggather/download`,
    ptStalkStatus: () => request('GET', `${base}/ptstalk`),
    ptStalkStart: () => request('POST', `${base}/ptstalk`),
    ptStalkDownloadURL: () => `${base}/ptstalk/download`,
  }
}

// Samba AD DC node management. `nid` is the design node id. The *URL helpers return a plain
// href the browser GETs directly (session cookie rides along) to download the file.
export function sambaApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}/samba`
  return {
    users: () => request('GET', `${base}/users`),
    userCreate: (body) => request('POST', `${base}/users`, body),
    userUpdate: (body) => request('POST', `${base}/users/update`, body),
    userPassword: (username, password) => request('POST', `${base}/users/password`, { username, password }),
    userDelete: (username) => request('POST', `${base}/users/delete`, { username }),
    groups: () => request('GET', `${base}/groups`),
    groupCreate: (group) => request('POST', `${base}/groups`, { group }),
    groupMembers: (group, uids) => request('POST', `${base}/groups/members`, { group, uids }),
    groupDelete: (group) => request('POST', `${base}/groups/delete`, { group }),
    krb5URL: () => `${base}/krb5`,
    targets: () => request('GET', `${base}/targets`),
    principals: () => request('GET', `${base}/principals`),
    principalCreate: (service, fqdn) => request('POST', `${base}/principals`, { service, fqdn }),
    keytabURL: (principal) => `${base}/keytab?principal=${encodeURIComponent(principal)}`,
    certGenerate: (value, unit) => request('POST', `${base}/cert`, { value, unit }),
  }
}

// Intranet node management (Phase 3). `nid` is the design node id.
export function intranetApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    emailList: () => request('GET', `${base}/email/users`),
    emailAdd: (username, password) => request('POST', `${base}/email/users`, { username, password }),
    emailPassword: (username, password) => request('POST', `${base}/email/users/password`, { username, password }),
    emailDelete: (username) => request('POST', `${base}/email/users/delete`, { username }),

    ldapUsers: () => request('GET', `${base}/ldap/users`),
    ldapUserCreate: (body) => request('POST', `${base}/ldap/users`, body),
    ldapUserUpdate: (body) => request('POST', `${base}/ldap/users/update`, body),
    ldapUserPassword: (uid, password) => request('POST', `${base}/ldap/users/password`, { uid, password }),
    ldapUserDelete: (uid) => request('POST', `${base}/ldap/users/delete`, { uid }),

    ldapGroups: () => request('GET', `${base}/ldap/groups`),
    ldapGroupCreate: (cn) => request('POST', `${base}/ldap/groups`, { cn }),
    ldapGroupMembers: (cn, uids) => request('POST', `${base}/ldap/groups/members`, { cn, uids }),
    ldapGroupDelete: (cn) => request('POST', `${base}/ldap/groups/delete`, { cn }),

    certInfo: () => request('GET', `${base}/cert`),
    certGenerate: (value, unit) => request('POST', `${base}/cert`, { value, unit }),

    dbCertList: () => request('GET', `${base}/dbcerts`),
    dbCertGenerate: (username, value, unit) => request('POST', `${base}/dbcerts`, { username, value, unit }),
    dbCertGet: (username) => request('GET', `${base}/dbcerts/${encodeURIComponent(username)}`),
    dbCertDelete: (username) => request('POST', `${base}/dbcerts/delete`, { username }),
  }
}

export const DEPLOY_TONE = {
  pending: 'muted',
  provisioning: 'warning',
  running: 'success',
  stopped: 'muted',
  error: 'danger',
}
