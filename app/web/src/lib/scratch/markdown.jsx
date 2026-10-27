// markdown.jsx — a small Markdown renderer for the Quick Note preview.
//
// It builds React elements directly and never sets HTML, so text pasted into a note cannot inject
// markup or script: a `<script>` in a note is shown as the characters `<script>`. Links are
// rendered only for http(s) and mailto URLs, and open in a new tab without an opener.
//
// Supported: # headings, paragraphs, **bold**, *italic*, `code`, ``` fenced blocks ```, - and 1.
// lists, > quotes, --- rules, [links](url).

const SAFE_URL = /^(https?:\/\/|mailto:)/i

function inline(text, keyBase) {
  const out = []
  const re = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\*[^*\s][^*]*\*)|(\[[^\]]+\]\([^)\s]+\))/g
  let last = 0
  let m
  let i = 0
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const k = `${keyBase}-${i++}`
    const t = m[0]
    if (m[1]) out.push(<code key={k} className="rounded bg-surface2 px-1 font-mono text-[0.9em]">{t.slice(1, -1)}</code>)
    else if (m[2]) out.push(<strong key={k}>{t.slice(2, -2)}</strong>)
    else if (m[3]) out.push(<em key={k}>{t.slice(1, -1)}</em>)
    else {
      const lm = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(t)
      out.push(SAFE_URL.test(lm[2])
        ? <a key={k} href={lm[2]} target="_blank" rel="noopener noreferrer" className="text-primary underline">{lm[1]}</a>
        : `${lm[1]} (${lm[2]})`)
    }
    last = m.index + t.length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

export function Markdown({ text }) {
  const lines = String(text ?? '').replace(/\r\n/g, '\n').split('\n')
  const blocks = []
  let i = 0
  let k = 0
  while (i < lines.length) {
    const line = lines[i]
    if (/^```/.test(line)) {
      const body = []
      i++
      while (i < lines.length && !/^```/.test(lines[i])) body.push(lines[i++])
      i++
      blocks.push(<pre key={k++} className="overflow-auto rounded bg-surface2 p-2 font-mono text-[11px]">{body.join('\n')}</pre>)
      continue
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line)
    if (h) {
      const size = ['text-lg', 'text-base', 'text-sm', 'text-sm', 'text-xs', 'text-xs'][h[1].length - 1]
      blocks.push(<div key={k++} className={`${size} font-semibold text-fg`}>{inline(h[2], `h${k}`)}</div>)
      i++
      continue
    }
    if (/^(-{3,}|\*{3,})\s*$/.test(line)) { blocks.push(<hr key={k++} className="border-border" />); i++; continue }
    if (/^>\s?/.test(line)) {
      const body = []
      while (i < lines.length && /^>\s?/.test(lines[i])) body.push(lines[i++].replace(/^>\s?/, ''))
      blocks.push(<blockquote key={k++} className="border-l-2 border-border pl-2 text-muted">{inline(body.join(' '), `q${k}`)}</blockquote>)
      continue
    }
    if (/^\s*([-*+]|\d+\.)\s+/.test(line)) {
      const ordered = /^\s*\d+\./.test(line)
      const items = []
      while (i < lines.length && /^\s*([-*+]|\d+\.)\s+/.test(lines[i])) items.push(lines[i++].replace(/^\s*([-*+]|\d+\.)\s+/, ''))
      const L = ordered ? 'ol' : 'ul'
      blocks.push(<L key={k++} className={`${ordered ? 'list-decimal' : 'list-disc'} pl-5`}>{items.map((t, j) => <li key={j}>{inline(t, `l${k}-${j}`)}</li>)}</L>)
      continue
    }
    if (!line.trim()) { i++; continue }
    const para = []
    while (i < lines.length && lines[i].trim() && !/^(#{1,6}\s|```|>|\s*([-*+]|\d+\.)\s+|-{3,}\s*$)/.test(lines[i])) para.push(lines[i++])
    blocks.push(<p key={k++}>{inline(para.join(' '), `p${k}`)}</p>)
  }
  return <div className="space-y-1.5 text-xs leading-relaxed text-fg">{blocks}</div>
}
