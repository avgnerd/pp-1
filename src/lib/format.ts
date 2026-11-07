// Formatting and file-kind helpers.

const units = ["B", "KB", "MB", "GB", "TB", "PB"]

export function formatBytes(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—"
  if (n < 1024) return `${n} B`
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)} ${units[i]}`
}

const dateFmt = new Intl.DateTimeFormat("ko-KR", {
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
})

export function formatDate(iso: string | undefined): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return "—"
  return dateFmt.format(d)
}

export function formatDuration(sec: number): string {
  if (!Number.isFinite(sec) || sec <= 0) return "—"
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = Math.floor(sec % 60)
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}` : `${m}:${String(s).padStart(2, "0")}`
}

export function extOf(name: string): string {
  const i = name.lastIndexOf(".")
  if (i <= 0 || i === name.length - 1) return ""
  const lower = name.toLowerCase()
  if (lower.endsWith(".tar.gz")) return "tar.gz"
  return lower.slice(i + 1)
}

export type FileKind = "image" | "video" | "audio" | "text" | "pdf" | "office" | "archive" | "other"

const kinds: Record<string, FileKind> = {}
const add = (k: FileKind, list: string) => list.split(" ").forEach((e) => (kinds[e] = k))
add("image", "jpg jpeg png gif webp avif bmp svg heic heif tif tiff")
add("video", "mp4 m4v mkv webm mov avi wmv flv ts m2ts mts mpg mpeg 3gp ogv vob rmvb rm asf divx")
add("audio", "mp3 m4a aac flac wav ogg oga opus wma ape alac aiff aif mka dsf")
add("text", "txt md markdown log csv tsv json xml yaml yml toml ini cfg conf srt vtt ass ssa smi sub js ts tsx jsx mjs cjs css scss html htm py go rs java kt c h cpp hpp cs php rb sh bash ps1 bat cmd sql lua pl swift dart vue svelte gradle properties env dockerfile makefile")
add("pdf", "pdf")
add("office", "doc docx docm ppt pptx pptm xls xlsx xlsm ai gsheet gslides")
add("archive", "zip 7z rar tar tgz tar.gz cbz cbr cb7")

export function fileKind(name: string): FileKind {
  const e = extOf(name)
  if (e === "ts" && /\.ts$/i.test(name)) {
    // MPEG-TS video is far more common in a media library than TypeScript.
    return "video"
  }
  const k = kinds[e]
  if (k) return k
  const lower = name.toLowerCase()
  if (lower === "dockerfile" || lower === "makefile" || lower === "readme") return "text"
  return "other"
}

/** Whether the app has an in-browser preview for this file. */
export function hasPreview(name: string, archivePreview: boolean): boolean {
  const k = fileKind(name)
  if (k === "archive") return archivePreview
  return k !== "other"
}

/** Images Dropbox can thumbnail. */
export function thumbnailable(name: string): boolean {
  return ["jpg", "jpeg", "png", "tif", "tiff", "gif", "webp", "ppm", "bmp", "heic", "heif"].includes(extOf(name))
}
