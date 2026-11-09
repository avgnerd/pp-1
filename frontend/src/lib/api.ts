// Typed client for the Go API. Every state-changing request carries the CSRF header
// the server requires; errors surface the server's Korean message.

export type EntryType = "file" | "folder" | "mount" | "virtual"

export interface Entry {
  name: string
  path: string
  type: EntryType
  size: number | null
  modified?: string
  rev?: string
  id?: string
  storage_id?: number
  status?: string
}

export interface ListResult {
  path: string
  virtual: boolean
  storage_id?: number
  entries: Entry[]
  total: number
  offset: number
}

export type PaginationType = "all" | "pages" | "more" | "auto_more"

export interface ClientConfig {
  pagination_type: PaginationType
  per_page: number
  audio_autoplay: boolean
  video_autoplay: boolean
  archive_preview: boolean
  data_mode: "direct" | "proxy"
  public_url: string
  transcode: boolean
  probe: boolean
}

export interface Mount {
  id: number
  name: string
  mount_path: string
  status: "pending" | "active" | "error"
  account_email: string
  account_name: string
}

export interface Storage extends Mount {
  root_path: string
  app_key: string
  account_id: string
  cache_ttl_min: number
  auth_mode: "localhost" | "public" | "code"
  redirect_uri: string
  last_error: string
  created_at: number
  updated_at: number
  authorized_at: number
}

export interface AuthStart {
  storage: Storage
  authorize_url: string
  redirect_uri: string
}

export interface Settings {
  robots_txt: string
  pagination_type: PaginationType
  per_page: number
  audio_autoplay: boolean
  video_autoplay: boolean
  archive_preview: boolean
  cache_cleanup_sec: number
  data_mode: "direct" | "proxy"
  transcode_concurrency: number
  thumb_cache_mb: number
  folder_size_concurrency: number
  search_concurrency: number
  archive_max_mb: number
}

export interface Me {
  authenticated: boolean
  username?: string
  is_default_password?: boolean
}

export interface MediaStream {
  index: number
  codec_type: string
  codec_name: string
  profile?: string
  width?: number
  height?: number
  pix_fmt?: string
  channels?: number
  color_transfer?: string
}

export interface VRInfo {
  projection: "" | "180" | "360"
  stereo: "mono" | "sbs" | "tb"
  source: "" | "metadata" | "filename"
}

export interface Probe {
  format: string
  duration: number
  bitrate: number
  streams?: MediaStream[]
  video?: MediaStream
  audio?: MediaStream
  vr: VRInfo
}

export interface ArchiveEntry {
  name: string
  size: number
  packed?: number
  modified?: string
  dir: boolean
}

export interface ArchiveListing {
  format: string
  entries: ArchiveEntry[]
  truncated: boolean
  encrypted?: boolean
}

export interface OpFailure {
  path: string
  error: string
}

export interface OpResult {
  ok: boolean
  done: number
  failures: OpFailure[] | null
}

export interface RestorePreviewStorage {
  name: string
  mount_path: string
  status: string
}

export interface RestorePreview {
  preview_id: string
  format_version: number
  encrypted: boolean
  encryption_mode: "password" | "none" | "instance-key"
  created_at: string
  app_version: string
  admin_username: string
  settings_count: number
  storages: RestorePreviewStorage[]
}

export interface BackupSnapshot {
  id: string
  created_at: string
  size: number
  app_version: string
  storage_count: number
}

export class ApiError extends Error {
  status: number
  code?: string
  field?: string
  constructor(status: number, message: string, code?: string, field?: string) {
    super(message)
    this.status = status
    this.code = code
    this.field = field
  }
}

async function parseErrorResponse(res: Response, dispatchUnauthorized = true): Promise<ApiError> {
  let msg = res.statusText || `요청이 실패했습니다 (HTTP ${res.status})`
  let code: string | undefined
  let field: string | undefined
  try {
    const j = await res.json()
    if (j?.error) msg = j.error
    code = j?.code
    field = j?.field
  } catch {
    /* not JSON */
  }
  if (dispatchUnauthorized && res.status === 401 && (!code || code === "auth_required")) {
    window.dispatchEvent(new CustomEvent("dvfs:unauthorized"))
  }
  return new ApiError(res.status, msg, code, field)
}

