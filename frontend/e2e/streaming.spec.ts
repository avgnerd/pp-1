import { expect, test, type Page } from "@playwright/test"

async function ready(page: Page) {
  await page.goto("/demo.mp4")
  await expect(page.getByRole("button", { name: "Settings", exact: true })).toBeVisible()
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(1)
}

async function convert(page: Page) {
  await page.getByRole("button", { name: "Settings", exact: true }).click()
  await page.getByRole("menuitem", { name: /화질/ }).click()
  const choice = page.getByRole("menuitemradio", { name: /변환 재생/ })
  const menu = page.getByRole("menu").filter({ has: choice })
  await expect(menu).toBeVisible()
  await expect(menu).not.toHaveAttribute("data-starting-style", "")
  const [response] = await Promise.all([
    page.waitForResponse(response => response.request().method() === "POST" && new URL(response.url()).pathname === "/api/media/hls"),
    choice.click(),
  ])
  expect(response.ok()).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentSrc.startsWith("blob:"))).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.duration)).toBe(240)
  await page.keyboard.press("Escape")
}

for (const playing of [false, true]) {
  test(`pre-metadata exhaustion retains ${playing ? "playing" : "paused"} intent through manual retry`, async ({ page }) => {
    await ready(page)
    await convert(page)
    await page.locator("video").evaluate(async (v: HTMLVideoElement) => { v.muted = true; await v.play() })
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.getVideoPlaybackQuality().totalVideoFrames)).toBeGreaterThan(5)
    await page.locator("video").evaluate((v: HTMLVideoElement, play) => { v.playbackRate = 1.75; if (!play) v.pause() }, playing)
    const baseline = await page.locator("video").evaluate((v: HTMLVideoElement) => ({ time: v.currentTime, rate: v.playbackRate, paused: v.paused, frames: v.getVideoPlaybackQuality().totalVideoFrames }))
    expect(baseline.rate).toBe(1.75)
    expect(baseline.paused).toBe(!playing)
    let failures = 0
    const failedMedia: { time: number; rate: number; paused: boolean; ready: number; frames: number }[] = []
    await page.route("**/seg*.ts", async route => {
      failedMedia.push(await page.locator("video").evaluate((v: HTMLVideoElement) => ({ time: v.currentTime, rate: v.playbackRate, paused: v.paused, ready: v.readyState, frames: v.getVideoPlaybackQuality().totalVideoFrames })))
      failures++
      await route.fulfill({ status: 410, json: { code: "past_end" } })
    })
    await page.locator("video").evaluate((v: HTMLVideoElement) => { v.currentTime = 200 })
    const retry = page.getByRole("button", { name: "다시 재생", exact: true })
    await expect(retry).toBeVisible()
    expect(failures).toBeGreaterThanOrEqual(3)
    const emptyReplacements = failedMedia.filter(media => media.ready === 0)
    expect(emptyReplacements.length).toBeGreaterThanOrEqual(2)
    for (const media of emptyReplacements) expect(media).toEqual({ time: 0, rate: 1, paused: true, ready: 0, frames: 0 })
    await page.unroute("**/seg*.ts")
    await retry.click()
    await expect(retry).not.toBeVisible()
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    const restored = await page.locator("video").evaluate((v: HTMLVideoElement) => ({ time: v.currentTime, rate: v.playbackRate, paused: v.paused, frames: v.getVideoPlaybackQuality().totalVideoFrames }))
    await test.info().attach("pre-metadata-recovery", { body: JSON.stringify({ baseline, failures, failedMedia, restored }), contentType: "application/json" })
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThanOrEqual(199.5)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeLessThan(210)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.75)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(!playing)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.getVideoPlaybackQuality().totalVideoFrames)).toBeGreaterThan(0)
    if (playing) {
      const before = await page.locator("video").evaluate((v: HTMLVideoElement) => ({ time: v.currentTime, frames: v.getVideoPlaybackQuality().totalVideoFrames }))
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThan(before.time + 0.5)
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.getVideoPlaybackQuality().totalVideoFrames)).toBeGreaterThan(before.frames + 4)
    }
  })
}

