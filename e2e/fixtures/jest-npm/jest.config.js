'use strict';

/** @type {import('jest').Config} */
module.exports = {
  testEnvironment: 'node',
  // Pin the clock and environment for every test file.
  setupFiles: ['<rootDir>/test-support/setup-env.js'],
  // Fixture data lives next to the tests but is not a test.
  testPathIgnorePatterns: ['/node_modules/', '/__fixtures__/'],
  // Same parallelism as the CI runners (4 vCPUs).
  maxWorkers: 3,
};
