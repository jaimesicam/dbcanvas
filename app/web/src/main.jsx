import React from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import { ThemeProvider } from './theme/ThemeProvider.jsx'
import { AuthProvider } from './auth/AuthProvider.jsx'
import Root from './Root.jsx'

createRoot(document.getElementById('root')).render(
  <React.StrictMode>
    <ThemeProvider>
      <AuthProvider>
        <Root />
      </AuthProvider>
    </ThemeProvider>
  </React.StrictMode>,
)
