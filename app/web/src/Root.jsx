import { useAuth } from './auth/AuthProvider.jsx'
import { Splash, SetupScreen, AuthScreen } from './auth/AuthScreens.jsx'
import App from './App.jsx'

export default function Root() {
  const { phase } = useAuth()
  if (phase === 'loading') return <Splash />
  if (phase === 'setup') return <SetupScreen />
  if (phase === 'anon') return <AuthScreen />
  return <App />
}
