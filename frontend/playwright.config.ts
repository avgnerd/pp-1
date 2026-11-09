import { defineConfig } from "@playwright/test"

// End-to-end smoke tests against the real Go server and the in-memory fake Dropbox.
// Build the frontend into backend/internal/web/dist first (scripts/run-local.ps1 does this),
// then: npm run e2e   (all three configs) or PLAYWRIGHT_HTML_OPEN=never npx playwright test
//
// testMatch keeps the streaming and browse/search specs out: they own separate configs with their
// own fixture servers, and collecting them here runs them against this Go server instead.
const port = 8099
// npm run e2e points this at a fresh temp directory; a bare `npx playwright test` keeps the
// existing data-playwright directory.
const dataDir = process.env.DVFS_E2E_DATA ?? "../data-playwright"
export default defineConfig({
  testDir: "./e2e",
  testMatch: "smoke.spec.ts",
  timeout: 90_000,
  fullyParallel: false,
  workers: 1,
  use: { baseURL: `http://localhost:${port}`, channel: "msedge", trace: "retain-on-failure" },
  webServer: [
    {
      command: "go run ./cmd/fakedropbox -addr 127.0.0.1:9911 -seed 3",
      cwd: "../backend",
      url: "http://127.0.0.1:9911/oauth2/authorize?client_id=probe",
      reuseExistingServer: false,
      timeout: 180_000,
    },
    {
      command: `go run ./cmd/server -data "${dataDir}"`,
      cwd: "../backend",
      url: `http://localhost:${port}/api/health`,
      env: { DVFS_LISTEN: `:${port}`, DVFS_DROPBOX_BASE: "http://127.0.0.1:9911" },
      // The server logs its data directory on startup; piping keeps that line in the run log.
      stderr: "pipe",
      reuseExistingServer: false,
      timeout: 180_000,
    },
  ],
})
