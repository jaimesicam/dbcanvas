// Data Generator API wrapper. Same conventions as lib/stackApi.js: same-origin JSON,
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

const base = (sid, nid) => `/api/datagen/stacks/${sid}/nodes/${nid}`

export const datagenApi = {
  connections: () => request('GET', '/api/datagen/connections'),
  databases: (sid, nid) => request('GET', `${base(sid, nid)}/databases`),
  tables: (sid, nid, db) => request('GET', `${base(sid, nid)}/tables?db=${encodeURIComponent(db)}`),
  columns: (sid, nid, db, schema, table) =>
    request('GET', `${base(sid, nid)}/columns?db=${encodeURIComponent(db)}&schema=${encodeURIComponent(schema)}&table=${encodeURIComponent(table)}`),
  preview: (sid, nid, cfg) => request('POST', `${base(sid, nid)}/preview`, cfg),
  generate: (sid, nid, cfg) => request('POST', `${base(sid, nid)}/generate`, cfg),
  job: (jobId) => request('GET', `/api/datagen/jobs/${jobId}`),
  cancel: (jobId) => request('POST', `/api/datagen/jobs/${jobId}/cancel`),
}

// Human labels for the generator combobox options.
export const GENERATOR_LABELS = {
  auto: 'Auto-detect',
  skip: 'Skip column',
  default: 'Use database default',
  null: 'NULL',
  constant: 'Constant value',
  seqint: 'Sequential integer',
  randint: 'Random integer',
  randbigint: 'Random bigint',
  decimal: 'Random decimal',
  bool: 'Random boolean',
  uuid: 'UUID',
  firstname: 'First name',
  lastname: 'Last name',
  fullname: 'Full name',
  username: 'Username',
  email: 'Email address',
  phone: 'Phone number',
  address: 'Address',
  city: 'City',
  country: 'Country',
  company: 'Company',
  jobtitle: 'Job title',
  url: 'URL',
  ipaddr: 'IP address',
  macaddr: 'MAC address',
  date: 'Date',
  timestamp: 'Timestamp',
  ts_timestamp: 'Time-series timestamp',
  json_object: 'JSON object',
  text: 'Random text',
  lorem: 'Lorem ipsum',
  enum: 'Enum-like value',
  fk: 'Foreign key sampler',
  pgvector: 'pgvector embedding',
  ts_metric: 'TimescaleDB metric',
  ts_device: 'Device/sensor ID',
}

export const genLabel = (id) => GENERATOR_LABELS[id] || id
