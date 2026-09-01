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

// uploadForm posts a multipart body with progress and cancellation. XHR rather
// than fetch: fetch reports nothing about the *request* body's progress, and a
// gigabyte-scale copy with no progress bar is indistinguishable from a hang.
//
// Two callbacks, because the wire has two phases the user experiences
// differently. onProgress covers the upload itself, which is cancellable and
// has a percentage. onSent fires when the last byte is away and the server
// starts extracting the tar into the container — from here there is no
// percentage to report and no safe way to cancel (see UploadDialog).
//
// Same error contract as request(): rejects with an Error carrying .status, and
// .aborted set when the caller cancelled.
function uploadForm(path, form, { onProgress, onSent, signal } = {}) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      const e = new Error('Cancelled'); e.aborted = true; return reject(e)
    }
    const xhr = new XMLHttpRequest()
    xhr.open('POST', path)
    xhr.withCredentials = true

    xhr.upload.onprogress = (ev) => {
      // ev.total is the whole multipart body, which is a little larger than the
      // files themselves (part headers and boundaries) — that is the honest
      // denominator for "how much is left to send".
      if (onProgress && ev.lengthComputable) onProgress(ev.loaded, ev.total)
    }
    xhr.upload.onload = () => { if (onSent) onSent() }

    const fail = (msg, status) => {
      const e = new Error(msg)
      if (status) e.status = status
      reject(e)
    }
    xhr.onload = () => {
      let data = null
      try { data = JSON.parse(xhr.responseText) } catch { /* non-JSON body */ }
      if (xhr.status >= 200 && xhr.status < 300) return resolve(data)
      fail((data && data.error) || `Request failed (${xhr.status})`, xhr.status)
    }
    xhr.onerror = () => fail('The connection to the server was lost.')
    xhr.ontimeout = () => fail('The upload timed out.')
    xhr.onabort = () => {
      const e = new Error('Cancelled'); e.aborted = true; reject(e)
    }
    signal?.addEventListener('abort', () => xhr.abort(), { once: true })
    xhr.send(form)
  })
}

// The destinations a dropped file may be copied to. The server enforces the
// same closed set (app/nodeupload.go) — this is only the menu.
export const NODE_UPLOAD_DESTS = ['/', '/home', '/root', '/tmp']

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
  // templateId seeds the new stack from a deployment template; the server reads
  // that template's design itself rather than taking a copy from here.
  create: (name, ttl, design, templateId) => request('POST', '/api/stacks', { name, ttl, design, templateId }),
  get: (id) => request('GET', `/api/stacks/${id}`),
  update: (id, name, design) => request('PUT', `/api/stacks/${id}`, { name, design }),
  remove: (id) => request('DELETE', `/api/stacks/${id}`),
  validate: (id) => request('POST', `/api/stacks/${id}/validate`),
  deploy: (id) => request('POST', `/api/stacks/${id}/deploy`),
  destroy: (id) => request('POST', `/api/stacks/${id}/destroy`),
  getNode: (id, nid) => request('GET', `/api/stacks/${id}/nodes/${nid}`),
  nodeAction: (id, nid, action) => request('POST', `/api/stacks/${id}/nodes/${nid}/${action}`),
  // The `ssh -L` line that tunnels this node's published ports to the operator's
  // machine. Only meaningful when system.sshForwarding.enabled (app/sshforward.go).
  nodeSSHForward: (id, nid) => request('GET', `/api/stacks/${id}/nodes/${nid}/sshforward`),
  // Copy host files into a running node. `dest` is one of NODE_UPLOAD_DESTS;
  // `files` is [{ path, file }] where path is relative to dest. The relative
  // path travels as the multipart field name — Go strips directories from the
  // filename parameter, which would flatten a dropped folder.
  //
  // opts.onProgress(sent, total) reports bytes on the wire, and opts.onSent()
  // fires when the last one leaves — see uploadForm for why both matter.
  // opts.signal aborts the transfer.
  nodeUpload: (id, nid, dest, files, opts = {}) => {
    const fd = new FormData()
    fd.append('dest', dest)
    for (const { path, file } of files) fd.append(path, file, path.split('/').pop())
    return uploadForm(`/api/stacks/${id}/nodes/${nid}/upload`, fd, opts)
  },
  // Stock Market Sim: check a manually-entered database connection before
  // deploying with it. Returns { ok, message } — message is the sim binary's
  // own one-line verdict, shown to the user verbatim.
  stocksimTest: (id, nid, conn) => request('POST', `/api/stacks/${id}/nodes/${nid}/stocksim/test`, conn),
  pmmCatalog: () => request('GET', '/api/catalog/pmm'),
  pxcCatalog: () => request('GET', '/api/catalog/pxc'),
  proxysqlCatalog: () => request('GET', '/api/catalog/proxysql'),
  valkeyCatalog: () => request('GET', '/api/catalog/valkey'),
  psCatalog: () => request('GET', '/api/catalog/ps'),
  orchestratorCatalog: () => request('GET', '/api/catalog/orchestrator'),
  mariadbCatalog: () => request('GET', '/api/catalog/mariadb'),
  mysqlceCatalog: () => request('GET', '/api/catalog/mysqlce'),
  psmdbCatalog: () => request('GET', '/api/catalog/psmdb'),
  ppgCatalog: () => request('GET', '/api/catalog/ppg'),
  spockCatalog: () => request('GET', '/api/catalog/spock'),
  imagesCatalog: () => request('GET', '/api/catalog/images'),
  pdpsCatalog: () => request('GET', '/api/catalog/pdps'),
  operatorsCatalog: () => request('GET', '/api/catalog/operators'),
  k3sCatalog: () => request('GET', '/api/catalog/k3s'),
}

