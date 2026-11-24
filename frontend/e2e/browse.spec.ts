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

test("gallery caption keeps a leading dot or hash in the file name", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/gallery")
  await page
    .getByRole("button", { name: "#1 <b>tag.jpg 원본 보기", exact: true })
    .click()
  await expect(page.locator(".lg-sub-html")).toHaveText("#1 <b>tag.jpg")
})

test("gallery caption keeps markup in the file name literal", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/gallery")
  await page
    .getByRole("button", { name: "A <b>B & C.jpg 원본 보기", exact: true })
    .click()
  await expect(page.locator(".lg-sub-html")).toHaveText("A <b>B & C.jpg")
})

test("mount bar marks the active mount regardless of path case", async ({
  page,
}) => {
  await page.goto(server.origin + "/SYNTHETIC/child")
  await expect(
    page.locator('nav[aria-label="마운트 경로"] a[aria-current="page"]')
  ).toHaveText("/synthetic")
})

test("path helpers decode safely and compare case-insensitively", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic")
  const result = await page.evaluate(async () => {
    const paths = await import("/src/lib/paths.ts")
    return {
      malformed: paths.decodePath("/mount/100%_test"),
      encoded: paths.decodePath("/mount/%ED%95%9C%EA%B8%80"),
      inside: paths.isWithin("/SYNTHETIC/a", "/synthetic"),
      outside: paths.isWithin("/other/a", "/synthetic"),
      same: paths.samePath("/SYNTHETIC", "/synthetic"),
      related: paths.isRelatedPath("/SYNTHETIC/a/b", "/synthetic/A"),
    }
  })
  expect(result).toEqual({
    malformed: "/mount/100%_test",
    encoded: "/mount/한글",
    inside: true,
    outside: false,
    same: true,
    related: true,
  })
})

test("file changes invalidate stat, siblings and picker caches case-insensitively", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic")
  const states = await page.evaluate(async () => {
    const { afterChange, queryClient } = await import("/src/lib/queries.ts")
    const cached = {
      stat: ["stat", "/synthetic/A/only-A"],
      siblings: ["siblings", "/synthetic/A", "name", "asc"],
      picker: ["picker", "/synthetic/A"],
      list: ["list", "/synthetic/A"],
      unrelated: ["stat", "/elsewhere"],
    }
    for (const key of Object.values(cached)) queryClient.setQueryData(key, [])
    afterChange(["/synthetic/a"])
    return Object.fromEntries(
      Object.entries(cached).map(([name, key]) => [
        name,
        queryClient.getQueryState(key)?.isInvalidated ?? null,
      ])
    )
  })
  expect(states).toEqual({
    stat: true,
    siblings: true,
    picker: true,
    list: true,
    unrelated: false,
  })
})

test("deleting a storage invalidates mount-scoped caches", async ({ page }) => {
  await page.goto(server.origin + "/admin/storages")
  await expect(page.getByText("1개 저장소", { exact: true })).toBeVisible()
  const cachedKeys = [
    ["list", "/synthetic"],
    ["stat", "/synthetic"],
    ["siblings", "/synthetic", "name", "asc"],
    ["picker", "/synthetic"],
  ]
  await page.evaluate(async (keys) => {
    const { queryClient } = await import("/src/lib/queries.ts")
    for (const key of keys) queryClient.setQueryData(key, [])
  }, cachedKeys)
  await page.getByRole("button", { name: "/synthetic 삭제" }).click()
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "저장소 삭제", exact: true })
    .click()
  await expect(
    page.getByText(
      "아직 저장소가 없습니다. '저장소 추가'로 첫 Dropbox 계정을 연결하세요.",
      { exact: true }
    )
  ).toBeVisible()
  const states = await page.evaluate(async (keys) => {
    const { queryClient } = await import("/src/lib/queries.ts")
    return keys.map(
      (key) => queryClient.getQueryState(key)?.isInvalidated ?? null
    )
  }, cachedKeys)
  expect(states).toEqual([true, true, true, true])
})

