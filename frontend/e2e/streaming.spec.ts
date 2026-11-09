import { expect, test, type Locator, type Page } from "@playwright/test"

type PlaybackState = { time: number; rate: number; paused: boolean; readyState: number; src: string; frames: number }
type PlaybackObservation = { state: PlaybackState; advanced: number | null; frames: number }
type PlaybackWatch =
  | { kind: "advance"; position?: number; target: number | "advance"; deadlineMs: number; minTime?: number; srcPrefix?: string; pauseAfter?: boolean }
  | { kind: "recovery"; minTime: number; advanceBy: number; captureDeadlineMs: number; advanceWindowMs: number }
type LastValidState = { time: number; rate: number; paused: boolean; src: string; frames: number }

// A restored position is compared with the last valid state captured before the trigger, not an
// absolute seek value. The player saves timeupdate state and the fallback sample runs every frame;
// one second covers the event cadence plus the first rendered frame after the restore, while still
// rejecting an unrelated source (for example next.mp4 at 0).
const RECOVERY_POSITION_TOLERANCE = 1

const openRootMenu = (page: Page) => page.locator('[role="menu"][data-open]:not([data-submenu])')
const openSubmenuPanel = (page: Page) => page.locator('[role="menu"][data-submenu][data-open]')

// Base UI focuses a menu one task after it opens and the browser scrolls the page to reveal the
// focused item. The popover is anchored to the controls, so a click sent while the page, focus or
// popover is still moving presses the old screen position and never reaches the item. Wait for any
// dialog to close, the menu attributes to settle, focus to enter the menu, and its box and the page
// scroll to stay still; the normal locator click follows.
async function settleMenu(menu: Locator, requireFocus = false) {
  await expect(menu).toBeVisible()
  await expect(menu).not.toHaveAttribute("data-starting-style", "")
  // Base UI focuses the selected item when a submenu opens, but a root menu can keep focus on the
  // trigger, so only the submenu requires focus to be inside.
  if (requireFocus) await expect.poll(() => menu.evaluate(node => node.contains(document.activeElement))).toBe(true)
  await menu.evaluate(node => new Promise<void>(resolve => {
    let lastY = window.scrollY
    let last = node.getBoundingClientRect()
    let stable = 0
    const tick = () => {
      const box = node.getBoundingClientRect()
      if (window.scrollY === lastY && box.top === last.top && box.left === last.left && box.width === last.width && box.height === last.height) stable++
      else { stable = 0; lastY = window.scrollY; last = box }
      if (stable >= 2) resolve()
      else requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  }))
}

async function openSubmenu(page: Page, title: RegExp | string): Promise<Locator> {
  await expect(page.getByRole("alertdialog")).toBeHidden()
  const player = page.getByRole("group", { name: "Media player" })
  // The controls auto-hide after a few idle seconds. A normal hover over the player shows them
  // again; Playwright still treats opacity 0 as visible, so confirm the pointer reaches the button.
  await player.hover()
  await expect(player).toHaveAttribute("data-controls-visible", "")
  const settings = page.getByRole("button", { name: "Settings", exact: true })
  await expect(settings).toBeVisible()
  await expect.poll(() => settings.evaluate(button => {
    const rect = button.getBoundingClientRect()
    const hit = document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2)
    return hit !== null && (hit === button || button.contains(hit))
  })).toBe(true)
  await settings.click()
  await settleMenu(openRootMenu(page))
  await page.getByRole("menuitem", { name: title }).click()
  const menu = openSubmenuPanel(page)
  await settleMenu(menu, true)
  return menu
}

type PlaybackWatchHandle = { id: string }

