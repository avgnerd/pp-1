import { defineConfig } from "@playwright/test"

// End-to-end smoke tests against the real Go server and the in-memory fake Dropbox.
// Build the frontend into backend/internal/web/dist first (scripts/run-local.ps1 does this),
// then: PLAYWRIGHT_HTML_OPEN=never npx playwright test
const port = 8099
export default defineConfig({
  testDir: "./e2e",
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
      command: `go run ./cmd/server -data ../data-playwright`,
      cwd: "../backend",
      url: `http://localhost:${port}/api/health`,
      env: { DVFS_LISTEN: `:${port}`, DVFS_DROPBOX_BASE: "http://127.0.0.1:9911" },
      reuseExistingServer: false,
      timeout: 180_000,
    },
  ],
})