function parseContentDispositionFilename(header: string | null, fallback = "dvfs-backup.json"): string {
  if (!header) return fallback
  const utf8Match = /filename\*=UTF-8''([^;\r\n]+)/i.exec(header)
  if (utf8Match?.[1]) {
    try {
      return decodeURIComponent(utf8Match[1].trim().replace(/^"(.*)"$/, "$1"))
    } catch {
      /* ignore malformed URI component */
    }
  }
  const quotedMatch = /filename="([^"\r\n]+)"/i.exec(header)
  if (quotedMatch?.[1]) return quotedMatch[1].trim()
  const plainMatch = /filename=([^;\r\n]+)/i.exec(header)
  if (plainMatch?.[1]) return plainMatch[1].trim().replace(/^"(.*)"$/, "$1")
  return fallback
}

function normalizeRestorePreview(raw: RestorePreview): RestorePreview {
  const encrypted = Boolean(raw.encrypted)
  const mode =
    raw.encryption_mode === "password" || raw.encryption_mode === "instance-key" || raw.encryption_mode === "none"
      ? raw.encryption_mode
      : encrypted
        ? "password"
        : "none"
  return {
    ...raw,
    encrypted,
    encryption_mode: mode,
    storages: Array.isArray(raw.storages) ? raw.storages : [],
  }
}

async function request<T>(method: string, url: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = { "X-Requested-With": "dvfs" }
  if (body !== undefined) headers["Content-Type"] = "application/json"
  const res = await fetch(url, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
    signal,
  })
  if (!res.ok) {
    let msg = `요청이 실패했습니다 (HTTP ${res.status})`
    let code: string | undefined
    let field: string | undefined
    try {
      const j = await res.json()
      if (j?.error) msg = j.error
      code = j?.code
      field = j?.field
    } catch {
      /* not JSON */
    }
    if (res.status === 401 && code === "auth_required") {
      window.dispatchEvent(new CustomEvent("dvfs:unauthorized"))
    }
    throw new ApiError(res.status, msg, code, field)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

const q = (params: Record<string, string | number | undefined>) => {
  const s = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") s.set(k, String(v))
  return s.toString()
}

// playerClient identifies this browser tab to the server. Survives reloads (sessionStorage) so a
// reload reuses its conversion session; other tabs and devices get their own sessions.
let clientId = ""
function playerClient(): string {
  if (clientId) return clientId
  try {
    clientId = sessionStorage.getItem("dvfs-player") ?? ""
    if (!clientId) {
      // randomUUID needs HTTPS; getRandomValues also works over plain HTTP.
      clientId = Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, "0")).join("")
      sessionStorage.setItem("dvfs-player", clientId)
    }
  } catch {
    clientId ||= Math.random().toString(36).slice(2)
  }
  return clientId
}

