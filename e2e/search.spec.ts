import { test, expect, type Page } from "@playwright/test"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

import { startSearchServer } from "./fixtures/search-server.cjs"

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..")
const root =
  process.env.SEARCH_TEST_TEMP ??
  fs.mkdtempSync(path.join(os.tmpdir(), "dvfs-search-e2e-"))
let server: Awaited<ReturnType<typeof startSearchServer>>
const status = (page: Page) =>
  page.locator('[aria-live="polite"]').filter({ hasText: "저장소" })
const searches = (q: string) =>
  server.requests.filter(
    (r: { path: string; query: { q: string } }) =>
      r.path === "/api/search" && r.query.q === q
  ).length
async function search(page: Page, q: string) {
  await page.goto(
    server.origin + "/search?" + new URLSearchParams({ q, kind: "all" })
  )
  await expect(page.getByRole("textbox", { name: "검색어" })).toBeVisible()
}

test.beforeAll(async () => {
  server = await startSearchServer({
    repo,
    root,
    appRoot: process.env.SEARCH_APP_ROOT ?? path.join(repo, "frontend"),
  })
})
test.afterAll(async () => {
  await server?.close()
})
test.beforeEach(async ({ context }) => {
  server.setConfig({ pagination_type: "pages", per_page: 30 })
  await context.route("**/*", (route) =>
    new URL(route.request().url()).origin === server.origin
      ? route.continue()
      : route.abort()
  )
})

for (const q of ["http-failure", "early-eof", "partial-eof"]) {
  test(`${q} reports interruption and same-query submission retries`, async ({
    page,
  }) => {
    await search(page, q)
    await expect(status(page)).toContainText("검색 중단")
    await expect(page.getByRole("alert")).toContainText("다시 검색")
    await expect(
      page.getByText(`'${q}'에 맞는 항목이 없습니다`, { exact: true })
    ).toHaveCount(0)
    if (q === "partial-eof")
      await expect(
        page.getByRole("link", { name: "partial-folder", exact: true })
      ).toBeVisible()
    const before = searches(q)
    await page
      .getByRole("search")
      .getByRole("button", { name: "검색", exact: true })
      .click()
    await expect.poll(() => searches(q)).toBe(before + 1)
    await expect(status(page)).toContainText("검색 중단")
    if (q === "partial-eof")
      await expect(
        page.getByRole("link", { name: "partial-folder", exact: true })
      ).toHaveCount(1)
  })
}

test("named error stays failed rather than completed", async ({ page }) => {
  await search(page, "server-error")
  await expect(page.getByRole("alert")).toContainText(
    "synthetic fatal search error"
  )
  await expect(status(page)).toContainText("검색 실패")
})

test("successful same-query submission starts a fresh request", async ({
  page,
}) => {
  await search(page, "fresh")
  await expect(status(page)).toContainText("검색 완료")
  const before = searches("fresh")
  await page
    .getByRole("search")
    .getByRole("button", { name: "검색", exact: true })
    .click()
  await expect.poll(() => searches("fresh")).toBe(before + 1)
  await expect(status(page)).toContainText("검색 완료")
})

test("retry clears the old error when the fresh request succeeds", async ({
  page,
}) => {
  await search(page, "recover")
  await expect(status(page)).toContainText("검색 중단")
  await page
    .getByRole("search")
    .getByRole("button", { name: "검색", exact: true })
    .click()
  await expect(status(page)).toContainText("검색 완료")
  await expect(page.getByRole("alert")).toHaveCount(0)
  await expect(
    page.getByRole("link", { name: "recover-folder", exact: true })
  ).toHaveCount(1)
})

test("parent-directory anchor preserves special path in new tabs and ordinary clicks", async ({
  page,
  context,
}) => {
  await search(page, "special-parent")
  const parent = page.getByRole("link", {
    name: "/synthetic/한글 #?%23",
    exact: true,
  })
  await expect(parent).toHaveAttribute(
    "href",
    "/synthetic/" + encodeURIComponent("한글 #?%23")
  )
  const popupPromise = context.waitForEvent("page")
  await parent.click({ modifiers: ["Control"] })
  const popup = await popupPromise
  await popup.waitForLoadState("domcontentloaded")
  expect(decodeURIComponent(new URL(popup.url()).pathname)).toBe(
    "/synthetic/한글 #?%23"
  )
  await popup.close()
  await parent.click()
  await expect
    .poll(() =>
      server.requests.some(
        (r: { path: string; query: { path: string } }) =>
          r.path === "/api/fs/stat" && r.query.path === "/synthetic/한글 #?%23"
      )
    )
    .toBe(true)
})

