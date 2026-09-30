import { useEffect } from 'react'
import { useAuth } from './auth/AuthProvider.jsx'
import { Splash, SetupScreen, AuthScreen } from './auth/AuthScreens.jsx'
import App from './App.jsx'

// onSessionEnded is set only for a shared-session guest (auth/GuestScreens.jsx): it
// is how the app hands the tab back to the closed screen when the session ends, or
// when the guest's credential stops working (removed, expired, sessions switched off).
export default function Root({ onSessionEnded }) {
  const { phase } = useAuth()
  if (onSessionEnded && phase === 'anon') return <GuestSignedOut onSessionEnded={onSessionEnded} />
  if (phase === 'loading') return <Splash />
  if (phase === 'setup') return <SetupScreen />
  if (phase === 'anon') return <AuthScreen />
  return <App onSessionEnded={onSessionEnded} />
}

function GuestSignedOut({ onSessionEnded }) {
  useEffect(() => { onSessionEnded('ended') }, [onSessionEnded])
  return <Splash />
}
