// Prints the licence text of every production npm dependency — the packages Vite
// bundles into dist, and so into the Go binary that embeds it. MIT, BSD and OFL all
// require their notice to travel with the copies, and a minified bundle does not
// reliably carry it, so app/Dockerfile runs this and ships the output beside the
// binary (/usr/share/doc/dbcanvas/). Run from app/web: node licenses/generate.mjs
import { execFileSync } from 'node:child_process'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))

// Walk the tree rather than take --parseable's flat list: that one also prints
// "extraneous" packages left in node_modules by some earlier install, which the
// lockfile — and so `npm ci` in the image build — never brings.
const tree = JSON.parse(execFileSync('npm', ['ls', '--omit=dev', '--all', '--long', '--json'], { encoding: 'utf8' }))
const dirs = []
const walk = deps => Object.values(deps || {}).forEach(d => {
  if (d.extraneous || !d.path) return
  dirs.push(d.path)
  walk(d.dependencies)
})
walk(tree.dependencies)

const seen = new Set()
const out = ['Third-party npm packages bundled into the DBCanvas web interface', '']
for (const dir of dirs.sort()) {
  const pkg = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'))
  const id = `${pkg.name}@${pkg.version}`
  if (seen.has(id)) continue
  seen.add(id)
  out.push('='.repeat(78), `${id}  (${pkg.license || 'see below'})`, '='.repeat(78))
  const files = readdirSync(dir).filter(f => /^(licen[cs]e|copying|notice)/i.test(f))
  if (files.length) {
    for (const f of files) out.push(readFileSync(join(dir, f), 'utf8').trim(), '')
    continue
  }
  // Some monorepos publish without their LICENSE (every rrweb package does). Their
  // upstream text is kept beside this script as <owner>_<repo>.LICENSE, fetched from the
  // release tag. A package that has neither fails the build on purpose: a new
  // dependency must not ship without somebody having looked at its licence.
  const repo = (typeof pkg.repository === 'string' ? pkg.repository : pkg.repository?.url) || ''
  const m = repo.match(/github\.com[/:]([^/]+)\/([^/.]+)/)
  const kept = m && join(here, `${m[1]}_${m[2]}.LICENSE`)
  if (!kept || !existsSync(kept)) throw new Error(`${id}: no licence file in the package and none kept in licenses/ (repository: ${repo || 'none'})`)
  out.push(`(The package publishes no licence file; this is the text from ${m[1]}/${m[2]}.)`, readFileSync(kept, 'utf8').trim(), '')
}
process.stdout.write(out.join('\n') + '\n')