// Install an in-page playback observation and return only after it is scheduled. The caller can
// then dispatch the trigger (a retry click) with no gap in which the replacement could play, reach
// the end and auto-next before the observation exists. "advance" optionally seeks and starts
// playback, then waits for currentTime to pass an absolute target or the captured time + 0.5 s.
// "recovery" waits for the replacement element, captures it once it plays at the restored position,
// proves the same advance, and pauses it. The result promise never rejects, settles exactly once,
// and stop() cancels the rAF loop; every caller cancels in a finally.
async function installPlaybackWatch(page: Page, spec: PlaybackWatch, previous?: Element): Promise<PlaybackWatchHandle> {
  const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  await page.evaluate(({ watchSpec, previousElement, token }) => {
    type State = { time: number; rate: number; paused: boolean; readyState: number; src: string; frames: number }
    type Observation = { state: State; advanced: number | null; frames: number }
    type PageWatch = { token: string; result: Promise<Observation | null>; stop: () => void }
    const scope = window as Window & { __dvfsPlaybackWatch?: PageWatch }
    scope.__dvfsPlaybackWatch?.stop()
    let frame = 0
    let state: State | null = null
    let target = 0
    let settled = false
    let resolveResult: (value: Observation | null) => void = () => {}
    const result = new Promise<Observation | null>(resolve => { resolveResult = resolve })
    const start = performance.now()
    let deadline = watchSpec.kind === "advance" ? start + watchSpec.deadlineMs : start + watchSpec.captureDeadlineMs
    const finish = (video: HTMLVideoElement | null, advanced: number | null) => {
      if (settled) return
      settled = true
      if (frame) cancelAnimationFrame(frame)
      if (watchSpec.kind === "recovery" || (watchSpec.kind === "advance" && watchSpec.pauseAfter)) video?.pause()
      resolveResult(state ? { state, advanced, frames: video?.getVideoPlaybackQuality?.().totalVideoFrames ?? 0 } : null)
    }
    const stop = () => {
      if (settled) return
      settled = true
      if (frame) cancelAnimationFrame(frame)
      resolveResult(null)
    }
    scope.__dvfsPlaybackWatch = { token, result, stop }
    if (watchSpec.kind === "advance" && watchSpec.position !== undefined) {
      const video = document.querySelector("video")
      if (video) {
        video.currentTime = watchSpec.position
        try {
          const playing = video.play()
          if (playing) void playing.catch(() => finish(video, null))
        } catch {
          finish(video, null)
        }
      }
    }
    const tick = () => {
      if (settled) return
      const video = document.querySelector("video")
      // Check the active phase deadline before capturing: a frame arriving after the capture
      // budget must not capture and receive a fresh advance window.
      if (performance.now() > deadline) return finish(video, null)
      const replaced = video !== null && (previousElement ? video !== previousElement && !previousElement.isConnected : true)
      const qualifies = video !== null && replaced && video.readyState >= 2 && !video.paused && !video.seeking
        && (watchSpec.kind !== "advance" || ((watchSpec.minTime === undefined || video.currentTime >= watchSpec.minTime)
          && (watchSpec.srcPrefix === undefined || video.currentSrc.startsWith(watchSpec.srcPrefix))))
      if (!state && qualifies && video) {
        const frames = video.getVideoPlaybackQuality?.().totalVideoFrames ?? 0
        if (watchSpec.kind === "advance") {
          state = { time: video.currentTime, rate: video.playbackRate, paused: video.paused, readyState: video.readyState, src: video.currentSrc, frames }
          target = watchSpec.target === "advance" ? state.time + 0.5 : watchSpec.target
        } else if (video.currentTime >= watchSpec.minTime) {
          state = { time: video.currentTime, rate: video.playbackRate, paused: video.paused, readyState: video.readyState, src: video.currentSrc, frames }
          target = state.time + watchSpec.advanceBy
          deadline = performance.now() + watchSpec.advanceWindowMs
        }
      }
      if (state && video && video.currentTime > target) return finish(video, video.currentTime)
      frame = requestAnimationFrame(tick)
    }
    tick()
  }, { watchSpec: spec, previousElement: previous, token: id })
  return { id }
}

async function playbackWatchResult(page: Page, handle: PlaybackWatchHandle): Promise<PlaybackObservation | null> {
  return page.evaluate((token) => {
    type State = { time: number; rate: number; paused: boolean; readyState: number; src: string; frames: number }
    const scope = window as Window & { __dvfsPlaybackWatch?: { token: string; result: Promise<{ state: State; advanced: number | null; frames: number } | null> } }
    if (scope.__dvfsPlaybackWatch?.token !== token) return null
    return scope.__dvfsPlaybackWatch.result
  }, handle.id)
}

async function cancelPlaybackWatch(page: Page, handle: PlaybackWatchHandle): Promise<void> {
  await page.evaluate((token) => {
    const scope = window as Window & { __dvfsPlaybackWatch?: { token: string; stop: () => void } }
    if (scope.__dvfsPlaybackWatch?.token !== token) return
    scope.__dvfsPlaybackWatch.stop()
    delete scope.__dvfsPlaybackWatch
  }, handle.id)
}

