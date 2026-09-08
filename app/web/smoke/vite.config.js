// Vite config for the render smoke test only (npm run smoke).
//
// The app's own build is untouched. This exists so the SSR bundle can swap the
// browser-only xterm packages for stubs: TerminalProvider imports them — and
// their stylesheet — at module scope, and they reach for browser globals the
// moment they load, so node cannot even import the provider without this.
//
// The alias list is an ARRAY rather than an object because order matters: the
// stylesheet entry has to match before the package-prefix entry, or
// "@xterm/xterm/css/xterm.css" gets rewritten into a path inside the JS stub.
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'

const js = fileURLToPath(new URL('./xterm-stub.js', import.meta.url))
const css = fileURLToPath(new URL('./empty.css', import.meta.url))

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: [
      { find: /^@xterm\/xterm\/css\/.*$/, replacement: css },
      { find: '@xterm/xterm', replacement: js },
      { find: '@xterm/addon-fit', replacement: js },
    ],
  },
})
