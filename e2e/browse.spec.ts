import { test, expect, type Page } from "@playwright/test"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { startBrowseServer } from "./fixtures/browse-server.cjs"

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..")
const root = fs.mkdtempSync(path.join(os.tmpdir(), "dvfs-browse-e2e-"))
let server: Awaited<ReturnType<typeof startBrowseServer>>
const link = (page: Page, name: string) =>
  page.getByRole("link", { name, exact: true })
async function navigate(page: Page, splat: string) {
  await page.evaluate(async (value) => {
    const modulePath = "/src/router.tsx"
    const { router } = await import(modulePath)
    await router.navigate({ to: "/$", params: { _splat: value } })
  }, splat)
}
test.beforeAll(async () => {
  server = await startBrowseServer({
    repo,
    root,
    appRoot: process.env.BROWSE_APP_ROOT ?? path.join(repo, "frontend"),
  })
})
test.afterAll(async () => {
  await server?.close()
})
test.beforeEach(async ({ context }) => {
  server.reset()
  server.setConfig({ pagination_type: "pages", per_page: 30 })
  await context.route("**/*", (route) =>
    new URL(route.request().url()).origin === server.origin
      ? route.continue()
      : route.abort()
  )
})

for (const view of ["list", "grid"]) {
  test(`${view} never exposes another folder during pending back navigation`, async ({
    page,
  }) => {
    await page.addInitScript(
      (value) =>
        localStorage.setItem(
          "dvfs-prefs",
          JSON.stringify({ state: { view: value }, version: 0 })
        ),
      view
    )
    server.setBMode("error")
    await page.goto(server.origin + "/synthetic/b")
    await expect(
      page.getByText("폴더를 열 수 없습니다", { exact: true })
    ).toBeVisible()
    await navigate(page, "synthetic/a")
    await expect(link(page, "only-A")).toBeVisible()
    await page
      .getByRole("checkbox", { name: "only-A 선택", exact: true })
      .check()
    server.setBMode("hold")
    await page.goBack()
    await expect(page.locator('[data-slot="breadcrumb-page"]')).toHaveText("b")
    await expect.poll(() => server.held.length).toBeGreaterThan(0)
    await expect(link(page, "only-A")).toHaveCount(0)
    await expect(
      page.getByRole("checkbox", { name: "only-A 선택", exact: true })
    ).toHaveCount(0)
    await expect(page.getByText("1개 선택", { exact: true })).toHaveCount(0)
    server.release()
    await expect(link(page, "only-B")).toBeVisible()
    await expect(link(page, "only-A")).toHaveCount(0)
  })
}

test("deleting the final item on page two shows remaining page one", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/pages?page=2")
  await expect(link(page, "item031")).toBeVisible()
  await page
    .getByRole("checkbox", { name: "item031 선택", exact: true })
    .check()
  await page.getByRole("button", { name: "삭제", exact: true }).click()
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "삭제", exact: true })
    .click()
  await expect.poll(() => server.data.get("/synthetic/pages").length).toBe(30)
  await expect(page).toHaveURL(server.origin + "/synthetic/pages")
  await expect(link(page, "item001")).toBeVisible()
  await expect(link(page, "item030")).toBeVisible()
  await expect(
    page.getByText("이 폴더는 비어 있습니다", { exact: true })
  ).toHaveCount(0)
})

test("direct out of range page waits for authoritative data then canonicalizes", async ({
  page,
}) => {
  let release: () => void = () => {}
  const hold = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route("**/api/fs/list?**", async (route) => {
    if (new URL(route.request().url()).searchParams.get("offset") === "2940")
      await hold
    await route.continue()
  })
  await page.goto(server.origin + "/synthetic/pages?page=99")
  await expect(page.getByRole("table")).toBeVisible()
  expect(new URL(page.url()).searchParams.get("page")).toBe("99")
  release()
  await expect(page).toHaveURL(server.origin + "/synthetic/pages?page=2")
  await expect(link(page, "item031")).toBeVisible()
})

for (const view of ["list", "grid"]) {
  test(`${view} native links preserve special path characters`, async ({
    page,
    context,
  }) => {
    await page.addInitScript(
      (value) =>
        localStorage.setItem(
          "dvfs-prefs",
          JSON.stringify({ state: { view: value }, version: 0 })
        ),
      view
    )
    await page.goto(server.origin + "/synthetic")
    for (const name of [
      "hash#tail",
      "query?tail",
      "percent%23tail",
      "한글 #?%",
      "raw#?%23.bin",
      "note#?%23.txt",
    ]) {
      const target = link(page, name)
      await expect(target).toBeVisible()
      const opened = context.waitForEvent("page")
      await target.click({ modifiers: ["Control"] })
      const popup = await opened
      expect(new URL(popup.url()).hash).toBe("")
      expect(new URL(popup.url()).search).toBe("")
      expect(new URL(popup.url()).pathname).toBe(
        (name.endsWith(".bin") ? "/download" : "") +
          "/synthetic/" +
          encodeURIComponent(name)
      )
      await popup.waitForLoadState("domcontentloaded")
      await expect(popup.locator('[data-slot="breadcrumb-page"]')).toHaveText(
        name
      )
      await expect(
        popup.getByText("열 수 없습니다", { exact: true })
      ).toHaveCount(0)
      await expect
        .poll(() =>
          server.requests.some(
            (r: { path: string; query: { path: string } }) =>
              r.path === "/api/fs/stat" && r.query.path === "/synthetic/" + name
          )
        )
        .toBe(true)
      await popup.close()
      await target.click()
      await expect(page.locator('[data-slot="breadcrumb-page"]')).toHaveText(
        name
      )
      await page.goBack()
      await expect(link(page, name)).toBeVisible()
    }
  })
}