test("special paths survive ordinary and new-tab navigation including downloads", async ({
  page,
  context,
}) => {
  await search(page, "special")
  await expect(status(page)).toContainText("검색 완료")
  for (const name of [
    "hash#tail",
    "query?tail",
    "percent%23tail",
    "한글 #?%",
    "raw#?%23.bin",
  ]) {
    const link = page.getByRole("link", { name, exact: true })
    const expected = "/synthetic/" + name
    const prefix = name.endsWith(".bin") ? "/download" : ""
    await expect(link).toHaveAttribute(
      "href",
      prefix + "/synthetic/" + encodeURIComponent(name)
    )
    const popupPromise = context.waitForEvent("page")
    await link.click({ modifiers: ["Control"] })
    const popup = await popupPromise
    await popup.waitForLoadState("domcontentloaded")
    expect(decodeURIComponent(new URL(popup.url()).pathname)).toBe(
      prefix + expected
    )
    expect(new URL(popup.url()).search).toBe("")
    expect(new URL(popup.url()).hash).toBe("")
    await popup.close()
  }
  await page.getByRole("link", { name: "hash#tail", exact: true }).click()
  await expect(page).toHaveURL(/hash%23tail/)
  await expect
    .poll(() =>
      server.requests.some(
        (r: { path: string; query: { path: string } }) =>
          r.path === "/api/fs/stat" && r.query.path === "/synthetic/hash#tail"
      )
    )
    .toBe(true)
})

test("successful rename refreshes results and opens the new path", async ({
  page,
}) => {
  server.resetRename()
  await search(page, "rename")
  const old = page.getByRole("link", { name: "rename-old", exact: true })
  await expect(old).toBeVisible()
  const before = searches("rename")
  await old.click({ button: "right" })
  await page.getByRole("menuitem", { name: "이름 바꾸기", exact: true }).click()
  await page.getByLabel("새 이름", { exact: true }).fill("rename-new")
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "이름 바꾸기", exact: true })
    .click()
  await expect(page.getByRole("dialog")).toHaveCount(0)
  await expect(
    page.getByRole("link", { name: "rename-new", exact: true })
  ).toBeVisible()
  await expect(old).toHaveCount(0)
  await expect.poll(() => searches("rename")).toBe(before + 1)
  await page.getByRole("link", { name: "rename-new", exact: true }).click()
  await expect
    .poll(() =>
      server.requests.some(
        (r: { path: string; query: { path: string } }) =>
          r.path === "/api/fs/stat" && r.query.path === "/synthetic/rename-new"
      )
    )
    .toBe(true)
})

test("truncation is visible only when reported", async ({ page }) => {
  await search(page, "truncated")
  await expect(status(page)).toContainText("500개")
  await expect(page.getByRole("alert")).toContainText("일부 결과")
  await search(page, "exact-cap")
  await expect(status(page)).toContainText("500개")
  await expect(page.getByRole("alert")).toHaveCount(0)
})

test("query replacement closes old stream, kind changes request and history restores input", async ({
  page,
}) => {
  await search(page, "old-query")
  await expect.poll(() => server.streams.has("old-query")).toBe(true)
  const old = server.streams.get("old-query")
  await page.getByRole("textbox", { name: "검색어" }).fill("  한글 & #?  ")
  await page
    .getByRole("search")
    .getByRole("button", { name: "검색", exact: true })
    .click()
  await expect(
    page.getByRole("link", { name: "한글 & #?-folder", exact: true })
  ).toBeVisible()
  await expect.poll(() => old.destroyed).toBe(true)
  await page.getByRole("button", { name: "파일", exact: true }).click()
  await expect
    .poll(() =>
      server.requests.some(
        (r: { path: string; query: { q: string; kind: string } }) =>
          r.path === "/api/search" &&
          r.query.q === "한글 & #?" &&
          r.query.kind === "file"
      )
    )
    .toBe(true)
  await page.goBack()
  await expect(page.getByRole("textbox", { name: "검색어" })).toHaveValue(
    "한글 & #?"
  )
  await page.goBack()
  await expect(page.getByRole("textbox", { name: "검색어" })).toHaveValue(
    "old-query"
  )
})

