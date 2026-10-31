import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Every JS file the browser receives says where its licences are. The bundle is a copy
// of React, xterm, rrweb and the rest (MIT/ISC/BSD), whose terms ask for their notice
// to go with it; the minifier drops their comments (and a plain `banner` with them, hence
// postBanner, added after minifying), so the bundle points at the texts
// the server publishes instead (app/licenses.go). `/*!` marks it a legal comment.
const banner = '/*! DBCanvas — GPL-3.0-only — source: https://github.com/jaimesicam/dbcanvas — ' +
  'this file bundles third-party code under its own licences; see /licenses/ */'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { rolldownOptions: { output: { postBanner: banner } } },
  server: {
    host: true,
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
      '/licenses': 'http://localhost:8080',
    },
  },
})
