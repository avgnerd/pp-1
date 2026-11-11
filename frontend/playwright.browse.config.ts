import { defineConfig } from "@playwright/test"

export default defineConfig({
  testDir: "./e2e",
  testMatch: ["browse.spec.ts", "search.spec.ts"],
  timeout: 30_000,
  workers: 1,
  use: { channel: "msedge", trace: "retain-on-failure" },
  outputDir: process.env.BROWSE_TEST_OUTPUT,
})