for (const { playing, fullscreen } of [{ playing: false, fullscreen: false }, { playing: true, fullscreen: false }, { playing: true, fullscreen: true }]) {
  test(`persistent past-end failures show a retry action and recover ${playing ? "playing" : "paused"} playback${fullscreen ? " in native fullscreen" : ""}`, async ({ page }) => {
    test.setTimeout(90_000)
    await ready(page)
    await convert(page)

    await page.locator("video").evaluate(async (v: HTMLVideoElement) => { v.currentTime = 200; await v.play() })
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThan(201)
    await page.reload()
    await ready(page)
    await convert(page)
    if (fullscreen) {
      await page.getByRole("button", { name: "Enter fullscreen", exact: true }).click()
      await expect.poll(() => page.evaluate(() => !!document.fullscreenElement)).toBe(true)
    }

    let healthy = false
    let failures = 0
    let opens = 0
    const pings: number[] = []
    page.on("request", request => {
      if (request.method() === "POST" && new URL(request.url()).pathname === "/api/media/hls") opens++
    })
    page.on("response", response => {
      if (new URL(response.url()).pathname.endsWith("/ping")) pings.push(response.status())
    })
    await page.route("**/seg*.ts", async route => {
      const segment = /seg(\d+)\.ts$/.exec(new URL(route.request().url()).pathname)
      if (!healthy && segment && Number(segment[1]) >= 50) {
        failures++
        await route.fulfill({ status: 410, json: { code: "past_end" } })
      } else await route.continue()
    })
    await page.locator("video").evaluate(async (v: HTMLVideoElement, play) => {
      v.playbackRate = 1.75
      if (play) await v.play()
      else v.pause()
      v.currentTime = 200
    }, playing)
    await expect.poll(() => failures).toBeGreaterThanOrEqual(3)
    await expect.poll(() => opens).toBe(2)
    const dialog = page.getByRole("alertdialog")
    await expect(dialog).toBeVisible()
    await expect(dialog).toContainText("다시 재생")
    const retry = page.getByRole("button", { name: "다시 재생", exact: true })
    await expect(retry).toBeVisible()
    if (fullscreen) await expect.poll(() => retry.evaluate(button => document.fullscreenElement?.contains(button))).toBe(true)
    expect(opens).toBe(2)
    const heartbeat = await page.waitForResponse(response => new URL(response.url()).pathname.endsWith("/ping"), { timeout: 25_000 })
    expect(heartbeat.status()).toBe(200)
    expect(pings.length).toBeGreaterThanOrEqual(3)
    expect(pings.every(status => status === 200)).toBe(true)
    expect(opens).toBe(2)
    await expect(page).toHaveURL(/\/demo\.mp4$/)

    await retry.click()
    await expect.poll(() => opens).toBe(5)
    await expect(dialog).toBeVisible()
    await expect(retry).toBeVisible()
    healthy = true
    const old = await page.locator("video").elementHandle()
    await retry.click()
    await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
    await expect(dialog).not.toBeVisible()
    await expect(retry).not.toBeVisible()
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(!playing)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.75)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThanOrEqual(199.5)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeLessThan(210)
    if (playing) {
      const position = await page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThan(position + 0.5)
    }
    if (fullscreen) await expect.poll(() => page.evaluate(() => !!document.fullscreenElement)).toBe(true)
  })
}

test("returning to a cached HLS source clears the previous terminal failure", async ({ page }) => {
  await ready(page)
  let playlist = ""
  let opens = 0
  page.on("request", request => {
    const path = new URL(request.url()).pathname
    if (path.endsWith("/index.m3u8")) playlist = path
    if (request.method() === "POST" && path === "/api/media/hls") opens++
  })
  await convert(page)
  let healthy = false
  await page.route("**/seg*.ts", async route => {
    const segment = /seg(\d+)\.ts$/.exec(new URL(route.request().url()).pathname)
    if (!healthy && segment && Number(segment[1]) >= 50) {
      await route.fulfill({ status: 410, json: { code: "past_end" } })
    } else await route.continue()
  })
  await page.locator("video").evaluate((v: HTMLVideoElement) => { v.pause(); v.currentTime = 200 })
  const dialog = page.getByRole("alertdialog")
  const retry = page.getByRole("button", { name: "다시 재생", exact: true })
  await expect(dialog).toBeVisible()
  await expect(retry).toBeVisible()
  expect(opens).toBe(3)
  const failedPlaylist = playlist
  healthy = true
  await page.keyboard.press("Escape")

  for (const name of [/^원본/, /변환 재생/]) {
    await page.getByRole("button", { name: "Settings", exact: true }).click()
    await page.getByRole("menuitem", { name: /화질/ }).click()
    const choice = page.getByRole("menuitemradio", { name })
    const menu = page.getByRole("menu").filter({ has: choice })
    await expect(menu).toBeVisible()
    await expect(menu).not.toHaveAttribute("data-starting-style", "")
    const old = await page.locator("video").elementHandle()
    await choice.click()
    await page.keyboard.press("Escape")
    await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
  }
  expect(opens).toBe(3)
  expect(playlist).toBe(failedPlaylist)
  await expect(dialog).not.toBeVisible()
  await expect(retry).not.toBeVisible()
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(200)
  await page.locator("video").evaluate(async (v: HTMLVideoElement) => { await v.play() })
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThan(200.5)
})

