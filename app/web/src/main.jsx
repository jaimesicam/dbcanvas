import React from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import { ThemeProvider } from './theme/ThemeProvider.jsx'
import { AuthProvider } from './auth/AuthProvider.jsx'
import { GuestGate } from './auth/GuestScreens.jsx'
import { installGuestTransport, isGuestMode } from './lib/guest.js'
import Root from './Root.jsx'
import { ResetPasswordScreen, resetToken } from './auth/AuthScreens.jsx'

// A tab on /join/<token> is a shared-session guest's: every API call it makes is
// marked as a guest's before anything renders (lib/guest.js), and the join screens
// come before the app.
const guest = isGuestMode()
if (guest) installGuestTransport()
// A tab on /reset-password/<token> is somebody using a reset link an admin sent: it
// needs no session, so it comes before the auth gate too.
const reset = resetToken()

createRoot(document.getElementById('root')).render(
  <React.StrictMode>
    <ThemeProvider>
      {guest ? (
        <GuestGate />
      ) : reset ? (
        <ResetPasswordScreen token={reset} />
      ) : (
        <AuthProvider>
          <Root />
        </AuthProvider>
      )}
    </ThemeProvider>
  </React.StrictMode>,
)