test("dotfiles and unicode names keep their in-app preview route", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic")
  await expect(link(page, ".env")).toHaveAttribute("href", "/synthetic/.env")
  await expect(link(page, "İ.mp4")).toHaveAttribute(
    "href",
    "/synthetic/%C4%B0.mp4"
  )
  const kinds = await page.evaluate(async () => {
    const format = await import("/src/lib/format.ts")
    return {
      env: format.extOf(".env"),
      envKind: format.fileKind(".env"),
      envPreview: format.hasPreview(".env", true),
      gitignore: format.extOf(".gitignore"),
      gitignoreKind: format.fileKind(".gitignore"),
      unicode: format.extOf("İ.mp4"),
      unicodeKind: format.fileKind("İ.mp4"),
      unicodeThumb: format.thumbnailable("İ.jpg"),
      tarGz: format.extOf("a.tar.gz"),
      onlyDot: format.extOf("."),
    }
  })
  expect(kinds).toEqual({
    env: "env",
    envKind: "text",
    envPreview: true,
    gitignore: "gitignore",
    gitignoreKind: "other",
    unicode: "mp4",
    unicodeKind: "video",
    unicodeThumb: true,
    tarGz: "tar.gz",
    onlyDot: "",
  })
  await page.goto(server.origin + "/download/synthetic/%C4%B0.mp4")
  await expect(page.locator("dd").first()).toHaveText("MP4")
})

test("out of range storage page canonicalizes to the last page", async ({
  page,
}) => {
  server.setMounts([
    {
      id: 1,
      name: "Synthetic 1",
      mount_path: "/synthetic",
      status: "active",
      account_email: "synthetic@example.invalid",
      account_name: "Synthetic",
    },
    {
      id: 2,
      name: "Synthetic 2",
      mount_path: "/synthetic-two",
      status: "active",
      account_email: "second@example.invalid",
      account_name: "Second",
    },
  ])
  server.setConfig({ pagination_type: "pages", per_page: 1 })
  await page.goto(server.origin + "/?page=99")
  await expect(
    page.getByText(/Synthetic 2 \(second@example\.invalid\)/)
  ).toBeVisible()
  await expect(page).toHaveURL(server.origin + "/?page=2")
  await expect(page.getByText("2개", { exact: true })).toBeVisible()
})

test("saving an invalid value shows the error on the tab that holds it", async ({
  page,
}) => {
  await stubAdminSettings(page)
  let saves = 0
  page.on("request", (request) => {
    if (
      request.method() === "PUT" &&
      new URL(request.url()).pathname === "/api/admin/settings"
    )
      saves++
  })
  await page.goto(server.origin + "/admin/settings")
  await expect(page.getByRole("heading", { name: "설정", exact: true })).toBeVisible()
  await page.getByRole("tab", { name: "시스템", exact: true }).click()
  await page.getByLabel("동시 변환 수", { exact: true }).fill("32")
  await page.getByRole("tab", { name: "사이트", exact: true }).click()
  await page.getByRole("button", { name: "설정 저장", exact: true }).click()
  await expect(
    page.getByText("동시 변환 수은(는) 16 이하여야 합니다", { exact: true })
  ).toBeVisible()
  await expect(
    page.getByRole("tab", { name: "시스템", exact: true })
  ).toHaveAttribute("aria-selected", "true")
  await expect(
    page.getByLabel("동시 변환 수", { exact: true })
  ).toHaveValue("32")
  expect(saves).toBe(0)
})

test("a deleted file closes the folder size stream it invalidates", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic")
  const folder = page.getByRole("link", { name: "normal", exact: true })
  await folder.click({ button: "right" })
  await page.getByRole("menuitem", { name: "폴더 크기", exact: true }).click()
  const row = page.getByRole("row").filter({ hasText: "normal" })
  await expect(row.getByText("1.00 KB", { exact: true })).toBeVisible()
  expect(
    server.sizeStreams.filter(
      (s: { path: string; closed: boolean }) =>
        s.path === "/synthetic/normal" && !s.closed
    )
  ).toHaveLength(1)

  await folder.click()
  await expect(link(page, "child")).toBeVisible()
  await page.getByRole("checkbox", { name: "child 선택", exact: true }).check()
  await page.getByRole("button", { name: "삭제", exact: true }).click()
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "삭제", exact: true })
    .click()
  await expect(link(page, "child")).toHaveCount(0)
  await navigate(page, "synthetic")
  // The total belongs to the state before the delete, so the stream must not deliver it.
  await expect
    .poll(() =>
      server.sizeStreams.some(
        (s: { path: string; closed: boolean }) => s.path === "/synthetic/normal" && s.closed
      )
    )
    .toBe(true)
  server.finishSize("/synthetic/normal", { bytes: 1024, files: 3, folders: 0 })
  await expect(row.getByText("1.00 KB", { exact: true })).toHaveCount(0)
})