// Record the last valid state of the element that played before the failure. The element may be
// disconnected and a replacement may appear; the expected position must come from this pre-failure
// element after the explicit seek, not from a replacement that loads at 0.
async function installLastValidSample(page: Page, element: Element, minimumTime: number): Promise<PlaybackWatchHandle> {
  const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  await page.evaluate(({ token, media, minimum }) => {
    type Last = { time: number; rate: number; paused: boolean; src: string; frames: number }
    type PageSample = { token: string; last: Last | null; stop: () => void }
    const scope = window as Window & { __dvfsLastValid?: PageSample }
    scope.__dvfsLastValid?.stop()
    const video = media as HTMLVideoElement
    const sample: PageSample = { token, last: null, stop: () => {} }
    const events = ["seeking", "timeupdate", "play", "pause", "ratechange"] as const
    let frame = 0
    let stopped = false
    const record = () => {
      if (stopped || !video) return
      if (!video.currentSrc.startsWith("blob:") || location.pathname !== "/demo.mp4") return
      // The explicit seek is the lower provenance bound; a reset to 0 after the failure must not
      // overwrite the position the user/test actually sought to.
      if (video.currentTime < minimum - 0.5) return
      sample.last = {
        time: video.currentTime,
        rate: video.playbackRate,
        paused: video.paused,
        src: video.currentSrc,
        frames: video.getVideoPlaybackQuality?.().totalVideoFrames ?? 0,
      }
    }
    sample.stop = () => {
      stopped = true
      if (frame) cancelAnimationFrame(frame)
      for (const event of events) video.removeEventListener(event, record)
    }
    scope.__dvfsLastValid = sample
    for (const event of events) video.addEventListener(event, record)
    record()
    const tick = () => {
      if (stopped) return
      record()
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
  }, { token: id, media: element, minimum: minimumTime })
  return { id }
}

async function readLastValidSample(page: Page, handle: PlaybackWatchHandle): Promise<LastValidState | null> {
  return page.evaluate((token) => {
    type Last = { time: number; rate: number; paused: boolean; src: string; frames: number }
    const scope = window as Window & { __dvfsLastValid?: { token: string; last: Last | null } }
    return scope.__dvfsLastValid?.token === token ? scope.__dvfsLastValid.last : null
  }, handle.id)
}

async function stopLastValidSample(page: Page, handle: PlaybackWatchHandle): Promise<void> {
  await page.evaluate((token) => {
    const scope = window as Window & { __dvfsLastValid?: { token: string; stop: () => void } }
    if (scope.__dvfsLastValid?.token !== token) return
    scope.__dvfsLastValid.stop()
    delete scope.__dvfsLastValid
  }, handle.id)
}

async function holdPlayback(page: Page) {
  return page.locator("video").evaluate((v: HTMLVideoElement) => {
    const wasPlaying = !v.paused && !v.ended
    v.pause()
    return { wasPlaying, time: v.currentTime, rate: v.playbackRate }
  })
}

async function ready(page: Page, navigate = true) {
  if (navigate) await page.goto("/demo.mp4")
  await expect(page.getByRole("button", { name: "Settings", exact: true })).toBeVisible()
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(1)
}

async function convert(page: Page) {
  await openSubmenu(page, /화질/)
  const choice = page.getByRole("menuitemradio", { name: /변환 재생/ })
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

test("initial converted playback advances before a real reload opens a new session", async ({ page }) => {
  test.setTimeout(90_000)
  const sessionIds: string[] = []
  page.on("response", response => {
    if (response.request().method() === "POST" && new URL(response.url()).pathname === "/api/media/hls") {
      void response.json().then((body: { id?: string }) => { if (body.id) sessionIds.push(body.id) }).catch(() => {})
    }
  })
  await ready(page)
  await convert(page)
  const advanceWatch = await installPlaybackWatch(page, { kind: "advance", position: 200, target: 201, deadlineMs: 15_000, pauseAfter: true })
  let ahead: PlaybackObservation | null = null
  try {
    ahead = await playbackWatchResult(page, advanceWatch)
  } finally {
    await cancelPlaybackWatch(page, advanceWatch).catch(() => {})
  }
  expect(ahead).not.toBeNull()
  if (!ahead?.advanced) throw new Error("playback did not advance past 201 within 15 s")
  expect(ahead.advanced).toBeGreaterThan(201)
  expect(ahead.state.readyState).toBeGreaterThanOrEqual(2)
  expect(ahead.state.time).toBeGreaterThanOrEqual(200)
  expect(ahead.state.src.startsWith("blob:")).toBe(true)
  await expect(page).toHaveURL(/\/demo\.mp4$/)
  await expect.poll(() => sessionIds.length).toBeGreaterThanOrEqual(1)
  const firstSession = sessionIds[0]
  await page.reload()
  await ready(page, false)
  await convert(page)
  await expect(page).toHaveURL(/\/demo\.mp4$/)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentSrc.startsWith("blob:"))).toBe(true)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.duration)).toBe(240)
  await expect.poll(() => sessionIds.length).toBeGreaterThanOrEqual(2)
  expect(sessionIds[1]).not.toBe(firstSession)
})

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
    const seekTarget = 200
    const seekVideo = await page.locator("video").elementHandle()
    if (!seekVideo) throw new Error("the converted video was not found before the failure")
    let sample: PlaybackWatchHandle | null = null
    let playbackWatch: PlaybackWatchHandle | null = null
    try {
      sample = await installLastValidSample(page, seekVideo, seekTarget)
      await page.route("**/seg*.ts", async route => {
        failedMedia.push(await page.locator("video").evaluate((v: HTMLVideoElement) => ({ time: v.currentTime, rate: v.playbackRate, paused: v.paused, ready: v.readyState, frames: v.getVideoPlaybackQuality().totalVideoFrames })))
        failures++
        await route.fulfill({ status: 410, json: { code: "past_end" } })
      })
      await seekVideo.evaluate((v: HTMLVideoElement, target) => { v.currentTime = target }, seekTarget)
      const retry = page.getByRole("button", { name: "다시 재생", exact: true })
      await expect(retry).toBeVisible()
      expect(failures).toBeGreaterThanOrEqual(3)
      const emptyReplacements = failedMedia.filter(media => media.ready === 0)
      expect(emptyReplacements.length).toBeGreaterThanOrEqual(2)
      for (const media of emptyReplacements) expect(media).toEqual({ time: 0, rate: 1, paused: true, ready: 0, frames: 0 })
      await page.unroute("**/seg*.ts")
      const expected = await readLastValidSample(page, sample)
      expect(expected).not.toBeNull()
      if (!expected) throw new Error("no valid demo media state was recorded before the retry")
      // The expected belongs to the pre-failure element after the explicit seek. A restore to 0 or
      // to a pre-seek position must fail here instead of being normalized by the ±1 comparison.
      expect(expected.time).toBeGreaterThanOrEqual(seekTarget - 0.5)
      expect(expected.time).toBeLessThan(240)
      expect(expected.src.startsWith("blob:")).toBe(true)
      const beforeRetry = await page.locator("video").elementHandle()
      if (!beforeRetry) throw new Error("the media element was not found before the retry")
      if (playing) {
        playbackWatch = await installPlaybackWatch(page, {
          kind: "advance",
          target: "advance",
          minTime: expected.time - RECOVERY_POSITION_TOLERANCE,
          srcPrefix: "blob:",
          deadlineMs: 15_000,
          pauseAfter: true,
        }, beforeRetry)
      }
      await retry.click()
      const observed = playbackWatch ? await playbackWatchResult(page, playbackWatch) : null
      await expect(retry).not.toBeVisible()
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
      if (playing) {
        expect(observed).not.toBeNull()
        if (!observed) throw new Error("playback did not resume after the manual retry")
        expect(observed.state.readyState).toBeGreaterThanOrEqual(2)
        expect(observed.state.rate).toBe(1.75)
        expect(observed.state.paused).toBe(false)
        expect(observed.state.src.startsWith("blob:")).toBe(true)
        expect(observed.state.time).toBeGreaterThanOrEqual(expected.time - RECOVERY_POSITION_TOLERANCE)
        expect(observed.state.time).toBeLessThanOrEqual(expected.time + RECOVERY_POSITION_TOLERANCE)
        expect(observed.state.frames).toBeGreaterThan(0)
        expect(observed.advanced).not.toBeNull()
        expect(observed.advanced ?? Number.NEGATIVE_INFINITY).toBeGreaterThan(observed.state.time + 0.5)
        expect(observed.frames).toBeGreaterThan(observed.state.frames + 4)
        await test.info().attach("pre-metadata-recovery", { body: JSON.stringify({ baseline, failures, failedMedia, expected, observed }), contentType: "application/json" })
        // The watch froze the state after it observed playback, rate, frames and the advance inside
        // the page, so the delayed protocol return cannot let the file auto-next.
        await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
      } else {
        const restored = await page.locator("video").evaluate((v: HTMLVideoElement) => ({ time: v.currentTime, rate: v.playbackRate, paused: v.paused, src: v.currentSrc, frames: v.getVideoPlaybackQuality().totalVideoFrames }))
        await test.info().attach("pre-metadata-recovery", { body: JSON.stringify({ baseline, failures, failedMedia, expected, restored }), contentType: "application/json" })
        expect(restored.paused).toBe(true)
        expect(restored.rate).toBe(1.75)
        expect(restored.src.startsWith("blob:")).toBe(true)
        expect(restored.frames).toBeGreaterThan(0)
        expect(restored.time).toBeGreaterThanOrEqual(expected.time - RECOVERY_POSITION_TOLERANCE)
        expect(restored.time).toBeLessThanOrEqual(expected.time + RECOVERY_POSITION_TOLERANCE)
      }
      await expect(page).toHaveURL(/\/demo\.mp4$/)
    } finally {
      if (playbackWatch) await cancelPlaybackWatch(page, playbackWatch).catch(() => {})
      if (sample) await stopLastValidSample(page, sample).catch(() => {})
    }
  })
}

