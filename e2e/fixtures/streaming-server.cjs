const http = require('node:http')
const fs = require('node:fs')
const path = require('node:path')
const os = require('node:os')
const { spawnSync } = require('node:child_process')

const root = path.resolve(process.env.DVFS_STREAMING_DIST || path.join(__dirname, '../../dist'))
const media = process.env.DVFS_STREAMING_TEMP || fs.mkdtempSync(path.join(os.tmpdir(), 'dvfs-streaming-'))
fs.mkdirSync(media, { recursive: true })
const duration = 240
function ffmpeg(args) {
  const result = spawnSync(process.env.FFMPEG_PATH || 'ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', ...args], { encoding: 'utf8', timeout: 90_000 })
  if (result.error || result.status !== 0) throw result.error || new Error(result.stderr)
}
ffmpeg(['-f', 'lavfi', '-i', `color=c=red:s=160x90:r=10:d=${duration}`, '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', '-g', '40', '-movflags', '+faststart', path.join(media, 'demo.mp4')])
ffmpeg(['-f', 'lavfi', '-i', `color=c=blue:s=160x90:r=10:d=${duration}`, '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', '-g', '40', '-sc_threshold', '0', '-hls_time', '4', '-hls_list_size', '0', '-hls_playlist_type', 'vod', '-hls_segment_filename', path.join(media, 'seg%d.ts'), path.join(media, 'index.m3u8')])
const entries = ['demo.mp4', 'next.mp4'].map(name => ({ name, path: '/' + name, type: 'file', size: fs.statSync(path.join(media, 'demo.mp4')).size, rev: 'streaming-fixture', modified: '2026-11-03T00:00:00Z' }))
let session = 0
const closed = new Set()
function file(req, res, filename) {
  const size = fs.statSync(filename).size
  const mime = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.mp4': 'video/mp4', '.m3u8': 'application/vnd.apple.mpegurl', '.ts': 'video/mp2t' }[path.extname(filename)] || 'application/octet-stream'
  const range = /bytes=(\d+)-(\d*)/.exec(req.headers.range || '')
  res.setHeader('Content-Type', mime)
  res.setHeader('Accept-Ranges', 'bytes')
  res.setHeader('Cache-Control', 'no-store')
  let stream
  if (range) {
    const start = Number(range[1]), end = range[2] ? Math.min(Number(range[2]), size - 1) : size - 1
    if (start >= size || end < start) { res.writeHead(416); res.end(); return }
    res.writeHead(206, { 'Content-Range': `bytes ${start}-${end}/${size}`, 'Content-Length': end - start + 1 })
    stream = fs.createReadStream(filename, { start, end })
  } else {
    res.setHeader('Content-Length', size)
    stream = fs.createReadStream(filename)
  }
  res.on('close', () => stream.destroy())
  stream.pipe(res)
}
const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://localhost')
  const send = (value, status = 200) => { res.writeHead(status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(value)) }
  try {
    if (u.pathname === '/__ready') return send({ ready: true })
    if (u.pathname === '/api/auth/me') return send({ authenticated: true, username: 'fixture', is_default_password: false })
    if (u.pathname === '/api/config') return send({ pagination_type: 'all', per_page: 50, audio_autoplay: true, video_autoplay: true, archive_preview: false, data_mode: 'proxy', public_url: '', transcode: true, probe: true })
    if (u.pathname === '/api/mounts') return send([])
    if (u.pathname === '/api/fs/stat') return send(entries.find(e => e.path === u.searchParams.get('path')) || entries[0])
    if (u.pathname === '/api/fs/siblings') return send(entries)
    if (u.pathname === '/api/media/probe') return send({ probe: { format: 'mp4', duration, bitrate: 100000, video: { index: 0, codec_type: 'video', codec_name: 'h264', width: 160, height: 90, pix_fmt: 'yuv420p' }, vr: { projection: '', stereo: 'mono', source: '' } }, transcode: true })
    if (u.pathname === '/api/media/hls') {
      let body = ''
      for await (const chunk of req) body += chunk
      const id = 'fixture-' + (++session)
      return send({ id, playlist: `/media/${id}/index.m3u8`, mode: JSON.parse(body || '{}').mode, duration })
    }
    if (u.pathname.startsWith('/api/media/hls/')) {
      const id = u.pathname.split('/')[4]
      if (req.method === 'DELETE') { closed.add(id); return send({ ok: true }) }
      return send(closed.has(id) ? { error: 'gone' } : { ok: true }, closed.has(id) ? 404 : 200)
    }
    if (u.pathname === '/api/fs/raw') return file(req, res, path.join(media, 'demo.mp4'))
    if (u.pathname.startsWith('/api/')) return send({ error: 'unimplemented fixture endpoint' }, 404)
    if (u.pathname.startsWith('/media/')) return file(req, res, path.join(media, path.basename(u.pathname)))
    const filename = path.resolve(root, '.' + decodeURIComponent(u.pathname))
    if (!filename.startsWith(root + path.sep) && filename !== root) return send({ error: 'invalid path' }, 400)
    return file(req, res, fs.existsSync(filename) && fs.statSync(filename).isFile() ? filename : path.join(root, 'index.html'))
  } catch (error) { if (!res.headersSent) res.writeHead(500); res.end(String(error.message)) }
})
server.listen(Number(process.env.DVFS_STREAMING_PORT || 18764), '127.0.0.1')
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => { server.closeAllConnections(); server.close(() => process.exit(0)) })
