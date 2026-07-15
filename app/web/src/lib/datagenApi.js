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
  // MongoDB / BSON generators
  objectid: 'ObjectId',
  int64: 'Int64 (long)',
  double: 'Double',
  decimal128: 'Decimal128',
  bson_date: 'Date',
  bson_ts: 'Timestamp (BSON)',
  binary: 'Binary',
  bson_uuid: 'UUID (binary)',
  regex: 'Regular expression',
  javascript: 'JavaScript',
  minkey: 'MinKey',
  maxkey: 'MaxKey',
  embedded: 'Embedded document',
  array: 'Array',
}

export const genLabel = (id) => GENERATOR_LABELS[id] || id

// BSON types offered when adding/retyping a MongoDB field, with a friendly label and the default
// generator each maps to (mirrors mongoInferGenerator on the server).
export const BSON_TYPES = [
  { udt: 'objectId', label: 'ObjectId', gen: 'objectid' },
  { udt: 'string', label: 'String', gen: 'text' },
  { udt: 'int', label: 'Int32', gen: 'randint' },
  { udt: 'long', label: 'Int64', gen: 'int64' },
  { udt: 'double', label: 'Double', gen: 'double' },
  { udt: 'decimal', label: 'Decimal128', gen: 'decimal128' },
  { udt: 'bool', label: 'Boolean', gen: 'bool' },
  { udt: 'date', label: 'Date', gen: 'bson_date' },
  { udt: 'timestamp', label: 'Timestamp', gen: 'bson_ts' },
  { udt: 'binData', label: 'Binary', gen: 'binary' },
  { udt: 'regex', label: 'Regex', gen: 'regex' },
  { udt: 'javascript', label: 'JavaScript', gen: 'javascript' },
  { udt: 'object', label: 'Embedded document', gen: 'embedded' },
  { udt: 'array', label: 'Array', gen: 'array' },
  { udt: 'minKey', label: 'MinKey', gen: 'minkey' },
  { udt: 'maxKey', label: 'MaxKey', gen: 'maxkey' },
]

// The combobox choices per BSON type (mirrors mongoGeneratorChoices on the server).
const MONGO_BASE = ['auto', 'skip', 'constant', 'null']
export const mongoChoices = (udt) => {
  const head = (...opts) => [...opts, ...MONGO_BASE]
  switch (udt) {
    case 'objectId': return head('objectid')
    case 'string': return [...head('text', 'lorem'), 'firstname', 'lastname', 'fullname', 'username', 'email', 'phone', 'city', 'country', 'company', 'jobtitle', 'address', 'url', 'uuid', 'enum']
    case 'double': return head('double', 'decimal128', 'randint')
    case 'int': return head('randint', 'seqint', 'int64')
    case 'long': return head('int64', 'randint', 'seqint')
    case 'decimal': return head('decimal128', 'double')
    case 'bool': return head('bool')
    case 'date': return head('bson_date')
    case 'timestamp': return head('bson_ts', 'bson_date')
    case 'binData': return head('binary', 'bson_uuid')
    case 'regex': return head('regex')
    case 'javascript': return head('javascript', 'text')
    case 'object': return head('embedded')
    case 'array': return head('array')
    case 'minKey': return head('minkey')
    case 'maxKey': return head('maxkey')
    default: return head('text')
  }
}

export const defaultGenFor = (udt) => (BSON_TYPES.find((t) => t.udt === udt) || {}).gen || 'text'
