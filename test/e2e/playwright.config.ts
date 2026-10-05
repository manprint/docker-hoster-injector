import { defineConfig } from '@playwright/test';

// The browser is the system Chrome, so nothing is downloaded. Point
// E2E_CHROME at another binary to use it instead.
const executablePath = process.env.E2E_CHROME || '/usr/bin/google-chrome';

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  // One agent and one set of containers are shared, and some tests change
  // them, so the tests run one after the other.
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  use: {
    launchOptions: { executablePath, args: ['--no-sandbox'] },
    trace: 'retain-on-failure',
  },
});
