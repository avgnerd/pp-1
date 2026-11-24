const fs = require("node:fs"),
  path = require("node:path"),
  net = require("node:net")
const { pathToFileURL } = require("node:url")
exports.startBrowseServer = async function ({ repo, root, appRoot }) {
  const imp = (p) =>
    import(pathToFileURL(path.join(repo, "frontend/node_modules", p)).href)
  const requests = []
  const entry = (p, type = "folder") => ({
    name: p.split("/").at(-1),
    path: p,
    type,
    size: null,
    storage_id: 1,
  })
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
  const mount = (id, p) => ({
    id,
    name: "Synthetic " + id,
    mount_path: p,
    status: "active",
    account_email: "synthetic@example.invalid",
    account_name: "Synthetic",
  })
  let mounts = [mount(1, "/synthetic")]
  let storages = [
    {
      ...mount(1, "/synthetic"),
      root_path: "/",
      app_key: "synthetic-key",
      account_id: "dbid:synthetic",
      cache_ttl_min: 30,
      auth_mode: "localhost",
      redirect_uri: "http://127.0.0.1/api/oauth/callback",
    },
  ]
  const initialMounts = [...mounts]
  const initialStorages = [...storages]
  const data = new Map([
    [
      "/synthetic",
      [
        entry("/synthetic/normal"),
        entry("/synthetic/hash#tail"),
        entry("/synthetic/query?tail"),
        entry("/synthetic/percent%23tail"),
        entry("/synthetic/한글 #?%"),
        entry("/synthetic/raw#?%23.bin", "file"),
        entry("/synthetic/note#?%23.txt", "file"),
        entry("/synthetic/archive.rar", "file"),
        entry("/synthetic/.env", "file"),
        entry("/synthetic/İ.mp4", "file"),
        entry("/synthetic/gallery"),
        entry("/synthetic/pages"),
        entry("/synthetic/a"),
        entry("/synthetic/b"),
        entry("/synthetic/Sub"),
      ],
    ],
    [
      "/synthetic/gallery",
      [
        entry("/synthetic/gallery/#1 <b>tag.jpg", "file"),
        entry("/synthetic/gallery/A <b>B & C.jpg", "file"),
        entry("/synthetic/gallery/vector.svg", "file"),
      ],
    ],
    ["/synthetic/normal", [entry("/synthetic/normal/child")]],
    [
      "/synthetic/pages",
      Array.from({ length: 31 }, (_, i) =>
        entry("/synthetic/pages/item" + String(i + 1).padStart(3, "0"))
      ),
    ],
    ["/synthetic/a", [entry("/synthetic/a/only-A")]],
    ["/synthetic/b", [entry("/synthetic/b/only-B")]],
    // The same folder reached through two spellings, as the real server builds entry paths from
    // the request path: the listing under /synthetic/sub keeps the lowercase request casing.
    ["/synthetic/Sub", [entry("/synthetic/Sub/folderA")]],
    ["/synthetic/sub", [entry("/synthetic/sub/folderA")]],
  ])
  const initialData = new Map([...data].map(([p, es]) => [p, [...es]]))
  let bMode = "ok",
    held = []
  const sizeStreams = []
  const { createServer } = await imp("vite/dist/node/index.js")
  const { default: react } = await imp("@vitejs/plugin-react/dist/index.js")
  const { default: tailwind } = await imp("@tailwindcss/vite/dist/index.mjs")
  const server = await createServer({
    configFile: false,
    root: appRoot,
    cacheDir: path.join(root, "vite-cache"),
    logLevel: "warn",
    plugins: [
      react(),
      tailwind(),
      {
        name: "synthetic-api",
        configureServer(v) {
          v.middlewares.use((req, res, next) => {
            const u = new URL(req.url, "http://127.0.0.1")
            if (!u.pathname.startsWith("/api/")) return next()
            const record = {
              method: req.method,
              path: u.pathname,
              query: Object.fromEntries(u.searchParams),
            }
            requests.push(record)
            const json = (value, status = 200) => {
              res.writeHead(status, { "Content-Type": "application/json" })
              res.end(JSON.stringify(value))
            }
            const p = u.searchParams.get("path")
            if (u.pathname === "/api/auth/me")
              return json({ authenticated: true, username: "synthetic-user" })
            if (u.pathname === "/api/config") return json(config)
            if (u.pathname === "/api/mounts") return json(mounts)
            if (u.pathname === "/api/oauth-info")
              return json({
                localhost_redirect: "http://127.0.0.1/api/oauth/callback",
                public_redirect: "",
                scopes: [],
              })
            if (u.pathname === "/api/admin/storages") {
              if (req.method === "GET") return json(storages)
            }
            if (
              u.pathname.startsWith("/api/admin/storages/") &&
              req.method === "DELETE"
            ) {
              const id = Number(u.pathname.split("/").at(-1))
              storages = storages.filter((s) => s.id !== id)
              mounts = mounts.filter((m) => m.id !== id)
              return json({ ok: true })
            }
            if (u.pathname === "/api/fs/folder-size") {
              const stream = { path: p, closed: false, res }
              sizeStreams.push(stream)
              res.writeHead(200, {
                "Content-Type": "text/event-stream",
                "Cache-Control": "no-store",
                Connection: "keep-alive",
              })
              res.write(
                `event: progress\ndata: ${JSON.stringify({ bytes: 1024, files: 3, folders: 0, queued: false })}\n\n`
              )
              req.on("close", () => {
                stream.closed = true
              })
              return
            }
            if (u.pathname === "/api/fs/stat") {
              const found = [...data.values()].flat().find((e) => e.path === p)
              return p === "/synthetic" || data.has(p) || found
                ? json(found ?? entry(p))
                : json({ error: "synthetic path not found" }, 404)
            }
            if (u.pathname === "/api/fs/list") {
              if (p === "/synthetic/b" && bMode === "error")
                return json(
                  { error: "synthetic transient folder failure" },
                  503
                )
              const reply = () => {
                let es = [...(data.get(p) || [])]
                es.sort((a, b) =>
                  a.name.localeCompare(b.name, "en", { numeric: true })
                )
                if (u.searchParams.get("order") === "desc") es.reverse()
                const total = es.length,
                  offset = Number(u.searchParams.get("offset") || 0),
                  limit = Number(u.searchParams.get("limit") || 0)
                return json({
                  path: p,
                  virtual: false,
                  entries: limit
                    ? es.slice(offset, offset + limit)
                    : es.slice(offset),
                  total,
                  offset,
                })
              }
              if (p === "/synthetic/b" && bMode === "hold") {
                held.push({ res, reply })
                return
              }
              return reply()
            }
            if (u.pathname === "/api/fs/delete") {
              let body = ""
              req.on("data", (x) => (body += x))
              req.on("end", () => {
                record.body = JSON.parse(body)
                for (const [key, es] of data)
                  data.set(
                    key,
                    es.filter((e) => !record.body.paths.includes(e.path))
                  )
                json({
                  ok: true,
                  done: record.body.paths.length,
                  failures: null,
                })
              })
              return
            }
            return json({ error: "Synthetic endpoint unavailable" }, 404)
          })
        },
      },
    ],
    resolve: { alias: { "@": path.join(appRoot, "src") } },
    server: {
      host: "127.0.0.1",
      port: 22000 + Math.floor(Math.random() * 10000),
      fs: { allow: [appRoot, path.join(repo, "frontend/node_modules")] },
    },
  })
  await server.listen()
  const port = server.httpServer.address().port,
    origin = "http://127.0.0.1:" + port

  return {
    origin,
    requests,
    data,
    held,
    sizeStreams,
    finishSize: (target, payload) => {
      for (const stream of sizeStreams) {
        if (stream.path !== target || stream.closed) continue
        stream.res.write(`event: done\ndata: ${JSON.stringify(payload)}\n\n`)
        stream.res.end()
        stream.closed = true
      }
    },
    reset: () => {
      for (const x of held.splice(0)) x.res.destroy()
      for (const stream of sizeStreams.splice(0)) {
        if (!stream.closed) stream.res.end()
      }
      bMode = "ok"
      mounts = [...initialMounts]
      storages = [...initialStorages]
      data.clear()
      for (const [p, es] of initialData) data.set(p, [...es])
    },
    setConfig: (x) => {
      config = { ...config, ...x }
    },
    setMounts: (x) => {
      mounts = x
    },
    setBMode: (x) => {
      bMode = x
    },
    release: () => {
      bMode = "ok"
      for (const x of held.splice(0)) if (!x.res.destroyed) x.reply()
    },
    close: async () => {
      for (const x of held.splice(0)) x.res.destroy()
      await server.close()
      const released = await new Promise((resolve) => {
        const p = net.createServer()
        p.once("error", () => resolve(false))
        p.listen(port, "127.0.0.1", () => p.close(() => resolve(true)))
      })
      fs.writeFileSync(
        path.join(root, "cleanup.json"),
        JSON.stringify({
          portReleased: released,
          serverListening: server.httpServer.listening,
        })
      )
      if (!released) throw Error("Browse port not released")
    },
  }
}