for (const { playing, fullscreen } of [{ playing: false, fullscreen: false }, { playing: true, fullscreen: false }, { playing: true, fullscreen: true }]) {
  test(`persistent past-end failures show a retry action and recover ${playing ? "playing" : "paused"} playback${fullscreen ? " in native fullscreen" : ""}`, async ({ page }) => {
    test.setTimeout(90_000)
    await ready(page)
    await convert(page)

    await page.reload()
    await ready(page, false)
    await convert(page)
    if (fullscreen) {
      await page.getByRole("button", { name: "Enter fullscreen", exact: true }).click()
      await expect.poll(() => page.evaluate(() => !!document.fullscreenElement)).toBe(true)
    }
    const sampleVideo = playing ? await page.locator("video").elementHandle() : null
    if (playing && !sampleVideo) throw new Error("the converted video was not found before the failure")
    let sample: PlaybackWatchHandle | null = null
    // The sampler is installed before the heartbeat, so its cleanup must cover the heartbeat too.
    try {
      sample = sampleVideo ? await installLastValidSample(page, sampleVideo, 200) : null

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
    const seekVideo = playing ? sampleVideo : null
    if (seekVideo) await seekVideo.evaluate(async (v: HTMLVideoElement) => {
      v.playbackRate = 1.75
      await v.play()
      v.currentTime = 200
    })
    else await page.locator("video").evaluate((v: HTMLVideoElement) => {
      v.playbackRate = 1.75
      v.pause()
      v.currentTime = 200
    })
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
    if (!old) throw new Error("the video element was replaced before the retry")
    let recoveryWatch: PlaybackWatchHandle | null = null
    try {
      const expected = sample ? await readLastValidSample(page, sample) : null
      if (playing) {
        expect(expected).not.toBeNull()
        if (!expected) throw new Error("no valid demo media state was recorded before the retry")
        expect(expected.time).toBeGreaterThanOrEqual(199.5)
        expect(expected.time).toBeLessThan(240)
        expect(expected.src.startsWith("blob:")).toBe(true)
      }
      const expectedTime = expected?.time ?? 199.5
      // Install the observation before the retry input: if the click's response is slow, the
      // replacement can otherwise resume, advance and auto-next before an observation installed
      // after the click returns exists.
      recoveryWatch = playing
        ? await installPlaybackWatch(page, { kind: "recovery", minTime: expectedTime - RECOVERY_POSITION_TOLERANCE, advanceBy: 0.5, captureDeadlineMs: 30_000, advanceWindowMs: 15_000 }, old)
        : null
      await retry.click()
      const observed = recoveryWatch ? await playbackWatchResult(page, recoveryWatch) : null
      await expect.poll(() => old.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
      await expect(dialog).not.toBeVisible()
      await expect(retry).not.toBeVisible()
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2)
      await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentSrc.startsWith("blob:"))).toBe(true)
      if (playing) {
        expect(observed).not.toBeNull()
        if (!observed) throw new Error("playback did not resume after the manual retry")
        expect(observed.state.readyState).toBeGreaterThanOrEqual(2)
        expect(observed.state.rate).toBe(1.75)
        expect(observed.state.paused).toBe(false)
        expect(observed.state.src.startsWith("blob:")).toBe(true)
        expect(observed.state.time).toBeGreaterThanOrEqual(expectedTime - RECOVERY_POSITION_TOLERANCE)
        expect(observed.state.time).toBeLessThanOrEqual(expectedTime + RECOVERY_POSITION_TOLERANCE)
        expect(observed.advanced).not.toBeNull()
        expect(observed.advanced ?? Number.NEGATIVE_INFINITY).toBeGreaterThan(observed.state.time + 0.5)
        await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
      } else {
        await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
        await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.playbackRate)).toBe(1.75)
        await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThanOrEqual(199.5)
        await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeLessThan(210)
      }
      await expect(page).toHaveURL(/\/demo\.mp4$/)
      if (fullscreen) await expect.poll(() => page.evaluate(() => !!document.fullscreenElement)).toBe(true)
    } finally {
      if (recoveryWatch) await cancelPlaybackWatch(page, recoveryWatch).catch(() => {})
    }
    } finally {
      if (sample) await stopLastValidSample(page, sample).catch(() => {})
    }
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
    await openSubmenu(page, /화질/)
    const choice = page.getByRole("menuitemradio", { name })
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
  // The initial demo can already be playing; ready alone does not mean paused. Establish the
  // paused precondition explicitly and keep the observed initial state in the attachment.
  const heldBeforeMenu = await holdPlayback(page)
  await test.info().attach("vr-initial-hold", { body: JSON.stringify(heldBeforeMenu), contentType: "application/json" })
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
  await openSubmenu(page, "VR 일반 영상")
  await page.getByRole("menuitemradio", { name: "360°단일", exact: true }).click()
  await page.keyboard.press("Escape")
  await expect(page.locator("[data-vr] canvas")).toBeVisible()
  // Capture the VR element before play. The first played frames are the slowest part on a
  // software-rendered runner, so resolving this locator after play adds an avoidable wait there.
  // The handle below proves play/upload and the conversion replacement on the same element.
  const old = await page.locator("video").elementHandle()
  if (!old) throw new Error("VR video element is missing after enabling 360° mode")
  await old.evaluate(async (v: HTMLVideoElement) => { await v.play() })
  await expect.poll(() => old.evaluate((v: HTMLVideoElement) => v.dataset.vrUpload === "true")).toBe(true)
  // This is the only menu call while playing: pause explicitly as a user would so the auto-hiding
  // controls stay reachable across the ready-to-click delay. The post-conversion play below proves
  // the source replacement still plays. Reusing the handle avoids a second locator resolution, and
  // the current check fails loudly if anything replaced the element before conversion.
  const heldBeforeConvert = await old.evaluate((v: HTMLVideoElement) => {
    const wasPlaying = !v.paused && !v.ended
    v.pause()
    return { wasPlaying, time: v.currentTime, rate: v.playbackRate, current: v.isConnected && v === document.querySelector("video") }
  })
  if (!heldBeforeConvert.current) throw new Error("VR video element was replaced before conversion")
  await test.info().attach("vr-before-convert-hold", { body: JSON.stringify(heldBeforeConvert), contentType: "application/json" })
  await expect.poll(() => old.evaluate((v: HTMLVideoElement) => v.paused && v.isConnected && v === document.querySelector("video"))).toBe(true)
  await convert(page)
  await expect.poll(() => old.evaluate((v: HTMLVideoElement) => v.isConnected)).toBe(false)
  const converted = await page.locator("video").elementHandle()
  if (!converted) throw new Error("converted video element is missing after conversion")
  const convertedState = await converted.evaluate((v: HTMLVideoElement) => ({
    connected: v.isConnected,
    current: v === document.querySelector("video"),
    blob: v.currentSrc.startsWith("blob:"),
  }))
  if (!convertedState.connected || !convertedState.current || !convertedState.blob) {
    throw new Error(`converted video element is not the current HLS element: ${JSON.stringify(convertedState)}`)
  }
  await converted.evaluate(async (v: HTMLVideoElement) => { await v.play() })
  await expect.poll(() => converted.evaluate((v: HTMLVideoElement) => v.dataset.vrUpload === "true")).toBe(true)
  await expect.poll(() => converted.evaluate((v: HTMLVideoElement) => Number(v.dataset.vrFrames ?? 0))).toBeGreaterThan(1)
  let injected = false
  await page.route("**/seg*.ts", async route => {
    if (!injected && /seg(4[5-9]|5[0-9])\.ts/.test(route.request().url())) {
      injected = true
      await route.fulfill({ status: 410, body: "expired session" })
    } else await route.continue()
  })
  // Keep the converted handle honest through the HLS trigger. If anything replaced it, fail here
  // instead of letting the later disconnect assertion pass on an element that was already gone.
  const beforeTrigger = await converted.evaluate((v: HTMLVideoElement) => {
    const current = v.isConnected && v === document.querySelector("video")
    if (current) v.currentTime = 200
    return current
  })
  if (!beforeTrigger) throw new Error("converted video element was replaced before the HLS failure trigger")
  await expect.poll(() => injected).toBe(true)
  await expect.poll(() => page.evaluate((oldElement: HTMLVideoElement) => {
    const videos = document.querySelectorAll("video")
    const current = videos[0]
    return oldElement.isConnected === false &&
      videos.length === 1 &&
      current.dataset.vrUpload === "true" &&
      Number(current.dataset.vrFrames ?? 0) > 1
  }, converted)).toBe(true)

  // The recovered frames above are the playback assertion. The source switch below checks source
  // identity and rendering, not playing, so freeze the restored position before the menu sequence:
  // on a slow runner the remaining 40 s otherwise plays out and auto-next changes the page under
  // the assertions. The VR view still renders loadeddata/seeked frames while paused.
  const held = await holdPlayback(page)
  await test.info().attach("vr-hold", { body: JSON.stringify(held), contentType: "application/json" })
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)

  await openSubmenu(page, /화질/)
  await page.getByRole("menuitemradio", { name: /^원본/ }).click()
  await page.keyboard.press("Escape")
  await expect(page.locator("video")).toHaveAttribute("src", /api\/fs\/raw/)
  await expect(page.locator("video")).toHaveAttribute("data-vr-upload", "true")
  // The source switch replaced the element; freeze the new one before the error/fresh-link steps.
  const switched = await holdPlayback(page)
  await test.info().attach("vr-hold-switched", { body: JSON.stringify(switched), contentType: "application/json" })
  await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.paused)).toBe(true)
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
    await openSubmenu(page, /화질/)
    await page.getByRole("menuitemradio", { name: /^원본/ }).click()
    await page.keyboard.press("Escape")
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentSrc.includes("/api/fs/raw"))).toBe(true)
    // The replacement restores the pre-switch position from its loadedmetadata handler, and that event
    // is a queued task: readyState can already be HAVE_CURRENT_DATA before the handler runs and writes
    // the position back. Wait for the restore itself before seeking.
    await expect.poll(() => page.locator("video").evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThanOrEqual(199.5)
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
  await openSubmenu(page, /Speed/)
  await page.getByRole("menuitemradio", { name: "1.7×", exact: true }).click()
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

type ControlSurface = Awaited<ReturnType<typeof readControlSurface>>

async function readControlSurface(target: Locator) {
  return target.evaluate(element => {
    const css = getComputedStyle(element)
    const rect = element.getBoundingClientRect()
    return {
      opacity: Number(css.opacity),
      pointerEvents: css.pointerEvents,
      filter: css.filter,
      rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
    }
  })
}

async function readFocusRing(target: Locator) {
  return target.evaluate(element => {
    const css = getComputedStyle(element)
    return {
      visible: element.matches(":focus-visible"),
      style: css.outlineStyle,
      width: parseFloat(css.outlineWidth),
      color: css.outlineColor,
    }
  })
}

// A hidden control surface transitions opacity, translate, scale and filter. Reading computed styles
// in the same frame the vis idle flag drops still shows the visible values, so wait for the
// element's own CSS transitions to finish (or stay absent for several frames when the focused
// surface never starts one). This is event driven, not a fixed sleep.
async function settleControlTransition(target: Locator) {
  await target.evaluate(async element => {
    for (let round = 0; round < 30; round++) {
      const running = element.getAnimations()
      if (running.length > 0) {
        await Promise.all(running.map(animation => animation.finished.catch(() => {})))
        continue
      }
      let quiet = 0
      for (let frame = 0; frame < 3; frame++) {
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()))
        if (element.getAnimations().length === 0) quiet++
        else break
      }
      if (quiet === 3) return
    }
    throw new Error("media control transition did not settle")
  })
}