test("an unrelated audio error retains the default dismiss action", async ({ page }) => {
  await page.route("**/api/config", async route => {
    const response = await route.fetch()
    await route.fulfill({ json: { ...await response.json(), transcode: false } })
  })
  await page.route("**/api/fs/stat?*", route => route.fulfill({ json: { name: "demo.mp3", path: "/demo.mp3", type: "file", rev: "audio-fixture", size: 1000 } }))
  await page.goto("/demo.mp3")
  await expect.poll(() => page.locator("audio").evaluate((audio: HTMLAudioElement) => audio.readyState)).toBeGreaterThanOrEqual(1)
  await page.locator("audio").evaluate((audio: HTMLAudioElement) => { audio.src = "data:audio/mp3;base64,AAAA"; audio.load() })
  const dialog = page.getByRole("alertdialog")
  await expect(dialog).toBeVisible()
  await expect(dialog.getByRole("button", { name: "다시 재생", exact: true })).toHaveCount(0)
  await dialog.getByRole("button").click()
  await expect(dialog).not.toBeVisible()
})

test("VR renders current decoded frames after conversion, HLS recovery and a fresh link", async ({ page }) => {
  await page.addInitScript(() => {
    const frames = new WeakMap<HTMLVideoElement, number>()
    const original = HTMLVideoElement.prototype.requestVideoFrameCallback
    HTMLVideoElement.prototype.requestVideoFrameCallback = function (callback) {
      return original.call(this, (now, metadata) => {
        frames.set(this, (frames.get(this) ?? 0) + 1)
        this.dataset.vrFrames = String(frames.get(this))
        callback(now, metadata)
      })
    }
    const upload = WebGL2RenderingContext.prototype.texImage2D
    WebGL2RenderingContext.prototype.texImage2D = function (this: WebGL2RenderingContext, ...args: unknown[]) {
      const source = args.at(-1)
      if (source instanceof HTMLVideoElement) {
        document.querySelectorAll("video[data-vr-upload]").forEach(v => v.removeAttribute("data-vr-upload"))
        source.dataset.vrUpload = "true"
      }
      return Reflect.apply(upload, this, args)
    }
  })
  await ready(page)
  await page.getByRole("button", { name: "Settings", exact: true }).click()
  await page.getByRole("menuitem", { name: "VR 일반 영상" }).click()
  await page.getByRole("menuitemradio", { name: "360°단일", exact: true }).click()
  await page.keyboard.press("Escape")
  await expect(page.locator("[data-vr] canvas")).toBeVisible()
  await page.locator("video").evaluate(async (v: HTMLVideoElement) => { await v.play() })
  await expect(page.locator("video")).toHaveAttribute("data-vr-upload", "true")
  const old = await page.locator("video").elementHandle()
  await convert(page)
  await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
  await page.locator("video").evaluate(async (v: HTMLVideoElement) => { await v.play() })
  await expect(page.locator("video")).toHaveAttribute("data-vr-upload", "true")
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => Number(v.dataset.vrFrames ?? 0))).toBeGreaterThan(1)

  const converted = await page.locator("video").elementHandle()
  let injected = false
  await page.route("**/seg*.ts", async route => {
    if (!injected && /seg(4[5-9]|5[0-9])\.ts/.test(route.request().url())) {
      injected = true
      await route.fulfill({ status: 410, body: "expired session" })
    } else await route.continue()
  })
  await page.locator("video").evaluate((v: HTMLVideoElement) => { v.currentTime = 200 })
  await expect.poll(() => injected).toBe(true)
  await expect.poll(() => converted?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
  await expect(page.locator("video")).toHaveAttribute("data-vr-upload", "true")
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => Number(v.dataset.vrFrames ?? 0))).toBeGreaterThan(1)

  await page.getByRole("button", { name: "Settings", exact: true }).click()
  await page.getByRole("menuitem", { name: /화질/ }).click()
  await page.getByRole("menuitemradio", { name: /^원본/ }).click()
  await page.keyboard.press("Escape")
  await expect(page.locator("video")).toHaveAttribute("src", /api\/fs\/raw/)
  await expect(page.locator("video")).toHaveAttribute("data-vr-upload", "true")
  const original = await page.locator("video").elementHandle()
  await page.locator("video").evaluate((v: HTMLVideoElement) => {
    Object.defineProperty(v, "error", { configurable: true, value: { code: 2, message: "injected original network failure" } })
    v.dispatchEvent(new Event("error"))
  })
  await expect.poll(() => original?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
  await expect(page.locator("video")).toHaveAttribute("src", /fresh=/)
  await expect(page.locator("video")).toHaveAttribute("data-vr-upload", "true")
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => Number(v.dataset.vrFrames ?? 0))).toBeGreaterThan(1)
  await expect(page.locator("[data-vr] canvas")).toHaveCount(1)
})