test("sorting and pagination preserve existing results without another request", async ({
  page,
}) => {
  await search(page, "sorted")
  await expect(status(page)).toContainText("검색 완료")
  await expect(page.locator("main li a.font-medium")).toHaveText([
    "folder2",
    "folder10",
    "afile.txt",
    "zfile.txt",
  ])
  await search(page, "many")
  await expect(status(page)).toContainText("220개")
  const before = searches("many")
  await page.locator('[aria-label="2쪽"]').click()
  await expect(
    page.getByRole("link", { name: "item31", exact: true })
  ).toBeVisible()
  await expect(page.locator("main li.grid a.font-medium")).toHaveCount(30)
  expect(searches("many")).toBe(before)
})

test("virtualized list reaches final row and storage errors remain visible", async ({
  page,
}) => {
  server.setConfig({ pagination_type: "all" })
  await search(page, "many")
  await expect(status(page)).toContainText("220개")
  expect(await page.locator("main li[data-index]").count()).toBeLessThan(220)
  await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight))
  await expect(
    page.getByRole("link", { name: "item220", exact: true })
  ).toBeVisible()
  await search(page, "storage-failure")
  await expect(page.getByRole("alert")).toContainText(
    "synthetic storage offline"
  )
})

test("deleting the last page canonicalizes the URL and copying stays on page two", async ({ page }) => {
  server.resetMutations()
  await search(page, "page-mutations")
  await expect(status(page)).toContainText("61개")
  const before = searches("page-mutations")
  await page.getByLabel("3쪽", { exact: true }).click()
  const historyLength = await page.evaluate(() => history.length)
  await page.getByRole("link", { name: "mut061", exact: true }).click({ button: "right" })
  await page.getByRole("menuitem", { name: "삭제", exact: true }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "삭제", exact: true }).click()
  await expect(status(page)).toContainText("60개")
  await expect(page).toHaveURL(/page=2(?:&|$)/)
  await expect(page.locator('a[aria-current="page"]')).toHaveText("2")
  expect(await page.evaluate(() => history.length)).toBe(historyLength)
  await expect(page.locator("main li.grid a.font-medium")).toHaveCount(30)
  expect(searches("page-mutations")).toBe(before + 1)
  await page.getByLabel("2쪽", { exact: true }).click()
  await page.getByRole("link", { name: "mut031", exact: true }).click({ button: "right" })
  await page.getByRole("menuitem", { name: "복사", exact: true }).click()
  await page.getByRole("dialog").getByRole("button", { name: "dest", exact: true }).click()
  await page.getByRole("dialog").getByRole("button", { name: "여기로 복사", exact: true }).click()
  await expect(page.getByRole("dialog")).toHaveCount(0)
  await expect(status(page)).toContainText("61개")
  await expect(page).toHaveURL(/page=2(?:&|$)/)
  await expect(page.locator('a[aria-current="page"]')).toHaveText("2")
  await expect(page.locator("main li.grid a.font-medium")).toHaveCount(30)
  expect(new URL(page.url()).searchParams.get("q")).toBe("page-mutations")
  expect(new URL(page.url()).searchParams.get("kind")).toBe("all")
  expect(searches("page-mutations")).toBe(before + 2)
})

test("partial streams retain the requested page until completion", async ({ page }) => {
  await page.goto(server.origin + "/search?q=delayed-page&kind=file&page=3")
  await expect(status(page)).toContainText("검색 중")
  await expect(page.getByRole("link", { name: "partial-folder", exact: true })).toBeVisible()
  await expect(page).toHaveURL(/page=3/)
  server.finishStream("delayed-page", 61)
  await expect(status(page)).toContainText("검색 완료")
  await expect(page).toHaveURL(/page=3/)
  await expect(page.getByRole("link", { name: "stream60", exact: true })).toBeVisible()
  expect(searches("delayed-page")).toBe(1)
})

