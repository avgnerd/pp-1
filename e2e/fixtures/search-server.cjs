const path = require("node:path")
const fs = require("node:fs")
const net = require("node:net")
const { pathToFileURL } = require("node:url")
exports.startSearchServer = async function ({ repo, root, appRoot }) {
  const mod = (p) =>
    import(pathToFileURL(path.join(repo, "frontend/node_modules", p)).href)
  const { createServer } = await mod("vite/dist/node/index.js")
  const { default: react } = await mod("@vitejs/plugin-react/dist/index.js")
  const { default: tailwind } = await mod("@tailwindcss/vite/dist/index.mjs")
  let config = {
    pagination_type: "pages",
    per_page: 30,
    archive_preview: true,
    public_url: "",
    data_mode: "proxy",
    audio_autoplay: false,
    video_autoplay: false,
    transcode: false,
    probe: false,
  }
  const requests = []
  const streams = new Map()
  let renamed = false
  let mutationEntries = []
  const makeEntry = (name, type = "folder") => ({
    name,
    path: "/synthetic/" + name,
    type,
    size: type === "file" ? 10 : null,
    storage_id: 1,
  })
  const frame = (event, data) =>
    `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`
  const complete = (res, entries, errors = false, truncated = false) => {
    res.write(frame("start", { storages: 1 }))
    if (entries.length)
      res.write(
        frame("results", { storage_id: 1, mount: "/synthetic", entries })
      )
    res.write(
      frame("storage", {
        storage_id: 1,
        mount: "/synthetic",
        status: errors ? "error" : "done",
        count: entries.length,
        ...(truncated ? { truncated: true } : {}),
        ...(errors ? { error: "synthetic storage offline" } : {}),
      })
    )
    res.end(
      frame("done", {
        total: entries.length,
        storages: 1,
        ...(truncated ? { truncated: true } : {}),
      })
    )
  }
  const server = await createServer({
    configFile: false,
    root: appRoot,
    cacheDir: path.join(root, "vite-cache"),
    plugins: [
      react(),
      tailwind(),
      {
        name: "synthetic-only-api",
        configureServer(v) {
          v.middlewares.use((req, res, next) => {
            const u = new URL(req.url, "http://127.0.0.1")
            if (!u.pathname.startsWith("/api/")) return next()
            requests.push({
              method: req.method,
              path: u.pathname,
              query: Object.fromEntries(u.searchParams),
            })
            const json = (value, code = 200) => {
              res.writeHead(code, { "Content-Type": "application/json" })
              res.end(JSON.stringify(value))
            }
            if (u.pathname === "/api/auth/me")
              return json({ authenticated: true, username: "synthetic-user" })
            if (u.pathname === "/api/config") return json(config)
            if (u.pathname === "/api/mounts")
              return json([
                {
                  id: 1,
                  name: "Synthetic",
                  mount_path: "/synthetic",
                  status: "active",
                  account_email: "synthetic@example.invalid",
                  account_name: "Synthetic",
                },
              ])
            if (u.pathname === "/api/fs/stat") {
              if (
                renamed &&
                u.searchParams.get("path") === "/synthetic/rename-old"
              )
                return json({ error: "synthetic path no longer exists" }, 404)
              return json({
                name: u.searchParams.get("path").split("/").at(-1),
                path: u.searchParams.get("path"),
                type: "folder",
                size: null,
                storage_id: 1,
              })
            }
            if (u.pathname === "/api/fs/list" && u.searchParams.get("path") === "/synthetic")
              return json({ path: "/synthetic", virtual: false, entries: [makeEntry("dest")], total: 1, offset: 0 })
            if (["/api/fs/delete", "/api/fs/copy"].includes(u.pathname)) {
              const request = requests.at(-1)
              let body = ""
              req.on("data", (x) => (body += x))
              req.on("end", () => {
                const d = JSON.parse(body)
                request.body = d
                if (u.pathname === "/api/fs/delete")
                  mutationEntries = mutationEntries.filter((e) => !d.paths.includes(e.path))
                else
                  mutationEntries.push(...mutationEntries.filter((e) => d.paths.includes(e.path)).map((e) => ({ ...e, path: d.dest + "/" + e.name })))
                json({ ok: true, done: d.paths.length, failures: null })
              })
              return
            }
            if (u.pathname === "/api/fs/list")
              return json({
                path: u.searchParams.get("path"),
                virtual: false,
                entries: [],
                total: 0,
                offset: 0,
              })
            if (u.pathname === "/api/fs/rename") {
              const request = requests.at(-1)
              let body = ""
              req.on("data", (x) => (body += x))
              req.on("end", () => {
                const d = JSON.parse(body)
                request.body = d
                renamed = true
                json({ ok: true, path: "/synthetic/" + d.name })
              })
              return
            }
            if (u.pathname !== "/api/search")
              return json({ error: "Synthetic endpoint not implemented" }, 404)
            const q = u.searchParams.get("q")
            if (
              q === "http-failure" ||
              (q === "recover" &&
                requests.filter(
                  (r) => r.path === "/api/search" && r.query.q === q
                ).length === 1)
            )
              return json({ error: "synthetic unavailable" }, 503)
            res.writeHead(200, {
              "Content-Type": "text/event-stream",
              "Cache-Control": "no-cache",
            })
            if (q === "page-mutations") return complete(res, mutationEntries)
            if (q === "empty") return complete(res, [])
            if (q === "delayed-page" || q === "delayed-clamp") {
              res.write(frame("start", { storages: 1 }))
              const entries = q === "delayed-clamp"
                ? Array.from({ length: 60 }, (_, i) => makeEntry("stream" + (i + 1)))
                : [makeEntry("partial-folder")]
              res.write(frame("results", { entries }))
              streams.set(q, res)
              res.on("close", () => streams.delete(q))
              return
            }
            if (q === "partial-error") {
              res.write(frame("results", { entries: [makeEntry("partial-folder")] }))
              res.end(frame("error", { error: "synthetic fatal search error" }))
              return
            }
            if (q === "early-eof") {
              res.end(frame("start", { storages: 2 }))
              return
            }
            if (q === "partial-eof") {
              res.write(frame("start", { storages: 2 }))
              res.end(
                frame("results", { entries: [makeEntry("partial-folder")] })
              )
              return
            }
            if (q === "server-error") {
              res.end(frame("error", { error: "synthetic fatal search error" }))
              return
            }
            if (q === "old-query") {
              res.write(frame("start", { storages: 1 }))
              streams.set(q, res)
              res.on("close", () => streams.delete(q))
              return
            }
            if (q === "truncated")
              return complete(
                res,
                Array.from({ length: 500 }, (_, i) => makeEntry("capped" + i)),
                false,
                true
              )
            if (q === "exact-cap")
              return complete(
                res,
                Array.from({ length: 500 }, (_, i) => makeEntry("exact" + i))
              )
            if (q === "special-parent")
              return complete(res, [
                { ...makeEntry("child"), path: "/synthetic/한글 #?%23/child" },
              ])
            if (q === "special")
              return complete(res, [
                makeEntry("hash#tail"),
                makeEntry("query?tail"),
                makeEntry("percent%23tail"),
                makeEntry("한글 #?%"),
                makeEntry("raw#?%23.bin", "file"),
              ])
            if (q === "rename")
              return complete(res, [
                makeEntry(renamed ? "rename-new" : "rename-old"),
              ])
            if (q === "many")
              return complete(
                res,
                Array.from({ length: 220 }, (_, i) =>
                  makeEntry("item" + (220 - i))
                )
              )
            if (q === "sorted")
              return complete(res, [
                makeEntry("zfile.txt", "file"),
                makeEntry("folder10"),
                makeEntry("folder2"),
                makeEntry("afile.txt", "file"),
              ])
            if (q === "storage-failure") return complete(res, [], true)
            return complete(res, [makeEntry(q + "-folder")])
          })
        },
      },
    ],
    resolve: { alias: { "@": path.join(appRoot, "src") } },
    server: {
      host: "127.0.0.1",
      port: 0,
      strictPort: false,
      fs: { allow: [appRoot, path.join(repo, "frontend/node_modules")] },
    },
    logLevel: "warn",
  })

  await server.listen()
  const port = server.httpServer.address().port
  const origin = `http://127.0.0.1:${port}`
  fs.writeFileSync(path.join(root, "runtime.json"), JSON.stringify({ pid: process.pid, port, appRoot }))
  return {
    origin,
    requests,
    streams,
    setConfig: (value) => {
      config = { ...config, ...value }
    },
    resetRename: () => {
      renamed = false
    },
    resetMutations: () => {
      mutationEntries = Array.from({ length: 61 }, (_, i) => makeEntry("mut" + String(i + 1).padStart(3, "0")))
    },
    finishStream: (q, count) => {
      const res = streams.get(q)
      const received = q === "delayed-clamp" ? 60 : 1
      const entries = Array.from({ length: count - received }, (_, i) => makeEntry("stream" + (i + 1)))
      if (entries.length) res.write(frame("results", { entries }))
      res.write(frame("storage", { storage_id: 1, mount: "/synthetic", status: "done", count }))
      res.end(frame("done", { total: count, storages: 1 }))
    },
    close: async () => {
      for (const res of streams.values()) res.destroy()
      streams.clear()
      await server.close()
      const portReleased = await new Promise((resolve) => {
        const probe = net.createServer()
        probe.once("error", () => resolve(false))
        probe.listen(port, "127.0.0.1", () => probe.close(() => resolve(true)))
      })
      fs.writeFileSync(path.join(root, "cleanup.json"), JSON.stringify({ serverListening: server.httpServer.listening, openStreams: streams.size, portReleased }))
      if (!portReleased) throw new Error("Synthetic search port was not released")
    },
  }
}
