// nodeWebLinks lists the web UIs a deployed node publishes — the same addresses its
// properties panel links to — for the canvas right-click menu's "Open in new tab" and
// "Open in VNC Browser". Each is { label, url }; the first is the node's main page.
//
// The panels build these one by one (VNCManager, PMMManager, SimDashboardLink, …), so
// a node type that gains a web UI there needs a line here too.
const SIMS = ['mclusteradmin', 'trafficsim', 'hotelsim', 'airlinesim', 'carsim', 'marketchaos', 'ledgersim', 'stocksim']

export function nodeWebLinks(type, dep) {
  const cfg = dep?.config || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const http = (port, path = '/') => (port ? `http://${host}:${port}${path}` : null)
  const out = []
  const add = (label, url) => { if (url) out.push({ label, url }) }
  switch (type) {
    case 'vnc':
      add('Web desktop', http(cfg.webPort, '/vnc.html'))
      break
    case 'pmm':
    case 'pmm2':
      add('PMM (HTTPS)', cfg.httpsPort ? `https://${host}:${cfg.httpsPort}/` : null)
      add('PMM (HTTP)', http(cfg.httpPort))
      break
    case 'bighole':
      // Always localhost: the app needs a secure context (see BigHoleLink).
      add('Big Hole', cfg.httpPort ? `http://localhost:${cfg.httpPort}/` : null)
      break
    case 'orchestrator':
      add('Orchestrator', http(cfg.exportPort))
      break
    case 'haproxy':
      add('Stats page', http(cfg.statsPort))
      break
    case 'seaweedfs':
      add('SeaweedFS UI', http(cfg.webPort, '/ui/index.html'))
      break
    case 'repository':
      add('Repository', http(cfg.httpPort))
      break
    case 'intranet':
      add('Webmail', http(cfg.webmailPort))
      break
    case 'k3d':
      if (cfg.operator === 'everest') add('OpenEverest', http(cfg.everestHostPort))
      break
    case 'aio':
      for (const r of cfg.instances || []) {
        for (const w of r.web || []) {
          if (w.hostPort > 0) add(`${r.inst} · ${w.label}`, http(w.hostPort, w.path || '/'))
        }
      }
      break
    default:
      if (SIMS.includes(type)) {
        add('Dashboard', http(cfg.httpPort))
        if (type === 'stocksim') add('Report', http(cfg.httpPort, '/report'))
      }
  }
  return out
}