for (const playing of [false, true]) {
  test(`HLS 410 recovery retains seek position and ${playing ? "playing" : "paused"} intent`, async ({ page }) => {
    await ready(page)
    await convert(page)
    await page.locator("video").evaluate(async (v: HTMLVideoElement, play) => { v.playbackRate = 1.75; if (play) await v.play(); else v.pause() }, playing)
    const old = await page.locator("video").elementHandle()
    let injected = 0
    await page.route("**/seg*.ts", async route => {
      if (!injected && /seg(4[5-9]|5[0-9])\.ts/.test(route.request().url())) {
        injected++
        await route.fulfill({ status: 410, body: "expired session" })
      } else await route.continue()
    })
    await page.locator("video").evaluate((v: HTMLVideoElement) => { v.currentTime = 200 })
    await expect.poll(() => injected).toBe(1)
    await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(!playing)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.75)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThanOrEqual(199.5)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeLessThan(210)
    if (playing) {
      const start = await page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThan(start + 0.5)
    }
  })
}

for (const waitingOn of ["ping", "session"] as const) {
  test(`pause while awaiting HLS ${waitingOn} wins over recovery`, async ({ page }) => {
    await ready(page)
    await convert(page)
    await page.locator("video").evaluate(async (v: HTMLVideoElement) => { await v.play() })
    const old = await page.locator("video").elementHandle()
    let release = () => {}
    let pending = false
    let injected = false
    await page.route(waitingOn === "ping" ? "**/api/media/hls/*/ping" : "**/api/media/hls", async route => {
      pending = true
      await new Promise<void>(resolve => { release = resolve })
      await route.continue()
    })
    await page.route("**/seg*.ts", async route => {
      if (!injected && /seg(4[5-9]|5[0-9])\.ts/.test(route.request().url())) {
        injected = true
        await route.fulfill({ status: 410, body: "expired session" })
      } else await route.continue()
    })
    try {
      await page.locator("video").evaluate((v: HTMLVideoElement) => { v.currentTime = 200 })
      await expect.poll(() => pending).toBe(true)
      await page.locator("video").evaluate((v: HTMLVideoElement) => { v.pause(); v.playbackRate = 1.5 })
    } finally {
      release()
    }
    await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(200, 0)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.5)
  })
}

for (const code of [2, 3]) {
  test(`paused original error ${code} retains intent despite configured autoplay`, async ({ page }) => {
    await ready(page)
    await page.locator("video").evaluate((v: HTMLVideoElement) => { v.pause(); v.currentTime = 12.5; v.playbackRate = 1.75 })
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(12.5)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.75)
    const old = await page.locator("video").elementHandle()
    await page.locator("video").evaluate((v: HTMLVideoElement, errorCode) => {
      Object.defineProperty(v, "error", { configurable: true, value: { code: errorCode, message: "injected original media failure" } })
      v.dispatchEvent(new Event("error"))
    }, code)
    await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(12.5)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.75)
    if (code === 2) await expect(page.locator("video")).toHaveAttribute("src", /fresh=/)
    else await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentSrc.startsWith("blob:"))).toBe(true)
  })
}

