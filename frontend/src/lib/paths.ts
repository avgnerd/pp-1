// Virtual path helpers. Virtual paths look like "/path/to/폴더/영상.mp4".

export function joinPath(dir: string, name: string): string {
  return dir === "/" ? `/${name}` : `${dir}/${name}`
}

export function parentPath(p: string): string {
  if (p === "/" || p === "") return "/"
  const i = p.lastIndexOf("/")
  return i <= 0 ? "/" : p.slice(0, i)
}

export function baseName(p: string): string {
  const i = p.lastIndexOf("/")
  return i < 0 ? p : p.slice(i + 1)
}

/** "/a/b c" -> "a/b c" for the router splat param. */
export function toSplat(p: string): string {
  return p.replace(/^\/+/, "")
}

/** Router splat -> virtual path. */
export function fromSplat(s: string | undefined): string {
  if (!s) return "/"
  return "/" + s.replace(/^\/+/, "").replace(/\/+$/, "")
}

/** Percent-encodes each segment so the URL survives copy/paste and spaces or # in names. */
export function encodePath(p: string): string {
  return p
    .split("/")
    .map((s) => encodeURIComponent(s))
    .join("/")
}

/** Absolute shareable link: public_url (or this origin) + encoded virtual path. */
export function shareLink(p: string, publicUrl: string): string {
  const base = (publicUrl || window.location.origin).replace(/\/+$/, "")
  return base + encodePath(p)
}

export function crumbs(p: string): { name: string; path: string }[] {
  const out = [{ name: "/", path: "/" }]
  if (p === "/") return out
  let cur = ""
  for (const seg of p.split("/").filter(Boolean)) {
    cur += "/" + seg
    out.push({ name: seg, path: cur })
  }
  return out
}

/** Copies text, falling back to a hidden textarea where the Clipboard API is unavailable (plain HTTP). */
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      /* fall through */
    }
  }
  const ta = document.createElement("textarea")
  ta.value = text
  ta.setAttribute("readonly", "")
  ta.style.position = "fixed"
  ta.style.top = "-1000px"
  ta.style.opacity = "0"
  document.body.appendChild(ta)
  ta.select()
  ta.setSelectionRange(0, text.length)
  let ok: boolean
  try {
    ok = document.execCommand("copy")
  } catch {
    ok = false
  }
  document.body.removeChild(ta)
  return ok
}

/** Starts a browser download of a same-origin URL without leaving the page. */
export function startDownload(url: string, filename?: string) {
  const a = document.createElement("a")
  a.href = url
  if (filename) a.download = filename
  a.rel = "noopener"
  document.body.appendChild(a)
  a.click()
  a.remove()
}
