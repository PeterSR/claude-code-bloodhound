import { defineConfig, type UserConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import path from 'node:path';

// The daemon serves its API on a unix socket at
// $XDG_RUNTIME_DIR/bloodhound/api.sock. Vite's proxy is built on
// http-proxy, which supports unix sockets via `target.socketPath`.
// Only `vite dev` needs the socket — `vite build` produces a static
// bundle and the GUI binary proxies API calls at runtime — so we
// only require XDG_RUNTIME_DIR in serve mode. This lets the bundle
// build on CI runners (macOS, Windows) that don't have it set.
export default defineConfig(({ command }) => {
  const config: UserConfig = {
    plugins: [react(), tailwindcss()],
    build: {
      outDir: 'dist',
      emptyOutDir: true,
    },
  };

  if (command === 'serve') {
    const runtimeDir = process.env.XDG_RUNTIME_DIR;
    if (!runtimeDir) {
      throw new Error(
        'XDG_RUNTIME_DIR is not set; cannot locate the bloodhound daemon socket. ' +
          'Run `bloodhound daemon` under a normal logged-in session.',
      );
    }
    const socketPath = path.join(runtimeDir, 'bloodhound', 'api.sock');
    config.server = {
      port: 5173,
      proxy: {
        '/api': {
          target: { socketPath, host: 'bloodhound.local', protocol: 'http:' },
        },
      },
    };
  }

  return config;
});
