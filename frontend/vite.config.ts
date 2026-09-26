import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The Go server serves every file under web/ at /admin/<file>, the admin page at
// /admin and the customer portal at /user. Both HTML entries therefore load their
// hashed bundles from /admin/assets/.
//
// The dev server proxies the API to a locally running kiro-go (default port 8080,
// override with KIROGO_DEV_TARGET).
const target = process.env.KIROGO_DEV_TARGET || 'http://127.0.0.1:8080'

export default defineConfig({
  base: '/admin/',
  plugins: [react()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: '../web',
    emptyOutDir: true,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 900,
    rollupOptions: {
      input: {
        index: fileURLToPath(new URL('./index.html', import.meta.url)),
        user: fileURLToPath(new URL('./user.html', import.meta.url)),
      },
    },
  },
  server: {
    port: 5174,
    proxy: {
      '/admin/api': target,
      '/user/api': target,
      '/v1': target,
    },
  },
})
