// Avatar — the picture beside a person's name: an account's (app/profile.go) or a
// shared-session guest's.
//
// An avatar is one of a fixed set, stored by id: a glyph on a colour of its own.
// Nothing is uploaded, so there is nothing to store, scan or serve, and every
// browser draws the same set. With none chosen, the person's initials stand in.
//
// The ids and their order are app/profile.go's avatarIDs; app/profile_test.go checks
// that the two lists agree.

export const AVATARS = [
  { id: 'fox', glyph: '🦊', bg: '#f97316', label: 'Fox' },
  { id: 'panda', glyph: '🐼', bg: '#64748b', label: 'Panda' },
  { id: 'koala', glyph: '🐨', bg: '#94a3b8', label: 'Koala' },
  { id: 'owl', glyph: '🦉', bg: '#a16207', label: 'Owl' },
  { id: 'octopus', glyph: '🐙', bg: '#db2777', label: 'Octopus' },
  { id: 'whale', glyph: '🐳', bg: '#0284c7', label: 'Whale' },
  { id: 'lion', glyph: '🦁', bg: '#d97706', label: 'Lion' },
  { id: 'tiger', glyph: '🐯', bg: '#ea580c', label: 'Tiger' },
  { id: 'frog', glyph: '🐸', bg: '#16a34a', label: 'Frog' },
  { id: 'monkey', glyph: '🐵', bg: '#92400e', label: 'Monkey' },
  { id: 'penguin', glyph: '🐧', bg: '#1e293b', label: 'Penguin' },
  { id: 'unicorn', glyph: '🦄', bg: '#c026d3', label: 'Unicorn' },
  { id: 'bee', glyph: '🐝', bg: '#eab308', label: 'Bee' },
  { id: 'turtle', glyph: '🐢', bg: '#15803d', label: 'Turtle' },
  { id: 'butterfly', glyph: '🦋', bg: '#2563eb', label: 'Butterfly' },
  { id: 'wolf', glyph: '🐺', bg: '#475569', label: 'Wolf' },
  { id: 'bear', glyph: '🐻', bg: '#78350f', label: 'Bear' },
  { id: 'rabbit', glyph: '🐰', bg: '#f472b6', label: 'Rabbit' },
  { id: 'dino', glyph: '🦖', bg: '#65a30d', label: 'Dinosaur' },
  { id: 'dragon', glyph: '🐲', bg: '#059669', label: 'Dragon' },
  { id: 'cat', glyph: '🐱', bg: '#f59e0b', label: 'Cat' },
  { id: 'dog', glyph: '🐶', bg: '#b45309', label: 'Dog' },
  { id: 'rocket', glyph: '🚀', bg: '#4f46e5', label: 'Rocket' },
  { id: 'cactus', glyph: '🌵', bg: '#0d9488', label: 'Cactus' },
  { id: 'clover', glyph: '🍀', bg: '#22c55e', label: 'Clover' },
  { id: 'rainbow', glyph: '🌈', bg: '#0ea5e9', label: 'Rainbow' },
  { id: 'robot', glyph: '🤖', bg: '#6366f1', label: 'Robot' },
  { id: 'alien', glyph: '👽', bg: '#7c3aed', label: 'Alien' },
]

const BY_ID = Object.fromEntries(AVATARS.map((a) => [a.id, a]))

export function initials(name) {
  const parts = String(name || '?').trim().split(/\s+/).filter(Boolean)
  if (parts.length >= 2) return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
  return (parts[0] || '?').slice(0, 2).toUpperCase()
}

// fullName is how a person is shown: "First Last", else the username.
export function fullName(u) {
  const n = `${u?.firstName || ''} ${u?.lastName || ''}`.trim()
  return n || u?.username || u?.name || ''
}

// Avatar draws one. avatar is an id from AVATARS (or nothing); name gives the
// initials when there is none, and the tooltip.
export function Avatar({ avatar, name, size = 28, className = '', title }) {
  const a = BY_ID[avatar]
  const style = { width: size, height: size, fontSize: a ? Math.round(size * 0.56) : Math.round(size * 0.4) }
  if (a) {
    return (
      <span title={title ?? name} className={`inline-flex shrink-0 select-none items-center justify-center rounded-full leading-none ${className}`}
        style={{ ...style, background: a.bg }} aria-label={name ? `${name} (${a.label})` : a.label}>
        <span aria-hidden>{a.glyph}</span>
      </span>
    )
  }
  return (
    <span title={title ?? name} className={`inline-flex shrink-0 select-none items-center justify-center rounded-full bg-primary/15 font-semibold text-primary ${className}`}
      style={style}>
      {initials(name)}
    </span>
  )
}

// AvatarPicker is the grid to choose one from.
export function AvatarPicker({ value, onChange, size = 36 }) {
  return (
    <div className="grid max-w-[22rem] grid-cols-7 gap-1.5" role="radiogroup" aria-label="Avatar">
      {AVATARS.map((a) => {
        const on = value === a.id
        return (
          <button key={a.id} type="button" role="radio" aria-checked={on} title={a.label} onClick={() => onChange(a.id)}
            className={`flex items-center justify-center rounded-full p-0.5 transition ${on ? 'ring-2 ring-primary ring-offset-2 ring-offset-surface' : 'opacity-80 hover:opacity-100'}`}>
            <Avatar avatar={a.id} name={a.label} size={size} title={a.label} />
          </button>
        )
      })}
    </div>
  )
}

// randomAvatar is a starting pick, so nobody has to choose to get going.
export function randomAvatar() {
  return AVATARS[Math.floor(Math.random() * AVATARS.length)].id
}
