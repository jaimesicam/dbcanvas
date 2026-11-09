import { Icon } from './Icons.jsx'
import { CopyButton as CopyBtn } from './Secret.jsx'
import { Help } from './Tooltip.jsx'
import { HELP } from '../lib/help.js'

// BigHoleLink is the way into a deployed Big Hole, and it is NOT SimDashboardLink.
//
// The app needs a secure context — HTTPS or localhost — for the private on-disk
// storage and workers it decodes a capture into. Opened as http://<host>:<port> it
// cannot keep a capture and fails partway through ingest, which reads like a broken
// decoder rather than a deployment mistake. SimDashboardLink builds its URL from
// location.hostname, which is right for a dashboard and wrong here: on a DBCanvas
// browsed at http://192.168.x.y:8080 it would hand over the one address that does
// not work.
//
// So the link is always localhost, and when this browser is somewhere else the row
// says so and gives the tunnel that fixes it.
export function BigHoleLink({ port }) {
  if (!port) return null
  const onLocalhost = typeof location === 'undefined' ||
    location.hostname === 'localhost' || location.hostname === '127.0.0.1'
  const url = `http://localhost:${port}/`
  const tunnel = `ssh -N -L ${port}:127.0.0.1:${port} <you>@<docker-host>`
  return (
    <div className="space-y-1.5">
      <a href={url} target="_blank" rel="noreferrer"
        className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
        <Icon.External size={15} /> Open Big Hole
      </a>
      {!onLocalhost && (
        <div className="space-y-1 rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          <div>
            You are not browsing DBCanvas on localhost, and this viewer only works there — a
            browser gives a page the on-disk storage it decodes a capture into over HTTPS or
            localhost, nowhere else. Forward the port and open it as localhost:
          </div>
          <div className="flex items-center gap-1 rounded border bg-bg px-2 py-1">
            <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-fg">{tunnel}</span>
            <CopyBtn text={tunnel} />
          </div>
        </div>
      )}
    </div>
  )
}


// BigHoleOption is the install toggle a MongoDB node or cluster and the Linux Client share
// (app/bighole.go): Big Hole served by nginx from the node, published to the host.
export function BigHoleOption({ value, deployed, onChange, where, disabledReason }) {
  return (
    <label className={`flex items-start gap-2 text-sm ${deployed || disabledReason ? 'opacity-70' : ''}`}>
      <input type="checkbox" className="mt-1" disabled={deployed || !!disabledReason} checked={!!value && !disabledReason}
        onChange={(e) => onChange(e.target.checked)} />
      <span>
        Install Big Hole (FTDC viewer)<Help text={HELP.bigHole} />
        <span className="block text-xs text-muted">
          {disabledReason || <>The browser-only MongoDB FTDC viewer, served from {where} on a published port. Open it on
            localhost and drag a <span className="font-mono">diagnostic.data</span> folder or support tarball onto it.</>}
        </span>
      </span>
    </label>
  )
}
