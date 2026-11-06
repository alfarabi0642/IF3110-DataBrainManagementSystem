import { defineConfig } from 'vite';

const target = process.env.API_PROXY_TARGET ?? 'http://127.0.0.1:8080';

export default defineConfig({
  server: {
    port: 5173,
    strictPort: true,
    proxy: Object.fromEntries([
      '/api', '/static', '/healthz', '/logout', '^/databases/.+',
    ].map((prefix) => [prefix, { target }])),
  },
});
