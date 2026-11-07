import { Link, useLocation } from "@tanstack/react-router"

import { cn } from "@/lib/utils"
import { toSplat } from "@/lib/paths"
import { useMounts } from "@/lib/queries"

/**
 * Every mount path, written out in full (never summarised), between the header and the
 * browser. Each path is a link straight to that mount. The active mount is marked.
 */
export function MountBar() {
  const mounts = useMounts()
  const loc = useLocation()
  const current = decodeURIComponent(loc.pathname)
  const list = mounts.data ?? []
  if (!list.length) return null
  return (
    <nav aria-label="마운트 경로" className="border-b bg-card/40">
      <ul className="mx-auto flex w-full max-w-6xl flex-wrap gap-x-1 gap-y-1 px-4 py-2">
        {list.map((m) => {
          const active = current === m.mount_path || current.startsWith(m.mount_path + "/")
          return (
            <li key={m.id}>
              <Link
                to="/$"
                params={{ _splat: toSplat(m.mount_path) }}
                title={m.account_email || m.name || undefined}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded-md px-2 py-0.5 text-[0.8125rem] text-muted-foreground transition-colors",
                  "hover:bg-accent hover:text-accent-foreground focus-visible:outline-2 focus-visible:outline-ring",
                  active && "bg-folder/15 text-folder hover:bg-folder/20 hover:text-folder",
                  m.status !== "active" && "opacity-60",
                )}
              >
                {m.mount_path}
                {m.status === "error" ? <span className="text-destructive">· 재인증 필요</span> : null}
                {m.status === "pending" ? <span>· 인증 대기</span> : null}
              </Link>
            </li>
          )
        })}
      </ul>
    </nav>
  )
}
