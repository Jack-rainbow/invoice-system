import { defineConfig } from 'vite';

// /api is preserved for the RFC9421 target URI. Production uses same-origin TLS.
export default defineConfig({
  server: {
    host: '127.0.0.1',
    strictPort: true,
    proxy: { '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false } },
  },
  build: { target: 'es2023', sourcemap: false },
});