export const api = {
  me: () => request<Me>("GET", "/api/auth/me"),
  login: (username: string, password: string) => request<Me>("POST", "/api/auth/login", { username, password }),
  logout: () => request<{ ok: boolean }>("POST", "/api/auth/logout"),
  config: () => request<ClientConfig>("GET", "/api/config"),
  mounts: () => request<Mount[]>("GET", "/api/mounts"),
  list: (path: string, opts: { sort?: string; order?: string; offset?: number; limit?: number }, signal?: AbortSignal) =>
    request<ListResult>("GET", `/api/fs/list?${q({ path, ...opts })}`, undefined, signal),
  stat: (path: string) => request<Entry>("GET", `/api/fs/stat?${q({ path })}`),
  siblings: (path: string, sort?: string, order?: string) =>
    request<Entry[]>("GET", `/api/fs/siblings?${q({ path, sort, order })}`),
  link: (path: string) => request<{ url: string; kind: string }>("GET", `/api/fs/link?${q({ path })}`),
  text: (path: string) =>
    request<{ text: string; encoding: string; truncated: boolean; size: number }>("GET", `/api/fs/text?${q({ path })}`),
  archive: (path: string) =>
    request<{ listing: ArchiveListing; sequential: boolean; budget_mb: number }>("GET", `/api/fs/archive?${q({ path })}`),
  rename: (path: string, name: string) => request<{ ok: boolean; path: string }>("POST", "/api/fs/rename", { path, name }),
  move: (paths: string[], dest: string) => request<OpResult>("POST", "/api/fs/move", { paths, dest }),
  copy: (paths: string[], dest: string) => request<OpResult>("POST", "/api/fs/copy", { paths, dest }),
  remove: (paths: string[]) => request<OpResult>("POST", "/api/fs/delete", { paths }),
  zip: (paths: string[]) => request<{ url: string; name: string }>("POST", "/api/fs/zip", { paths }),
  probe: (path: string) =>
    request<{ probe: Probe; probe_error?: string; transcode: boolean }>("GET", `/api/media/probe?${q({ path })}`),
  hls: (path: string, mode: "remux" | "transcode" | "audio", height = 1080) =>
    request<{ id: string; playlist: string; mode: string; duration: number }>("POST", "/api/media/hls", { path, mode, height, client: playerClient() }),
  hlsClose: (id: string) => request<{ ok: boolean }>("DELETE", `/api/media/hls/${encodeURIComponent(id)}`),
  hlsPing: (id: string) => request<{ ok: boolean }>("POST", `/api/media/hls/${encodeURIComponent(id)}/ping`),

  // admin
  profile: () => request<{ username: string; is_default_password: boolean }>("GET", "/api/admin/profile"),
  updateProfile: (v: { username: string; current_password: string; new_password: string }) =>
    request<{ username: string; is_default_password: boolean }>("PUT", "/api/admin/profile", v),
  settings: () => request<Settings>("GET", "/api/admin/settings"),
  updateSettings: (s: Settings) => request<Settings>("PUT", "/api/admin/settings", s),
  system: () => request<SystemInfo>("GET", "/api/admin/system"),
  clearCache: (thumbs: boolean) => request<{ ok: boolean }>("POST", "/api/admin/cache/clear", { thumbs }),
  oauthInfo: () =>
    request<{ localhost_redirect: string; public_redirect: string; public_url: string; scopes: string[] }>("GET", "/api/admin/oauth/info"),
  storages: () => request<Storage[]>("GET", "/api/admin/storages"),
  storage: (id: number) => request<Storage>("GET", `/api/admin/storages/${id}`),
  createStorage: (v: StorageInput) => request<AuthStart>("POST", "/api/admin/storages", v),
  updateStorage: (id: number, v: StorageInput) => request<Storage>("PUT", `/api/admin/storages/${id}`, v),
  deleteStorage: (id: number) => request<{ ok: boolean }>("DELETE", `/api/admin/storages/${id}`),
  checkMount: (mount_path: string, id = 0) => request<{ ok: boolean }>("POST", "/api/admin/storages/check-mount", { mount_path, id }),
  reauth: (id: number, auth_mode: string) => request<AuthStart>("POST", `/api/admin/storages/${id}/reauth`, { auth_mode }),
  submitCode: (id: number, code: string) => request<Storage>("POST", `/api/admin/storages/${id}/code`, { code }),
  pasteUrl: (url: string) => request<Storage>("POST", "/api/admin/oauth/paste", { url }),

  admin: {
    exportBackup: async (body: {
      encrypted: boolean
      password?: string
      confirm_unencrypted?: boolean
    }): Promise<{ blob: Blob; filename: string }> => {
      const res = await fetch("/api/admin/backup", {
        method: "POST",
        headers: {
          "X-Requested-With": "dvfs",
          "Content-Type": "application/json",
        },
        credentials: "include",
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        throw await parseErrorResponse(res)
      }
      const filename = parseContentDispositionFilename(res.headers.get("Content-Disposition"), "dvfs-backup.json")
      const blob = await res.blob()
      return { blob, filename }
    },

    previewRestore: async (file: File, password?: string): Promise<RestorePreview> => {
      const form = new FormData()
      form.append("file", file)
      if (password !== undefined && password !== "") {
        form.append("password", password)
      }
      const res = await fetch("/api/admin/restore/preview", {
        method: "POST",
        headers: {
          "X-Requested-With": "dvfs",
        },
        credentials: "include",
        body: form,
      })
      if (!res.ok) {
        throw await parseErrorResponse(res)
      }
      const data = (await res.json()) as RestorePreview
      return normalizeRestorePreview(data)
    },

    applyRestore: async (previewId: string): Promise<{ ok: boolean; failed_storages: string[] }> => {
      const res = await fetch("/api/admin/restore/apply", {
        method: "POST",
        headers: {
          "X-Requested-With": "dvfs",
          "Content-Type": "application/json",
        },
        credentials: "include",
        body: JSON.stringify({ preview_id: previewId }),
      })
      if (!res.ok) {
        throw await parseErrorResponse(res, false)
      }
      let data: { ok?: boolean; failed_storages?: string[] | null } = {}
      if (res.status !== 204) {
        try {
          data = await res.json()
        } catch {
          data = { ok: true, failed_storages: [] }
        }
      }
      return {
        ok: Boolean(data?.ok ?? true),
        failed_storages: Array.isArray(data?.failed_storages) ? data.failed_storages : [],
      }
    },

    listSnapshots: async (): Promise<{ snapshots: BackupSnapshot[] }> => {
      const res = await fetch("/api/admin/backups/snapshots", {
        method: "GET",
        headers: {
          "X-Requested-With": "dvfs",
        },
        credentials: "include",
      })
      if (!res.ok) {
        throw await parseErrorResponse(res)
      }
      const data = await res.json()
      const snapshots: BackupSnapshot[] = Array.isArray(data)
        ? data
        : Array.isArray(data?.snapshots)
          ? data.snapshots
          : []
      return { snapshots }
    },

    previewSnapshot: async (id: string): Promise<RestorePreview> => {
      const res = await fetch(`/api/admin/backups/snapshots/${encodeURIComponent(id)}/preview`, {
        method: "POST",
        headers: {
          "X-Requested-With": "dvfs",
        },
        credentials: "include",
      })
      if (!res.ok) {
        throw await parseErrorResponse(res)
      }
      const data = (await res.json()) as RestorePreview
      return normalizeRestorePreview(data)
    },
  },
}

export interface StorageInput {
  name: string
  mount_path: string
  root_path: string
  app_key?: string
  app_secret?: string
  cache_ttl_min: number
  auth_mode?: string
}

export interface SystemInfo {
  cache: { entries: number; bytes: number; hits: number; misses: number; evictions: number; expirations: number; max_entries: number; max_bytes: number }
  thumbs: { files: number; bytes: number; max_bytes: number }
  folder_size: { active: number; waiting: number; limit: number }
  transcode: { active: number; waiting: number; limit: number; sessions: number }
  ffmpeg: string
  ffprobe: string
  storages: number
  data_dir: string
}

// URL builders for resources loaded by the browser directly (img, video, iframe, a[download]).
export const urls = {
  raw: (path: string, opts: { download?: boolean; proxy?: boolean; fresh?: string } = {}) =>
    `/api/fs/raw?${q({ path, download: opts.download ? 1 : undefined, proxy: opts.proxy ? 1 : undefined, fresh: opts.fresh || undefined })}`,
  thumb: (path: string, rev: string | undefined, size = "w256h256") => `/api/fs/thumb?${q({ path, rev, size })}`,
  officePreview: (path: string) => `/api/fs/preview?${q({ path })}`,
  folderSize: (path: string) => `/api/fs/folder-size?${q({ path })}`,
  search: (query: string, kind: string, scope?: string) => `/api/search?${q({ q: query, kind, scope })}`,
}
