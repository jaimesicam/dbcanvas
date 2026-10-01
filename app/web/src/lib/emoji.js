// emoji.js — the session chat's emoji (components/SessionPanel.jsx).
//
// A short, work-minded set rather than the whole Unicode table: what people say in a
// support or pairing session — agreement, thanks, a problem, a fix, waiting.
//
// Typed emoticons become emoji when the message is sent (withEmoji): ":)" → 🙂,
// ":+1:" → 👍. Only a standalone one is replaced — never inside a word, a URL or
// `code` — so "f(x:)" and a pasted SQL statement come through untouched.

export const EMOJI_GROUPS = [
  { name: 'Reactions', items: ['👍', '👎', '👏', '🙏', '🙌', '👀', '💯', '✅', '❌', '⚠️', '❓', '❗'] },
  { name: 'Faces', items: ['🙂', '😄', '😂', '😉', '😊', '😅', '🤔', '😮', '😕', '🙁', '😬', '😴'] },
  { name: 'Work', items: ['🚀', '🔥', '🐛', '🛠️', '🔧', '💡', '📌', '📈', '📉', '💾', '🔒', '⏳'] },
  { name: 'Hands', items: ['👋', '✋', '👌', '✌️', '🤞', '💪', '☕', '🎉', '❤️', '⭐', '🟢', '🔴'] },
]

const EMOTICONS = {
  ':)': '🙂', ':-)': '🙂', ':D': '😄', ':-D': '😄', ';)': '😉', ';-)': '😉',
  ':(': '🙁', ':-(': '🙁', ':P': '😛', ':-P': '😛', ':p': '😛', ':O': '😮', ':o': '😮',
  ":'D": '😂', ':/': '😕', '<3': '❤️', '8)': '😎',
  ':+1:': '👍', ':-1:': '👎', ':thumbsup:': '👍', ':thumbsdown:': '👎', ':ok:': '👌',
  ':wave:': '👋', ':tada:': '🎉', ':rocket:': '🚀', ':fire:': '🔥', ':bug:': '🐛',
  ':check:': '✅', ':x:': '❌', ':warning:': '⚠️', ':eyes:': '👀', ':pray:': '🙏',
  ':clap:': '👏', ':coffee:': '☕', ':heart:': '❤️', ':thinking:': '🤔', ':100:': '💯',
}

const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
// Longest first, so ":-)" wins over ":-" anything shorter.
const PATTERN = new RegExp(
  `(^|\\s)(${Object.keys(EMOTICONS).sort((a, b) => b.length - a.length).map(esc).join('|')})(?=$|\\s|[.,!?])`,
  'g',
)

// withEmoji replaces standalone emoticons outside `code` spans.
export function withEmoji(text) {
  return text
    .split(/(`[^`]*`)/)
    .map((part) => (part.startsWith('`') && part.endsWith('`') && part.length > 1
      ? part
      : part.replace(PATTERN, (_, pre, e) => pre + EMOTICONS[e])))
    .join('')
}

// onlyEmoji is whether a message is nothing but one to three emoji, drawn large.
export function onlyEmoji(text) {
  const t = text.trim()
  if (!t || t.length > 24) return false
  try {
    const rest = t.replace(/(\p{Extended_Pictographic}|\p{Emoji_Component}|\u200d|\ufe0f|\s)/gu, '')
    if (rest) return false
    const n = [...t.matchAll(/\p{Extended_Pictographic}/gu)].length
    return n >= 1 && n <= 3
  } catch {
    return false
  }
}