// Deployment templates (app/templates.go). Ids are opaque strings: a decimal for a
// saved template, "builtin:<slug>" for one of the defaults that ship with the app.
export const templateApi = {
  list: () => request('GET', '/api/templates'),
  get: (id) => request('GET', `/api/templates/${encodeURIComponent(id)}`),
  create: (name, description, category, design) =>
    request('POST', '/api/templates', { name, description, category, design }),
  update: (id, name, description, category, design) =>
    request('PUT', `/api/templates/${encodeURIComponent(id)}`, { name, description, category, design }),
  remove: (id) => request('DELETE', `/api/templates/${encodeURIComponent(id)}`),
  share: (id, shared) => request('POST', `/api/templates/${encodeURIComponent(id)}/share`, { shared }),
  // The export endpoint sets Content-Disposition, so a plain same-origin link is
  // the whole download — no blob juggling, and the cookie rides along.
  exportUrl: (id) => `/api/templates/${encodeURIComponent(id)}/export`,
  // A template file is small (a design document), so it is read here and posted as
  // JSON rather than as multipart — the server validates it either way.
  importFile: async (file) => {
    let doc
    try {
      doc = JSON.parse(await file.text())
    } catch {
      throw new Error('That file is not valid JSON.')
    }
    return request('POST', '/api/templates/import', doc)
  },
}

// TEMPLATE_BUILTIN_PREFIX marks the templates that ship with the app — they can be
// applied and exported, but never renamed, edited or deleted (app/templates.go).
export const TEMPLATE_BUILTIN_PREFIX = 'builtin:'
export const isBuiltinTemplate = (id) => String(id || '').startsWith(TEMPLATE_BUILTIN_PREFIX)

// Node File Manager (app/nodefs.go). `nid` is the design node id. Everything is
// scoped to one node except transfer(), which names the other end, and the
// stack-level node list the second pane picks from.
export function fsApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}/fs`
  return {
    list: (path) => request('GET', `${base}/list?path=${encodeURIComponent(path)}`),
    identities: () => request('GET', `${base}/identities`),
    mkdir: (path) => request('POST', `${base}/mkdir`, { path }),
    remove: (paths, recursive) => request('POST', `${base}/delete`, { paths, recursive }),
    rename: (path, to) => request('POST', `${base}/rename`, { path, to }),
    chmod: (paths, mode, recursive) => request('POST', `${base}/chmod`, { paths, mode, recursive }),
    chown: (paths, owner, group, recursive) => request('POST', `${base}/chown`, { paths, owner, group, recursive }),
    transfer: (paths, toNodeId, toPath) => request('POST', `${base}/transfer`, { paths, toNodeId, toPath }),
    // Editing a text file in place. read() hands back the mode/uid/gid and the
    // mtime; write() echoes them so a save preserves ownership and can refuse
    // to clobber a file that changed underneath the editor.
    read: (path) => request('GET', `${base}/read?path=${encodeURIComponent(path)}`),
    write: (body) => request('POST', `${base}/write`, body),
    upload: (dest, files, opts) => {
      const fd = new FormData()
      fd.append('dest', dest)
      for (const { path, file } of files) fd.append(path, file, path.split('/').pop())
      return uploadForm(`${base}/upload`, fd, opts || {})
    },
    // A plain href the browser GETs directly, so the session cookie rides along
    // and the file streams to disk without passing through JS.
    downloadURL: (paths) => `${base}/download?` + paths.map((p) => `path=${encodeURIComponent(p)}`).join('&'),
  }
}

// The stack's running nodes, for the file manager's second pane.
export const fsNodes = (id) => request('GET', `/api/stacks/${id}/fs/nodes`)

// PMM node management. `nid` is the design node id.
export function pmmApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    certInfo: () => request('GET', `${base}/pmm/cert`),
    certGenerate: (value, unit) => request('POST', `${base}/pmm/cert`, { value, unit }),
  }
}

// OpenBao node management. OpenBao seals itself on every restart, so the manager polls the live
// seal state and can replay the stored unseal keys (the keys themselves never leave the server).
export function openbaoApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}/openbao`
  return {
    status: () => request('GET', `${base}/status`),
    unseal: () => request('POST', `${base}/unseal`),
  }
}

