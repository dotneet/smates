import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['vitest/**/*.test.js'],
    coverage: {
      provider: 'v8',
      include: ['src/**'],
      reporter: ['json-summary', 'text'],
      reportsDirectory: 'coverage/vitest',
    },
  },
});
