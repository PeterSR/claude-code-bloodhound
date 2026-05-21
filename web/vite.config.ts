import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import path from 'node:path';

// The daemon serves its API on a unix socket at
// $XDG_RUNTIME_DIR/bloodhound/api.sock. Vite's proxy is built on
// http-proxy, which supports unix sockets via `target.socketPath`.
const runtimeDir = process.env.XDG_RUNTIME_DIR;
if (!runtimeDir) {
  throw new Error(
    'XDG_RUNTIME_DIR is not set; cannot locate the bloodhound daemon socket. ' +
      'Run `bloodhound daemon` under a normal logged-in session.',
  );
}
const socketPath = path.join(runtimeDir, 'bloodhound', 'api.sock');

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: { socketPath, host: 'bloodhound.local', protocol: 'http:' },
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
});
