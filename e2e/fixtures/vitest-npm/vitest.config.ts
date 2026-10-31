import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';

// Mirrors the "paths" of tsconfig.json.
export default defineConfig({
  resolve: {
    alias: {
      '@lib': fileURLToPath(new URL('./src', import.meta.url)),
      '@test': fileURLToPath(new URL('./test', import.meta.url)),
    },
  },
  test: {
    setupFiles: ['./test/setup.ts'],
    globalSetup: ['./test/global-setup.ts'],
  },
});
