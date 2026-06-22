import { createContext, useContext, useEffect, useState } from 'react'

export const THEMES = [
  { id: 'light', label: 'Light', swatch: '#4f46e5' },
  { id: 'dark', label: 'Dark', swatch: '#6366f1' },
  { id: 'midnight', label: 'Midnight', swatch: '#7c5cff' },
  { id: 'solarized', label: 'Solarized', swatch: '#268bd2' },
  { id: 'synthwave', label: 'Synthwave', swatch: '#ff2e97' },
  { id: 'forest', label: 'Forest', swatch: '#2fae66' },
]

const STORAGE_KEY = 'dbcanvas-theme'
const ThemeContext = createContext(null)

export function ThemeProvider({ children }) {
  const [theme, setTheme] = useState(() => {
    try {
      return localStorage.getItem(STORAGE_KEY) || 'dark'
    } catch {
      return 'dark'
    }
  })

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    try {
      localStorage.setItem(STORAGE_KEY, theme)
    } catch {
      // ignore storage failures (private mode, etc.)
    }
  }, [theme])

  return (
    <ThemeContext.Provider value={{ theme, setTheme, themes: THEMES }}>
      {children}
    </ThemeContext.Provider>
  )
}

export function useTheme() {
  const ctx = useContext(ThemeContext)
  if (!ctx) throw new Error('useTheme must be used within ThemeProvider')
  return ctx
}
