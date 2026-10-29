// FileTools — the offline MySQL file readers a node can have installed: innodump, binlogsum and
// gcache-inspector. The Linux Client offers them (app/linuxclient_files.go), and so does every
// MySQL-family server (app/dbfiletools.go); this is the one block of checkboxes both draw.

const mono = (s) => <span className="font-mono">{s}</span>

export const FILE_TOOLS = {
  innodump: {
    label: 'Install innodump',
    note: <>Writes a table's definition and rows out of its {mono('.ibd')}, {mono('.MYD')} or MyRocks files, or a whole datadir, with no server running.</>,
  },
  binlogsum: {
    label: 'Install binlogsum',
    note: <>Summarizes a binary log from {mono('mysqlbinlog')}'s output: event counts, GTID ranges, the busiest tables and writes over time.</>,
  },
  gcache: {
    label: 'Install gcache-inspector',
    note: <>Summarizes a Galera node's {mono('galera.cache')} and decodes its write-sets into rows.</>,
  },
}

// FileToolChecks draws one checkbox per entry of fields ([{ key, tool }], tool a FILE_TOOLS id)
// against obj, and calls onPatch with the change. footer is the line shown once any is ticked.
export function FileToolChecks({ obj, fields, onPatch, disabled, footer }) {
  const any = fields.some((f) => obj[f.key])
  return (
    <div className="space-y-2 rounded-lg bg-surface2 p-2">
      <span className="text-xs font-medium text-muted">MySQL file tools</span>
      {fields.map(({ key, tool }) => (
        <label key={key} className="flex items-start gap-2 text-sm">
          <input type="checkbox" className="mt-1" disabled={disabled} checked={!!obj[key]}
            onChange={(e) => onPatch({ [key]: e.target.checked })} />
          <span className="min-w-0 flex-1">
            {FILE_TOOLS[tool].label}
            <span className="block text-xs text-muted">{FILE_TOOLS[tool].note}</span>
          </span>
        </label>
      ))}
      {any && <p className="text-[11px] text-muted">{footer}</p>}
    </div>
  )
}

// DBFileToolChecks is the block on a MySQL-family server or cluster: innodump and binlogsum on all
// of them, gcache-inspector on the Galera kinds. Left editable once deployed — the tools are
// installed at the end of every deploy, so ticking one and redeploying adds it to a running server.
export function DBFileToolChecks({ obj, onPatch, galera, cluster }) {
  const fields = [{ key: 'installInnodump', tool: 'innodump' }, { key: 'installBinlogsum', tool: 'binlogsum' }]
  if (galera) fields.push({ key: 'installGcacheInspector', tool: 'gcache' })
  return (
    <FileToolChecks obj={obj} fields={fields} onPatch={onPatch}
      footer={<>The latest GitHub release, checksum-verified, in {mono('/usr/local/bin')} on {cluster ? 'every data member' : 'the server'} once it is running. Redeploy to add one to a running {cluster ? 'cluster' : 'server'}.</>} />
  )
}