test("a slow backup file read cannot describe the file chosen after it", async ({
  page,
}) => {
  await page.route("**/api/admin/backups/snapshots", (route) =>
    route.fulfill({ json: { snapshots: [] } })
  )
  // A slow disk read is what lets the two selections overlap; nothing else is stubbed. The
  // counter lets the test wait for both reads, and so for the stale continuation, without sleeping.
  await page.addInitScript(() => {
    const original = File.prototype.text
    File.prototype.text = function () {
      const wait = this.name.startsWith("slow-") ? 500 : 0
      const scope = window as Window & { __fileReads?: number }
      return new Promise((resolve, reject) =>
        setTimeout(
          () =>
            original.call(this).then((value) => {
              scope.__fileReads = (scope.__fileReads ?? 0) + 1
              resolve(value)
            }, reject),
          wait
        )
      )
    }
  })
  await page.goto(server.origin + "/admin/backup")
  const input = page.getByLabel("백업 파일 선택", { exact: true })
  const passwordField = page.getByLabel("복호화 비밀번호", { exact: true })
  const encrypted = {
    name: "slow-encrypted.json",
    mimeType: "application/json",
    buffer: Buffer.from(JSON.stringify({ encrypted: true })),
  }
  const plain = {
    name: "plain.json",
    mimeType: "application/json",
    buffer: Buffer.from(JSON.stringify({ encrypted: false })),
  }
  await input.setInputFiles(encrypted)
  await input.setInputFiles(plain)
  await page.waitForFunction(
    () => ((window as Window & { __fileReads?: number }).__fileReads ?? 0) >= 2
  )
  await expect(page.getByText("평문 백업", { exact: true })).toBeVisible()
  await expect(page.getByText("암호화됨", { exact: true })).toHaveCount(0)

  // A password entered for the encrypted file is meaningless for the plain one.
  await input.setInputFiles(encrypted)
  await expect(page.getByText("암호화됨", { exact: true })).toBeVisible()
  await passwordField.fill("secret-pass")
  await input.setInputFiles(plain)
  await expect(page.getByText("평문 백업", { exact: true })).toBeVisible()
  await expect(passwordField).toHaveValue("")
})

test("a grid thumbnail that fails to load falls back to the file icon", async ({
  page,
}) => {
  await page.addInitScript(() =>
    localStorage.setItem(
      "dvfs-prefs",
      JSON.stringify({ state: { view: "grid" }, version: 0 })
    )
  )
  const thumbs: string[] = []
  await page.route("**/api/fs/thumb?**", (route) => {
    thumbs.push(new URL(route.request().url()).searchParams.get("path") ?? "")
    return route.fulfill({ status: 404, body: "" })
  })
  await page.goto(server.origin + "/synthetic/gallery")
  const tile = page.getByRole("button", {
    name: "A <b>B & C.jpg 원본 보기",
    exact: true,
  })
  await expect
    .poll(() => thumbs.filter((path) => path.includes("B & C")).length)
    .toBeGreaterThan(0)
  await expect(tile.locator("svg")).toHaveCount(1)
  await expect(tile.locator("img")).toHaveCount(0)
})

test("the gallery asks for a thumbnail only for files that have one", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/gallery")
  await page
    .getByRole("button", { name: "vector.svg 원본 보기", exact: true })
    .click()
  await expect(page.locator(".lg-thumb-item")).toHaveCount(3)
  const sources = await page
    .locator(".lg-thumb-item img")
    .evaluateAll((images) => images.map((image) => image.getAttribute("src")))
  expect(sources.filter((src) => src?.includes("/api/fs/thumb"))).toHaveLength(2)
  expect(sources.some((src) => src?.includes("vector.svg"))).toBe(true)
})

test("a storage without an account email still reports the connection", async ({
  page,
}) => {
  const storage = {
    id: 1,
    name: "Synthetic 1",
    mount_path: "/synthetic",
    status: "pending",
    account_email: "",
    account_name: "",
    root_path: "/",
    app_key: "synthetic-key",
    account_id: "dbid:synthetic",
    cache_ttl_min: 30,
    auth_mode: "localhost",
    redirect_uri: "http://127.0.0.1/api/oauth/callback",
    last_error: "",
    created_at: 0,
    updated_at: 0,
    authorized_at: 1000,
  }
  let reads = 0
  // Dropbox can report a name without an email, and the server accepts that (handlers_auth.go
  // falls back to the name). The first poll is still pending, the second one is authorized.
  await page.route("**/api/admin/storages/1", async (route) => {
    if (route.request().method() !== "GET") return route.continue()
    const active = ++reads > 1
    await route.fulfill({
      json: {
        ...storage,
        status: active ? "active" : "pending",
        authorized_at: active ? 2000 : 1000,
        account_name: active ? "No Email Account" : "",
      },
    })
  })
  await page.route("**/api/admin/storages/1/reauth", (route) =>
    route.fulfill({
      json: {
        storage,
        authorize_url: "https://www.dropbox.com/oauth2/authorize?client_id=x",
        redirect_uri: storage.redirect_uri,
      },
    })
  )
  await page.goto(server.origin + "/admin/storages")
  await page.getByRole("button", { name: "재인증", exact: true }).click()
  await page.getByRole("button", { name: "승인 링크 만들기", exact: true }).click()
  await expect(page.getByText("연결되었습니다", { exact: true })).toBeVisible({
    timeout: 15_000,
  })
  await expect(page.getByText(/No Email Account 계정이/)).toBeVisible()
})

