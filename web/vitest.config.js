import { defineConfig } from 'vitest/config';

// Unit and component tests for web/src run under jsdom; the browser e2e stays
// in scripts/*-check.js against a live server.
export default defineConfig({
  test: {
    environment: 'jsdom',
    include: ['test/**/*.test.js'],
    restoreMocks: true,
  },
});