test("an old HLS recovery response cannot remount the user's new original source", async ({ page }) => {
  await ready(page)
  const manifestRequest = page.waitForRequest("**/media/*/index.m3u8")
  await convert(page)
  const playlist = new URL((await manifestRequest).url()).pathname
  const id = playlist.split("/")[2]
  let pending = false
  let release = () => {}
  let injected = false
  await page.route("**/api/media/hls", async route => {
    pending = true
    await new Promise<void>(resolve => { release = resolve })
    await route.fulfill({ json: { id, playlist, mode: "transcode", duration: 240 } })
  })
  await page.route("**/seg*.ts", async route => {
    if (!injected && /seg(4[5-9]|5[0-9])\.ts/.test(route.request().url())) {
      injected = true
      await route.fulfill({ status: 410, body: "expired session" })
    } else await route.continue()
  })
  let replacement
  try {
    await page.locator("video").evaluate((v: HTMLVideoElement) => { v.currentTime = 200 })
    await expect.poll(() => pending).toBe(true)
    await page.getByRole("button", { name: "Settings", exact: true }).click()
    await page.getByRole("menuitem", { name: /화질/ }).click()
    await page.getByRole("menuitemradio", { name: /^원본/ }).click()
    await page.keyboard.press("Escape")
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentSrc.includes("/api/fs/raw"))).toBe(true)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    await page.locator("video").evaluate((v: HTMLVideoElement) => { v.pause(); v.currentTime = 55 })
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(55)
    replacement = await page.locator("video").elementHandle()
  } finally {
    release()
  }
  await page.waitForResponse(response => response.url().endsWith("/api/media/hls"))
  await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
  await expect.poll(() => replacement?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(55)
})

test("conversion retains the selected playback rate, pause and position", async ({ page }) => {
  await ready(page)
  await page.locator("video").evaluate((v: HTMLVideoElement) => { v.pause(); v.currentTime = 12.5 })
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(12.5)
  await page.getByRole("button", { name: "Settings", exact: true }).click()
  await page.getByRole("menuitem", { name: /Speed/ }).click()
  const rate = page.getByRole("menuitemradio", { name: "1.7×", exact: true })
  const menu = page.getByRole("menu").filter({ has: rate })
  await expect(menu).toBeVisible()
  await expect(menu).not.toHaveAttribute("data-starting-style", "")
  await rate.click()
  await page.keyboard.press("Escape")
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.7)
  await convert(page)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => ({ rate: v.playbackRate, paused: v.paused, time: v.currentTime }))).toEqual({ rate: 1.7, paused: true, time: 12.5 })
  await page.getByRole("button", { name: "Settings", exact: true }).click()
  await expect(page.getByRole("menuitem", { name: /Speed.*1\.7/ })).toBeVisible()
})

for (const autoNext of [false, true]) {
  test(`a valid near-end segment failure recovers with auto-next ${autoNext ? "on" : "off"}`, async ({ page, request }) => {
    await page.addInitScript(on => localStorage.setItem("dvfs-prefs", JSON.stringify({ state: { autoNext: on }, version: 0 })), autoNext)
    await ready(page)
    const manifest = page.waitForRequest("**/media/*/index.m3u8")
    await convert(page)
    const playlist = (await manifest).url()
    expect((await request.get(new URL("seg58.ts", playlist).href)).ok()).toBe(true)
    await page.locator("video").evaluate((v: HTMLVideoElement) => v.pause())
    const old = await page.locator("video").elementHandle()
    let injected = false
    await page.route("**/seg58.ts", async route => {
      if (!injected) {
        injected = true
        await route.fulfill({ status: 410, body: "temporary failure of an existing segment" })
      } else await route.continue()
    })
    await page.locator("video").evaluate((v: HTMLVideoElement) => { v.currentTime = 232 })
    await expect.poll(() => injected).toBe(true)
    await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
    await expect(page).toHaveURL(/\/demo\.mp4$/)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(232)
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
  })
}

test("a natural HLS end still advances to the next file", async ({ page }) => {
  await ready(page)
  await convert(page)
  await page.locator("video").evaluate(async (v: HTMLVideoElement) => { v.currentTime = 239; await v.play() })
  await expect(page).toHaveURL(/\/next\.mp4$/)
})

test("a current HLS recovery reloads an unchanged playlist while retaining pause and seek", async ({ page }) => {
  await ready(page)
  const manifestRequest = page.waitForRequest("**/media/*/index.m3u8")
  await convert(page)
  const playlist = new URL((await manifestRequest).url()).pathname
  const id = playlist.split("/")[2]
  await page.route("**/api/media/hls", route => route.fulfill({ json: { id, playlist, mode: "transcode", duration: 240 } }))
  let injected = false
  await page.route("**/seg*.ts", async route => {
    if (!injected && /seg(4[5-9]|5[0-9])\.ts/.test(route.request().url())) {
      injected = true
      await route.fulfill({ status: 410, body: "expired session" })
    } else await route.continue()
  })
  const old = await page.locator("video").elementHandle()
  await page.locator("video").evaluate((v: HTMLVideoElement) => { v.pause(); v.currentTime = 200 })
  await expect.poll(() => injected).toBe(true)
  await expect.poll(() => old?.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(200)
})
