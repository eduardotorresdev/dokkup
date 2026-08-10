import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [sveltekit()],
  server: {
    port: 5173,
    // In development the frontend is served by Vite and the API by the Go
    // binary. Proxying keeps them same-origin, so the session cookie and the
    // CSRF header behave exactly as they will in production.
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false
      }
    }
  }
});