test("move dialog treats two spellings of one folder as the same destination", async ({
  page,
}) => {
  await page.goto(server.origin + "/synthetic/sub")
  await expect(link(page, "folderA")).toBeVisible()
  await page.getByRole("checkbox", { name: "folderA 선택", exact: true }).check()
  await page.getByRole("button", { name: "이동", exact: true }).click()
  const dialog = page.getByRole("dialog")
  await expect(
    dialog.getByRole("button", { name: "이미 이 폴더에 있습니다" })
  ).toBeVisible()
  await dialog.getByRole("button", { name: "상위 폴더", exact: true }).click()
  // The parent lists the folder by its real name; the moved entry kept the lowercase request path.
  await dialog.getByRole("button", { name: "Sub", exact: true }).click()
  await expect(
    dialog.getByRole("button", { name: "이미 이 폴더에 있습니다" })
  ).toBeVisible()
  await expect(
    dialog.getByRole("button", { name: "folderA", exact: true })
  ).toHaveCount(0)
})

const settingsValues = {
  robots_txt: "User-agent: *\nDisallow: /",
  pagination_type: "pages",
  per_page: 30,
  audio_autoplay: false,
  video_autoplay: false,
  archive_preview: true,
  cache_cleanup_sec: 120,
  data_mode: "proxy",
  transcode_concurrency: 2,
  thumb_cache_mb: 256,
  folder_size_concurrency: 2,
  search_concurrency: 2,
  archive_max_mb: 1,
}

/** Serves and stores the admin settings the settings page reads and writes. */
async function stubAdminSettings(page: Page) {
  let current: Record<string, unknown> = { ...settingsValues }
  await page.route("**/api/admin/settings", async (route) => {
    if (route.request().method() === "PUT") {
      current = { ...current, ...(route.request().postDataJSON() as Record<string, unknown>) }
      await route.fulfill({ json: current })
      return
    }
    await route.fulfill({ json: current })
  })
  return { values: () => current }
}

async function openAdminSettings(page: Page) {
  await page.evaluate(async () => {
    const modulePath = "/src/router.tsx"
    const { router } = await import(modulePath)
    await router.navigate({ to: "/admin/settings" })
  })
  await expect(page.getByRole("heading", { name: "설정", exact: true })).toBeVisible()
}

test("saving a new archive size limit refreshes an open archive preview", async ({
  page,
}) => {
  const settings = await stubAdminSettings(page)
  let maxMb = 1
  const budgetRequests: number[] = []
  await page.route("**/api/fs/archive?**", async (route) => {
    budgetRequests.push(maxMb)
    await route.fulfill({
      json: {
        listing: {
          format: "rar",
          truncated: maxMb <= 1,
          entries:
            maxMb <= 1
              ? [{ name: "first.txt", size: 10, dir: false }]
              : [
                  { name: "first.txt", size: 10, dir: false },
                  { name: "beyond-the-limit.txt", size: 20, dir: false },
                ],
        },
        sequential: true,
        budget_mb: maxMb,
      },
    })
  })
  await page.goto(server.origin + "/synthetic/archive.rar")
  await expect(
    page.getByText("목록 일부만 표시합니다", { exact: true })
  ).toBeVisible()
  await expect(
    page.getByRole("treeitem", { name: /beyond-the-limit\.txt/ })
  ).toHaveCount(0)

  await openAdminSettings(page)
  await page.getByRole("tab", { name: "시스템", exact: true }).click()
  await page.getByLabel("압축 파일 읽기 상한 (MB)", { exact: true }).fill("64")
  const saved = page.waitForResponse(
    (response) =>
      response.request().method() === "PUT" &&
      new URL(response.url()).pathname === "/api/admin/settings"
  )
  await page.getByRole("button", { name: "설정 저장", exact: true }).click()
  await saved
  maxMb = Number(settings.values().archive_max_mb)

  await navigate(page, "synthetic/archive.rar")
  await expect(
    page.getByRole("treeitem", { name: /beyond-the-limit\.txt/ })
  ).toBeVisible()
  await expect(
    page.getByText("목록 일부만 표시합니다", { exact: true })
  ).toHaveCount(0)
  expect(budgetRequests).toEqual([1, 64])
})
