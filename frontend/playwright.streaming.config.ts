import { defineConfig } from "@playwright/test"
import { mkdtempSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

const artifacts = process.env.DVFS_STREAMING_TEMP ?? mkdtempSync(join(tmpdir(), "dvfs-streaming-"))
process.env.DVFS_STREAMING_TEMP = artifacts
const port = Number(process.env.DVFS_STREAMING_PORT ?? 18764)

export default defineConfig({
  testDir: "./e2e",
  testMatch: "streaming.spec.ts",
  outputDir: join(artifacts, "results"),
  reporter: "list",
  timeout: 45_000,
  expect: { timeout: 15_000 },
  workers: 1,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    channel: "msedge",
    trace: "retain-on-failure",
    launchOptions: { args: ["--autoplay-policy=no-user-gesture-required"] },
  },
  webServer: {
    command: "node e2e/fixtures/streaming-server.cjs",
    env: { DVFS_STREAMING_TEMP: artifacts, DVFS_STREAMING_PORT: String(port) },
    url: `http://127.0.0.1:${port}/__ready`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
})
