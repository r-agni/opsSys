import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api/fleet':   { target: 'http://localhost:8080', changeOrigin: true, rewrite: (p) => p.replace(/^\/api\/fleet/, '') },
      '/api/query':   { target: 'http://localhost:8081', changeOrigin: true, rewrite: (p) => p.replace(/^\/api\/query/, '') },
      '/api/command': { target: 'http://localhost:8082', changeOrigin: true, rewrite: (p) => p.replace(/^\/api\/command/, '') },
      '/api/apikeys': { target: 'http://localhost:8083', changeOrigin: true, rewrite: (p) => p.replace(/^\/api\/apikeys/, '') },
    },
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          'vendor-react': ['react', 'react-dom', 'react-router-dom'],
          'vendor-charts': ['echarts', 'echarts-for-react'],
          'vendor-maps': ['leaflet', 'react-leaflet'],
          'vendor-auth': ['oidc-client-ts'],
        },
      },
    },
  },
})
