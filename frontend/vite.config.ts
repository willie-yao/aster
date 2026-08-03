import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// base path is set at build time via VITE_BASE_PATH so the same engine repo
// can build for multiple consumer projects (each deployed under its own
// gh-pages prefix). Defaults to "/" for local dev.
const basePath = process.env.VITE_BASE_PATH || '/'

export default defineConfig({
  plugins: [react()],
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
  },
})