// SeaweedFS node management — read-only browsing of the node's buckets. `path` is a folder inside
// the bucket, `after` the previous page's last entry (the filer pages by name, not by offset).
export function seaweedApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}/seaweed`
  return {
    objects: (bucket, path = '', after = '') =>
      request('GET', `${base}/objects?bucket=${encodeURIComponent(bucket)}` +
        `&path=${encodeURIComponent(path)}&after=${encodeURIComponent(after)}`),
  }
}

// All-in-One node management. Every action execs the container's own `aioctl`
// (see app/aio_mgmt.go), so these buttons and the CLI an operator runs in the
// node's terminal are the same implementation. `sel` is an instance name, a
// group (cluster) name, or "all" — the selectors aioctl itself accepts.
export function aioApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}/aio`
  return {
    instances: () => request('GET', `${base}/instances`),
    start: (sel) => request('POST', `${base}/instances/${encodeURIComponent(sel)}/start`),
    stop: (sel) => request('POST', `${base}/instances/${encodeURIComponent(sel)}/stop`),
    restart: (sel) => request('POST', `${base}/instances/${encodeURIComponent(sel)}/restart`),
    logs: (inst) => request('GET', `${base}/instances/${encodeURIComponent(inst)}/logs`),
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

// PXC / MySQL replication cluster (frame) management. `fid` is the design frame id.
export function frameApi(id, fid) {
  const base = `/api/stacks/${id}/frames/${fid}`
  return {
    // pmmNodeId "" turns monitoring off; a node id registers the cluster with that PMM server.
    setMonitoring: (pmmNodeId) => request('POST', `${base}/pmm`, { pmmNodeId }),
    // orchestratorNodeId "" clears the link; a node id seeds/refreshes topology discovery
    // against that Orchestrator node. Works for both "pxc" and "mysql" frames.
    setOrchestrator: (orchestratorNodeId) => request('POST', `${base}/orchestrator`, { orchestratorNodeId }),
  }
}

// Standalone PostgreSQL node management. `nid` is the design node id.
export function pgApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    // Run an on-demand pgBackRest full backup.
    backup: () => request('POST', `${base}/pg/backup`),
    // Re-issue the node's Intranet-CA server cert (overwrites in place, no restart).
    // Works for standalone PostgreSQL, Patroni, repmgr and Spock members.
    certInfo: () => request('GET', `${base}/pg/cert`),
    certGenerate: (value, unit) => request('POST', `${base}/pg/cert`, { value, unit }),
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

// K3D cluster (frame) management. `fid` is the design frame id. A copyable admin kubeconfig, plus
// Kubernetes RBAC users (a client-cert `User` bound to a built-in ClusterRole) for RBAC testing.
export function k3dApi(id, fid) {
  const base = `/api/stacks/${id}/frames/${fid}/k3d`
  return {
    kubeconfig: () => request('GET', `${base}/kubeconfig`),
    users: () => request('GET', `${base}/users`),
    userCreate: (body) => request('POST', `${base}/users`, body),
    userDelete: (username) => request('POST', `${base}/users/delete`, { username }),
    userKubeconfig: (username) => request('GET', `${base}/users/${encodeURIComponent(username)}/kubeconfig`),
  }
}

// Per-node MongoDB management (`nid` is the design node id). Re-issue the node's
// Intranet-CA cert (overwrites /etc/mongo/certs in place, no mongod restart).
export function mongoNodeApi(id, nid) {
  const base = `/api/stacks/${id}/nodes/${nid}`
  return {
    certInfo: () => request('GET', `${base}/mongo/cert`),
    certGenerate: (value, unit) => request('POST', `${base}/mongo/cert`, { value, unit }),
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

// PRODUCT_OS_FAMILIES — the OS families DBCanvas installs database products on.
// `make images` also builds Debian bases (see images/build.sh), but those exist for
// the Linux Client jump box, where nothing is installed at all: no product's install
// path is exercised on Debian. So the forms that pick an OS straight from the generic
// images catalog (HAProxy, All in One) filter the catalog through this list, and
// validateStack in app/intranet.go rejects the same combination server-side.
export const PRODUCT_OS_FAMILIES = ['oraclelinux', 'ubuntu']
