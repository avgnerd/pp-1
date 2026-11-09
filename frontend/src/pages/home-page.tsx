import { Link, useNavigate, useSearch } from "@tanstack/react-router"
import { HardDriveIcon, PlugZapIcon } from "lucide-react"
import { useMemo, useState } from "react"

import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Pager } from "@/components/browser/pager"
import { toSplat } from "@/lib/paths"
import { useConfig, useMounts } from "@/lib/queries"
import { cn } from "@/lib/utils"

/** Main page: every connected storage, paged with the admin's pagination type. */
export function HomePage() {
  const mounts = useMounts()
  const cfg = useConfig()
  const { page = 1 } = useSearch({ from: "/authed/shell/" })
  const navigate = useNavigate()
  const type = cfg.data?.pagination_type ?? "pages"
  const per = cfg.data?.per_page ?? 30
  // Count "more" steps rather than rows so the first screen always follows the loaded per_page.
  const [moreSteps, setMoreSteps] = useState(0)

  const all = useMemo(() => mounts.data ?? [], [mounts.data])
  const visible = useMemo(() => {
    if (type === "all") return all
    if (type === "pages") return all.slice((page - 1) * per, page * per)
    return all.slice(0, per * (moreSteps + 1))
  }, [all, type, page, per, moreSteps])

  if (mounts.isPending || cfg.isPending) {
    return (
      <div className="flex flex-col gap-2">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-12 w-full" />
        ))}
      </div>
    )
  }

  if (all.length === 0) {
    return (
      <Empty className="py-20">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <PlugZapIcon />
          </EmptyMedia>
          <EmptyTitle>연결된 저장소가 없습니다</EmptyTitle>
          <EmptyDescription>Dropbox 계정을 연결하고 마운트 경로를 정하면 여기에서 바로 탐색할 수 있습니다.</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button render={<Link to="/admin/storages" />} nativeButton={false}>
            저장소 추가하기
          </Button>
        </EmptyContent>
      </Empty>
    )
  }

  return (
    <div className="flex flex-col">
      <div className="flex items-baseline justify-between pb-2">
        <h1 className="text-base font-semibold">저장소</h1>
        <span className="text-sm text-muted-foreground tabular-nums">{all.length.toLocaleString()}개</span>
      </div>
      <ul className="flex flex-col divide-y divide-border/60 border-y">
        {visible.map((m) => (
          <li key={m.id}>
            <Link
              to="/$"
              params={{ _splat: toSplat(m.mount_path) }}
              className={cn(
                "grid grid-cols-[1.25rem_1fr] items-center gap-x-3 gap-y-0.5 px-2 py-2.5 hover:bg-accent/50 focus-visible:outline-2 focus-visible:outline-ring sm:grid-cols-[1.25rem_minmax(0,1fr)_minmax(0,16rem)_6rem]",
                m.status !== "active" && "opacity-70",
              )}
            >
              <HardDriveIcon className="size-4 text-folder" aria-hidden />
              <span className="truncate font-medium">{m.mount_path}</span>
              <span className="col-start-2 truncate text-sm text-muted-foreground sm:col-start-auto">
                {m.name ? `${m.name} (${m.account_email || "계정 정보 없음"})` : m.account_email || "계정 정보 없음"}
              </span>
              <span
                className={cn(
                  "col-start-2 text-sm sm:col-start-auto sm:text-right",
                  m.status === "active" ? "text-muted-foreground" : m.status === "error" ? "text-destructive" : "text-folder",
                )}
              >
                {m.status === "active" ? "연결됨" : m.status === "error" ? "재인증 필요" : "인증 대기"}
              </span>
            </Link>
          </li>
        ))}
      </ul>
      <Pager
        type={type}
        page={page}
        pageCount={Math.max(1, Math.ceil(all.length / per))}
        onPage={(p) => navigate({ to: "/", search: { page: p > 1 ? p : undefined } })}
        hasMore={visible.length < all.length}
        loadMore={() => setMoreSteps((n) => n + 1)}
        loading={false}
        shown={visible.length}
        total={all.length}
      />
    </div>
  )
}
