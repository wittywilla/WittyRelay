// SPDX-License-Identifier: AGPL-3.0-or-later
// The admin page over a made-up relay: `go run ./internal/admin/uidemo`,
// started fresh (no password yet) for each run.
import { defineConfig, devices } from '@playwright/test'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const port = 18089
const dir = mkdtempSync(join(tmpdir(), 'wittyrelay-e2e-'))

export default defineConfig({
  testDir: '.',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  use: { baseURL: `http://localhost:${port}`, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `go run ./internal/admin/uidemo -listen 127.0.0.1:${port} -dir "${dir}"`,
    cwd: '..',
    url: `http://localhost:${port}/setup`,
    timeout: 120_000,
    reuseExistingServer: false,
    stdout: 'pipe',
  },
})
