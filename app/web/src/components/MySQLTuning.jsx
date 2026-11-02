// MySQLTuning — the InnoDB sizing and durability a MySQL-family server writes into its my.cnf at
// deploy (app/mysqltuning.go): innodb_buffer_pool_size, the redo log size, sync_binlog and
// innodb_flush_log_at_trx_commit. A cluster sets them for every member, a standalone server for
// itself. Empty leaves the server default.

import { Field, inputCls } from './ui'
import { HELP } from '../lib/help.js'

const SIZE = /^[0-9]+[KMGkmg]?$/

// A standalone server deploys through the frame type its provisioner builds
// (mysqlTuningNodeTypes), which is what picks the redo option.
const NODE_FRAME = { ps: 'mysql', mysqlce: 'mysqlcerepl', mariadb: 'mariadbrepl' }

// seriesOf reads the version a frame installs the same way mysqlFrameSeries does.
function seriesOf(f) {
  switch (NODE_FRAME[f.type] || f.type) {
    case 'pxc': return [f.pxcMajor, f.pxcVersion]
    case 'innodb': {
      const repo = f.pdpsRepo || ''
      const v = repo.replace(/^pdps-?/, '')
      const major = /^(9\.7|97)/.test(v) ? '9.7' : /^(8\.4|84)/.test(v) ? '8.4' : '8.0'
      return [major, v.split('.').length >= 3 ? v : '']
    }
    case 'mysqlcerepl': case 'mysqlceinnodb': return [f.mysqlceMajor || '8.4', f.mysqlceVersion]
    default: return [f.psMajor, f.psVersion]
  }
}

// redoOption is the option the redo log size is written under: innodb_redo_log_capacity from
// 8.0.30, innodb_log_file_size before that and on MariaDB. Mirrors mysqlRedoOption.
export function redoOption(f) {
  const type = NODE_FRAME[f.type] || f.type
  if (type === 'mariadbrepl' || type === 'mariadbgalera') return 'innodb_log_file_size'
  const [major, version] = seriesOf(f)
  if (major === '5.7') return 'innodb_log_file_size'
  if (major && major !== '8.0') return 'innodb_redo_log_capacity'
  const patch = parseInt((version || '').split('.')[2], 10)
  return Number.isNaN(patch) || patch >= 30 ? 'innodb_redo_log_capacity' : 'innodb_log_file_size'
}

const mono = (s) => <span className="font-mono">{s}</span>

// MySQLTuningFields draws the four fields for obj, a MySQL-family frame or standalone node.
export function MySQLTuningFields({ obj: f, onPatch, deployed, cluster }) {
  const redo = redoOption(f)
  const sizeHint = (v) => (v && !SIZE.test(v.trim()) ? 'Use a size such as 512M or 2G.' : '')
  const perFile = redo === 'innodb_log_file_size' && !/^maria/.test(NODE_FRAME[f.type] || f.type)
  return (
    <div className="space-y-2 rounded-lg bg-surface2 p-2">
      <span className="text-xs font-medium text-muted">InnoDB and binary log{cluster ? ' (every member)' : ''}</span>
      <div className="grid grid-cols-2 gap-2">
        <Field label="innodb_buffer_pool_size" help={HELP.bufferPoolSize} hint={sizeHint(f.innodbBufferPoolSize) || (deployed ? 'Locked.' : '')}>
          <input className={`${inputCls} font-mono`} placeholder="default" value={f.innodbBufferPoolSize || ''} disabled={deployed}
            onChange={(e) => onPatch({ innodbBufferPoolSize: e.target.value })} />
        </Field>
        <Field label={redo} help={HELP.redoLogSize} hint={sizeHint(f.innodbRedoLogCapacity) || (perFile ? 'Per redo file; there are two.' : '')}>
          <input className={`${inputCls} font-mono`} placeholder="default" value={f.innodbRedoLogCapacity || ''} disabled={deployed}
            onChange={(e) => onPatch({ innodbRedoLogCapacity: e.target.value })} />
        </Field>
        <Field label="sync_binlog" help={HELP.syncBinlog} hint={f.syncBinlog && !/^[0-9]+$/.test(String(f.syncBinlog).trim()) ? 'A whole number: 0, 1 or N.' : ''}>
          <input className={`${inputCls} font-mono`} placeholder="default (1)" inputMode="numeric" value={f.syncBinlog ?? ''} disabled={deployed}
            onChange={(e) => onPatch({ syncBinlog: e.target.value })} />
        </Field>
        <Field label="innodb_flush_log_at_trx_commit" help={HELP.flushLogAtTrxCommit}>
          <select className={`${inputCls} font-mono`} value={String(f.innodbFlushLogAtTrxCommit ?? '')} disabled={deployed}
            onChange={(e) => onPatch({ innodbFlushLogAtTrxCommit: e.target.value })}>
            <option value="">default (1)</option>
            <option value="1">1</option>
            <option value="2">2</option>
            <option value="0">0</option>
          </select>
        </Field>
      </div>
      <p className="text-[11px] text-muted">Written into {cluster ? "each member's" : "the server's"} {mono('my.cnf')}; empty keeps the server default.</p>
    </div>
  )
}