for (const pagination_type of ["all", "more", "auto_more"]) {
  test(`${pagination_type} preserves page search`, async ({ page }) => {
    server.setConfig({ pagination_type })
    await page.goto(server.origin + "/synthetic/pages?page=99")
    await expect(link(page, "item001")).toBeVisible()
    await expect(page).toHaveURL(/page=99/)
  })
}

test("failed listing never canonicalizes a page from an empty fallback", async ({
  page,
}) => {
  server.setBMode("error")
  await page.goto(server.origin + "/synthetic/b?page=99")
  await expect(
    page.getByText("폴더를 열 수 없습니다", { exact: true })
  ).toBeVisible()
  await expect(page).toHaveURL(/page=99/)
})

test("initial config delay cannot canonicalize using default totals", async ({
  page,
}) => {
  let release: () => void = () => {}
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  let held = false
  await page.route("**/api/config", async (route) => {
    held = true
    await gate
    await route.continue()
  })
  await page.goto(server.origin + "/synthetic/pages?page=2")
  await expect.poll(() => held).toBe(true)
  await expect(page.getByRole("table")).toBeVisible()
  expect(new URL(page.url()).searchParams.get("page")).toBe("2")
  release()
  await expect(link(page, "item031")).toBeVisible()
  await expect(page).toHaveURL(/page=2/)
})

test("background listing refetch preserves current rows and waits before clamping", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/pages?page=2")
  await expect(link(page, "item031")).toBeVisible()
  let release: () => void = () => {}
  let held = false
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route("**/api/fs/list?**", async (route) => {
    held = true
    await gate
    await route.continue()
  })
  server.data.get("/synthetic/pages").pop()
  await page.evaluate(async () => {
    const modulePath = "/src/lib/queries.ts"
    const { queryClient } = await import(modulePath)
    void queryClient.invalidateQueries({ queryKey: ["list"] })
  })
  await expect.poll(() => held).toBe(true)
  await expect(link(page, "item031")).toBeVisible()
  expect(new URL(page.url()).searchParams.get("page")).toBe("2")
  release()
  await expect(page).toHaveURL(server.origin + "/synthetic/pages")
  await expect(link(page, "item001")).toBeVisible()
})

test("config refetch defers page correction until the new page size settles", async ({
  page,
}) => {
  server.setConfig({ per_page: 1 })
  await page.goto(server.origin + "/synthetic/pages?page=2")
  await expect(link(page, "item002")).toBeVisible()
  let release: () => void = () => {}
  let held = false
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route("**/api/config", async (route) => {
    held = true
    await gate
    await route.continue()
  })
  server.setConfig({ per_page: 30 })
  await page.evaluate(async () => {
    const modulePath = "/src/lib/queries.ts"
    const { queryClient } = await import(modulePath)
    void queryClient.invalidateQueries({ queryKey: ["config"] })
  })
  await expect.poll(() => held).toBe(true)
  const response = page.waitForResponse(
    (r) =>
      r.url().includes("/api/fs/list?") &&
      new URL(r.url()).searchParams.get("offset") === "98"
  )
  await page.evaluate(async () => {
    const modulePath = "/src/router.tsx"
    const { router } = await import(modulePath)
    await router.navigate({
      to: "/$",
      params: { _splat: "synthetic/pages" },
      search: { page: 99 },
    })
  })
  await response
  await expect(
    page.getByText("이 폴더는 비어 있습니다", { exact: true })
  ).toBeVisible()
  expect(new URL(page.url()).searchParams.get("page")).toBe("99")
  release()
  await expect(page).toHaveURL(server.origin + "/synthetic/pages?page=2")
  await expect(link(page, "item031")).toBeVisible()
})

test("same folder page transitions retain rows until the requested page arrives", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/pages")
  await expect(link(page, "item001")).toBeVisible()
  let release: () => void = () => {}
  let held = false
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route("**/api/fs/list?**", async (route) => {
    held = true
    await gate
    await route.continue()
  })
  await page.getByRole("button", { name: "2쪽", exact: true }).click()
  await expect.poll(() => held).toBe(true)
  await expect(link(page, "item001")).toBeVisible()
  await expect(page).toHaveURL(/page=2/)
  release()
  await expect(link(page, "item031")).toBeVisible()
  await expect(link(page, "item001")).toHaveCount(0)
})
