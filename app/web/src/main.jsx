import React from 'react'
import { createRoot } from 'react-dom/client'
// The Looks' typefaces, self-hosted: Vite copies the .woff2 files into dist, which the
// Go binary embeds, so they render in labs with no route out. All are SIL OFL 1.1 with
// no Reserved Font Name — see THIRD_PARTY_NOTICES.md before adding another.
import '@fontsource-variable/inter/wght.css'
import '@fontsource-variable/jetbrains-mono/wght.css'
import '@fontsource/barlow/400.css'
import '@fontsource/barlow/500.css'
import '@fontsource/barlow/600.css'
import '@fontsource/barlow/700.css'
import '@fontsource-variable/newsreader/opsz.css'
import '@fontsource-variable/newsreader/opsz-italic.css'
import '@fontsource-variable/nunito/wght.css'
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
