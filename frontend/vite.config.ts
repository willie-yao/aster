import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// base path is set at build time via VITE_BASE_PATH so the same engine repo
// can build for multiple consumer projects (each deployed under its own
// gh-pages prefix). Defaults to "/" for local dev.
const basePath = process.env.VITE_BASE_PATH || '/'

// VITE_MOCK_API points /api at a local `server -mock` process, so the dev
// server keeps HMR while the authenticated features light up. The proxy leaves
// the Host header alone: the server's CSRF guard compares it against the
// browser's Origin, and rewriting it would make every write look cross-origin.
// /data is not proxied because Vite already serves public/data, which is the
// same directory the mock server reads.
const mockAPI = process.env.VITE_MOCK_API

// The entry HTML pins a strict CSP that the dev server enforces too, so any
// local design or debugging tool that injects a script from its own origin is
// blocked while iterating. Drop the meta tag during `vite dev` only; the
// production build serves it untouched.
function stripDevCSP(): Plugin {
  return {
    name: 'aster-strip-dev-csp',
    apply: 'serve',
    transformIndexHtml(html) {
      return html.replace(
        /[ \t]*<meta\s+http-equiv="Content-Security-Policy"[^>]*>\n?/i,
        '',
      )
    },
  }
}

export default defineConfig({
  plugins: [react(), stripDevCSP()],
  base: basePath,
  build: {
    rollupOptions: {
      input: {
        index: fileURLToPath(new URL('./index.html', import.meta.url)),
        404: fileURLToPath(new URL('./404.html', import.meta.url)),
      },
    },
  },
  server: {
    strictPort: false,
    proxy: mockAPI
      ? { '/api': { target: mockAPI, changeOrigin: false } }
      : undefined,
  },
})