async function tabUntilFocused(page: Page, target: Locator, limit = 50) {
  for (let presses = 0; presses < limit; presses++) {
    if (await target.evaluate(element => element === document.activeElement)) return presses
    await page.keyboard.press("Tab")
  }
  throw new Error("real Tab input did not reach the target control")
}

async function waitForControlsIdle(page: Page) {
  const player = page.getByRole("group", { name: "Media player" })
  await expect.poll(() => player.evaluate(element => element.hasAttribute("data-controls-visible")), { timeout: 10_000 }).toBe(false)
}

function expectKeyboardFocus(ring: Awaited<ReturnType<typeof readFocusRing>>) {
  expect(ring.visible).toBe(true)
  expect(ring.style).toBe("solid")
  expect(ring.width).toBeGreaterThanOrEqual(2)
  expect(ring.color).not.toBe("rgba(0, 0, 0, 0)")
}

function expectSurfaceRetained(surface: ControlSurface, visible: ControlSurface) {
  expect(surface.opacity).toBeGreaterThan(0.99)
  expect(surface.pointerEvents).toBe("auto")
  expect(surface.filter === "none" || surface.filter === "blur(0px)").toBe(true)
  expect(Math.abs(surface.rect.width - visible.rect.width)).toBeLessThanOrEqual(1)
  expect(Math.abs(surface.rect.height - visible.rect.height)).toBeLessThanOrEqual(1)
  expect(Math.abs(surface.rect.x - visible.rect.x)).toBeLessThanOrEqual(1)
  expect(Math.abs(surface.rect.y - visible.rect.y)).toBeLessThanOrEqual(1)
}

