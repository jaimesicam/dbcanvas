import { createContext, useContext, useEffect, useState } from 'react'

// Appearance is two independent choices, and this provider owns both.
//
//   THEME  what colour is it   — the palettes in index.css (data-theme)
//   LOOK   what shape is it    — radius, borders, elevation, density and the
//                                heading typeface (data-look)
//
// They are orthogonal on purpose: every look works in every theme, so needing a
// dark UI does not also decide the shape of your cards, and a new theme is six
// lines of colour rather than four more variants to draw.

export const THEMES = [
  { id: 'light', label: 'Light', swatch: '#4f46e5' },
  { id: 'dark', label: 'Dark', swatch: '#6366f1' },
  { id: 'midnight', label: 'Midnight', swatch: '#7c5cff' },
  { id: 'solarized', label: 'Solarized', swatch: '#268bd2' },
  { id: 'synthwave', label: 'Synthwave', swatch: '#ff2e97' },
  { id: 'forest', label: 'Forest', swatch: '#2fae66' },
]

// The four looks. `hint` is what the Settings page prints under each one, so it
// says what the look is FOR rather than listing what it changes — the preview
// beside it already shows that.
export const LOOKS = [
  {
    id: 'modern',
    label: 'Modern',
    hint: 'Rounded cards, soft shadows, comfortable rows. The default.',
  },
  {
    id: 'industrial',
    label: 'Industrial',
    hint: 'An instrument panel: mono upper-case headings, buttons and inputs, square corners, flat surfaces, zebra tables, and the smallest and tightest rows — the most data per screen.',
  },
  {
    id: 'editorial',
    label: 'Editorial',
    hint: 'A printed report: a book serif throughout with sans controls, hairline rules, ruled card titles and generous whitespace. Set largest, for reading.',
  },
  {
    id: 'soft',
    label: 'Soft',
    hint: 'A rounded, humanist face with pill buttons, inputs and badges, borders faded back and elevation doing the layering. Roomy, and the easiest on the eye.',
  },
]

const THEME_KEY = 'dbcanvas-theme'
const LOOK_KEY = 'dbcanvas-look'
const ThemeContext = createContext(null)

// Both choices are mirrored to localStorage as well as to the account (see
// SettingsProvider): the account copy is the one that follows you to another
// browser, and this one paints the login screen before there IS an account to
// ask, which is what stops a flash of the wrong appearance on every load.
function stored(key, fallback, valid) {
  try {
    const v = localStorage.getItem(key)
    return valid.some((o) => o.id === v) ? v : fallback
  } catch {
    return fallback
  }
}

export function ThemeProvider({ children }) {
  const [theme, setTheme] = useState(() => stored(THEME_KEY, 'dark', THEMES))
  const [look, setLook] = useState(() => stored(LOOK_KEY, 'modern', LOOKS))

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    try {
      localStorage.setItem(THEME_KEY, theme)
    } catch {
      // ignore storage failures (private mode, etc.)
    }
  }, [theme])

  useEffect(() => {
    document.documentElement.setAttribute('data-look', look)
    try {
      localStorage.setItem(LOOK_KEY, look)
    } catch {
      // ignore storage failures (private mode, etc.)
    }
  }, [look])

  return (
    <ThemeContext.Provider value={{ theme, setTheme, themes: THEMES, look, setLook, looks: LOOKS }}>
      {children}
    </ThemeContext.Provider>
  )
}

export function useTheme() {
  const ctx = useContext(ThemeContext)
  if (!ctx) throw new Error('useTheme must be used within ThemeProvider')
  return ctx
}