test("pending config refetch cannot clamp using the previous page size", async ({ page }) => {
  const before = searches("delayed-page")
  await page.goto(server.origin + "/search?q=delayed-page&kind=all&page=3")
  await expect(page.getByRole("link", { name: "partial-folder", exact: true })).toBeVisible()
  let release!: () => void
  const gate = new Promise<void>((resolve) => { release = resolve })
  let requested!: () => void
  const pending = new Promise<void>((resolve) => { requested = resolve })
  await page.route("**/api/config", async (route) => {
    requested()
    await gate
    await route.continue()
  })
  server.setConfig({ per_page: 20 })
  try {
    await page.evaluate(() => {
      const modulePath = "/src/lib/queries.ts"
      void import(modulePath).then(({ queryClient, keys }) => queryClient.invalidateQueries({ queryKey: keys.config }))
    })
    await pending
    server.finishStream("delayed-page", 41)
    await expect(status(page)).toContainText("검색 완료")
    await expect(page).toHaveURL(/page=3/)
  } finally {
    release()
  }
  await expect(page.getByRole("link", { name: "stream40", exact: true })).toBeVisible()
  await expect(page).toHaveURL(/page=3/)
  expect(searches("delayed-page")).toBe(before + 1)
})

test("completed empty results remove an invalid page", async ({ page }) => {
  await page.goto(server.origin + "/search?q=empty&kind=folder&page=99")
  await expect(status(page)).toContainText("검색 완료")
  await expect(page).toHaveURL(/kind=folder$/)
  expect(new URL(page.url()).searchParams.has("page")).toBe(false)
  expect(searches("empty")).toBe(1)
})

test("initial config must load before an invalid page can be clamped", async ({ page }) => {
  server.setConfig({ per_page: 20 })
  let release!: () => void
  const gate = new Promise<void>((resolve) => { release = resolve })
  await page.route("**/api/config", async (route) => {
    await gate
    await route.continue()
  })
  try {
    await page.goto(server.origin + "/search?q=many&kind=all&page=10", { waitUntil: "domcontentloaded" })
    await expect(status(page)).toContainText("검색 완료")
    await expect(page).toHaveURL(/page=10/)
  } finally {
    release()
  }
  await expect(page.getByRole("link", { name: "item181", exact: true })).toBeVisible()
  await expect(page).toHaveURL(/page=10/)
})

test("automatic URL correction preserves scroll position", async ({ page }) => {
  await page.goto(server.origin + "/search?q=delayed-clamp&kind=all&page=3")
  await expect(status(page)).toContainText("검색 중")
  await expect(page.locator("main li.grid a.font-medium")).toHaveCount(30)
  await page.addStyleTag({ content: "html { overflow-anchor: none; } main { min-height: 3000px; }" })
  const maxScroll = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)
  await page.evaluate(() => window.scrollTo(0, 200))
  const scroll = await page.evaluate(() => window.scrollY)
  expect(scroll).toBeGreaterThan(0)
  server.finishStream("delayed-clamp", 60)
  await expect(page).toHaveURL(/page=2/)
  await expect(status(page)).toContainText("검색 완료")
  expect(await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)).toBe(maxScroll)
  expect(await page.evaluate(() => window.scrollY)).toBe(scroll)
  expect(searches("delayed-clamp")).toBe(1)
})

for (const q of ["partial-error", "partial-eof"]) {
  test(`${q} retains the requested page and partial rows`, async ({ page }) => {
    const before = searches(q)
    await page.goto(server.origin + `/search?q=${q}&kind=all&page=3`)
    await expect(status(page)).toContainText(q === "partial-error" ? "검색 실패" : "검색 중단")
    await expect(page.getByRole("link", { name: "partial-folder", exact: true })).toBeVisible()
    await expect(page).toHaveURL(/page=3/)
    expect(searches(q)).toBe(before + 1)
  })
}

test("completed truncated results clamp and keep their warning", async ({ page }) => {
  await page.goto(server.origin + "/search?q=truncated&kind=all&page=99")
  await expect(status(page)).toContainText("검색 완료")
  await expect(page).toHaveURL(/page=17/)
  await expect(page.getByRole("alert")).toContainText("일부 결과")
})

for (const pagination_type of ["all", "more"]) {
  test(`${pagination_type} pagination preserves the requested page`, async ({ page }) => {
    server.setConfig({ pagination_type })
    await page.goto(server.origin + "/search?q=fresh&kind=all&page=99")
    await expect(status(page)).toContainText("검색 완료")
    await expect(page).toHaveURL(/page=99/)
  })
}
