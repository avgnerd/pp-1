import { expect, test, type Page } from "@playwright/test"

const H = { "Content-Type": "application/json", "X-Requested-With": "dvfs" }

async function login(page: Page) {
  await page.goto("/login")
  await page.getByLabel("이름").fill("admin")
  await page.getByLabel("비밀번호").fill("1234")
  await page.getByRole("button", { name: "로그인" }).click()
  await expect(page).toHaveURL(/\/$/)
}

test.describe.configure({ mode: "serial" })

test("redirects to login and signs in", async ({ page }) => {
  await page.goto("/admin/settings")
  await expect(page).toHaveURL(/\/login/)
  await login(page)
  await expect(page.getByText("초기 비밀번호(1234)")).toBeVisible()
})

test("connects a storage with the localhost flow and browses it", async ({ page, context }) => {
  await login(page)
  await page.goto("/admin/storages")
  await page.getByRole("button", { name: "저장소 추가" }).click()
  const mount = `/e2e/${Date.now()}`
  await page.getByLabel("마운트 경로").fill(mount)
  await page.getByLabel("App key").fill("app1")
  await page.getByLabel("App secret").fill("secret")
  await page.getByRole("button", { name: "저장하고 인증하기" }).click()

  // The fake Dropbox redirects straight back to http://localhost:<port>/api/oauth/callback.
  const href = await page.getByRole("button", { name: "Dropbox에서 승인하기" }).getAttribute("href")
  const tab = await context.newPage()
  await tab.goto(href!)
  await expect(tab.getByRole("heading", { name: "연결 완료, 원래 탭으로 돌아가세요" })).toBeVisible()
  await tab.close()
  await expect(page.getByText("연결되었습니다", { exact: true })).toBeVisible({ timeout: 15_000 })
  await page.getByRole("button", { name: "완료" }).click()

  await page.goto(mount)
  await expect(page.getByRole("link", { name: "사진", exact: true })).toBeVisible()
  // Folder sizes are not computed until asked for.
  const row = page.getByRole("row").filter({ hasText: "사진" })
  await expect(row.getByRole("cell").nth(3)).toHaveText("—")
  await page.getByRole("link", { name: "사진", exact: true }).click({ button: "right" })
  await page.getByRole("menuitem", { name: "폴더 크기" }).click()
  await expect(row.getByRole("cell").nth(3)).toHaveText(/\d/, { timeout: 15_000 })

  // Sorting: clicking the same header twice flips the order.
  await page.goto(`${mount}/${encodeURIComponent("사진")}/2024`)
  await expect(page.getByRole("link", { name: "IMG_0001.jpg" })).toBeVisible()
  await page.getByRole("button", { name: "이름" }).click()
  await expect(page.locator("[role=row] a").first()).toHaveText("IMG_0040.jpg")
  await page.getByRole("button", { name: "이름" }).click()
  await expect(page.locator("[role=row] a").first()).toHaveText("IMG_0001.jpg")
})

test("robots.txt default blocks crawlers", async ({ request }) => {
  const r = await request.get("/robots.txt")
  expect(await r.text()).toBe("User-agent: *\nDisallow: /")
})

test("CSRF header is required for writes", async ({ request }) => {
  const r = await request.post("/api/auth/login", { data: { username: "admin", password: "1234" } })
  expect(r.status()).toBe(403)
  const ok = await request.post("/api/auth/login", { data: { username: "admin", password: "1234" }, headers: H })
  expect(ok.status()).toBe(200)
})
