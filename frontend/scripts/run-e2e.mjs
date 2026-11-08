// Runs each owned Playwright config in order and stops at the first failure, so the exit code is
// Playwright's. Each config owns its spec files and fixture servers.
import { spawnSync } from "node:child_process"
import { existsSync, mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"

const root = fileURLToPath(new URL("..", import.meta.url))

const configs = [
  { key: "smoke", file: "playwright.config.ts", note: "Go server + fake Dropbox" },
  { key: "streaming", file: "playwright.streaming.config.ts", note: "streaming fixture server" },
  { key: "browse", file: "playwright.browse.config.ts", note: "Vite browse/search fixtures" },
]

const cli = [
  join(root, "node_modules", "playwright", "cli.js"),
  join(root, "node_modules", "@playwright", "test", "cli.js"),
].find(existsSync)
if (!cli) {
  console.error("Playwright CLI not found. Run npm install in frontend/ first.")
  process.exit(1)
}

const artifacts = mkdtempSync(join(tmpdir(), "dvfs-e2e-run-"))
const dataDir = mkdtempSync(join(tmpdir(), "dvfs-e2e-data-"))
if (!existsSync(join(root, "..", "backend", "internal", "web", "dist", "index.html"))) {
  console.warn("warning: backend/internal/web/dist/index.html is missing; the smoke config serves the")
  console.warn("unbuilt frontend. Run npm run build and copy frontend/dist into that directory first.")
}
console.log(`artifacts: ${artifacts}`)
console.log(`server data directory (fresh per run): ${dataDir}\n`)

function countSpecs(file) {
  if (!existsSync(file)) return null
  const report = JSON.parse(readFileSync(file, "utf8"))
  const perFile = new Map()
  const walk = (suite, inherited) => {
    const current = suite.file ?? inherited
    const specs = suite.specs?.length ?? 0
    if (current && specs > 0) perFile.set(current, (perFile.get(current) ?? 0) + specs)
    for (const child of suite.suites ?? []) walk(child, current)
  }
  for (const suite of report.suites ?? []) walk(suite, undefined)
  return { stats: report.stats ?? {}, perFile }
}

const totals = { expected: 0, unexpected: 0, skipped: 0, flaky: 0, files: 0 }
let failed = null

for (const config of configs) {
  console.log(`=== ${config.key} (${config.file}, ${config.note}) ===`)
  const jsonFile = join(artifacts, `${config.key}.json`)
  const env = {
    ...process.env,
    PLAYWRIGHT_HTML_OPEN: "never",
    PLAYWRIGHT_HTML_OUTPUT_DIR: join(artifacts, `html-${config.key}`),
    PLAYWRIGHT_JSON_OUTPUT_FILE: jsonFile,
    DVFS_E2E_DATA: dataDir,
  }
  const result = spawnSync(
    process.execPath,
    [cli, "test", `--config=${join(root, config.file)}`, "--reporter=list,html,json"],
    { cwd: root, env, stdio: "inherit" },
  )
  const status = result.status ?? 1
  const summary = countSpecs(jsonFile)
  if (summary) {
    const stats = summary.stats
    for (const [file, count] of summary.perFile) {
      console.log(`  ${file}: ${count} specs`)
    }
    console.log(
      `  passed ${stats.expected ?? 0}, failed ${stats.unexpected ?? 0}, ` +
        `skipped ${stats.skipped ?? 0}, flaky ${stats.flaky ?? 0}`,
    )
    totals.expected += stats.expected ?? 0
    totals.unexpected += stats.unexpected ?? 0
    totals.skipped += stats.skipped ?? 0
    totals.flaky += stats.flaky ?? 0
    totals.files += summary.perFile.size
  } else {
    console.log("  no JSON report was written; see the Playwright output above")
  }
  if (status !== 0) {
    failed = { config, status }
    break
  }
  console.log("")
}

if (failed) {
  console.error(`\nFAILED in ${failed.config.key} (${failed.config.file}); later configs did not run.`)
  console.error(`Run artifacts kept at ${artifacts}`)
  process.exit(failed.status)
}

console.log(
  `\nall ${configs.length} configs passed: ${totals.expected} passed, ${totals.unexpected} failed, ` +
    `${totals.skipped} skipped, ${totals.flaky} flaky across ${totals.files} spec files`,
)
console.log(`html reports and json reports: ${artifacts}`)
console.log(`server data written to: ${dataDir}`)
