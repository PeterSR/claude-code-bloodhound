import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:7777',
    },
  },
  build: {
    // emptyOutDir is false so the tracked dist/.gitkeep survives builds.
    // Stale hashed asset files will linger; harmless and rebuilt on next
    // clean.
    outDir: 'dist',
    emptyOutDir: false,
  },
});
