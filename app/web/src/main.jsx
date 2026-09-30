import React from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import { ThemeProvider } from './theme/ThemeProvider.jsx'
import { AuthProvider } from './auth/AuthProvider.jsx'
import { GuestGate } from './auth/GuestScreens.jsx'
import { installGuestTransport, isGuestMode } from './lib/guest.js'
import Root from './Root.jsx'

// A tab on /join/<token> is a shared-session guest's: every API call it makes is
// marked as a guest's before anything renders (lib/guest.js), and the join screens
// come before the app.
const guest = isGuestMode()
if (guest) installGuestTransport()

createRoot(document.getElementById('root')).render(
  <React.StrictMode>
    <ThemeProvider>
      {guest ? (
        <GuestGate />
      ) : (
        <AuthProvider>
          <Root />
        </AuthProvider>
      )}
    </ThemeProvider>
  </React.StrictMode>,
)