function expectSurfaceHidden(surface: ControlSurface) {
  expect(surface.opacity).toBeLessThan(0.01)
  expect(surface.pointerEvents).toBe("none")
}

for (const layout of [
  { name: "wide", viewport: { width: 1440, height: 900 }, compact: false },
  { name: "compact", viewport: { width: 520, height: 900 }, compact: true },
]) {
  test(`${layout.name} keyboard focus keeps media controls usable through idle and releases after focus leaves`, async ({ page }) => {
    await page.setViewportSize(layout.viewport)
    await ready(page)
    const player = page.getByRole("group", { name: "Media player" })
    const content = page.locator("[data-interactive]")
    const settings = page.getByRole("button", { name: "Settings", exact: true })
    const fullscreen = page.getByRole("button", { name: "Enter fullscreen" })
    const surfaces = layout.compact ? [settings.locator("xpath=.."), fullscreen.locator("xpath=..")] : [content]

    await page.locator("video").evaluate(async (video: HTMLVideoElement) => {
      video.muted = true
      await video.play().catch(() => {})
    })
    await expect.poll(() => page.locator("video").evaluate((video: HTMLVideoElement) => video.paused)).toBe(false)
    const startTime = await page.locator("video").evaluate((video: HTMLVideoElement) => video.currentTime)
    await expect.poll(() => page.locator("video").evaluate((video: HTMLVideoElement) => video.currentTime)).toBeGreaterThan(startTime + 0.2)

    const containerWidth = await player.evaluate(element => element.getBoundingClientRect().width)
    if (layout.compact) expect(containerWidth).toBeLessThan(512)
    else expect(containerWidth).toBeGreaterThanOrEqual(512)

    await page.evaluate(() => document.body.focus())
    await tabUntilFocused(page, settings)
    await expect(settings).toBeFocused()
    expectKeyboardFocus(await readFocusRing(settings))
    const visible = await Promise.all(surfaces.map(readControlSurface))
    expect(await content.evaluate(element => element.hasAttribute("data-visible"))).toBe(true)

    await waitForControlsIdle(page)
    await Promise.all(surfaces.map(settleControlTransition))
    expect(await content.evaluate(element => element.hasAttribute("data-visible"))).toBe(false)
    expect(await settings.evaluate(element => element === document.activeElement)).toBe(true)
    const retained = await Promise.all(surfaces.map(readControlSurface))
    retained.forEach((surface, index) => expectSurfaceRetained(surface, visible[index]))
    expectKeyboardFocus(await readFocusRing(settings))
    await expect.poll(() => page.locator("video").evaluate((video: HTMLVideoElement) => video.paused)).toBe(false)
    await expect.poll(() => page.locator("video").evaluate((video: HTMLVideoElement) => video.currentTime)).toBeGreaterThan(startTime + 0.5)

    await settings.press("Enter")
    const menu = openRootMenu(page)
    await settleMenu(menu)
    await expect(page.getByRole("menuitem", { name: /화질/ })).toBeVisible()
    await page.keyboard.press("Escape")
    await expect(menu).toBeHidden()
    await expect(settings).toBeFocused()

    for (let presses = 0; presses < 20 && await content.evaluate(element => element.contains(document.activeElement)); presses++) {
      await page.keyboard.press("Tab")
    }
    expect(await content.evaluate(element => element.contains(document.activeElement))).toBe(false)
    await waitForControlsIdle(page)
    await Promise.all(surfaces.map(settleControlTransition))
    for (const surface of await Promise.all(surfaces.map(readControlSurface))) expectSurfaceHidden(surface)

    await player.hover()
    await page.getByRole("button", { name: /Show duration/ }).click()
    const pointerFocus = await page.evaluate(() => ({
      tag: document.activeElement?.tagName,
      focusVisible: document.activeElement?.matches(":focus-visible") ?? false,
    }))
    expect(pointerFocus).toEqual({ tag: "TIME", focusVisible: false })
    await waitForControlsIdle(page)
    await Promise.all(surfaces.map(settleControlTransition))
    for (const surface of await Promise.all(surfaces.map(readControlSurface))) expectSurfaceHidden(surface)

    if (layout.compact) {
      await tabUntilFocused(page, fullscreen, 20)
      await expect(fullscreen).toBeFocused()
      expectKeyboardFocus(await readFocusRing(fullscreen))
      await waitForControlsIdle(page)
      await Promise.all(surfaces.map(settleControlTransition))
      const topFocus = await Promise.all(surfaces.map(readControlSurface))
      topFocus.forEach((surface, index) => expectSurfaceRetained(surface, visible[index]))
      await page.keyboard.press("Tab")
      expect(await content.evaluate(element => element.contains(document.activeElement))).toBe(false)
      await waitForControlsIdle(page)
      await Promise.all(surfaces.map(settleControlTransition))
      for (const surface of await Promise.all(surfaces.map(readControlSurface))) expectSurfaceHidden(surface)
    }
  })
}

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
