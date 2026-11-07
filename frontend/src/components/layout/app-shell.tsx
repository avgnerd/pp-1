import { Link, Outlet, useLocation, useNavigate } from "@tanstack/react-router"
import { LayoutGridIcon, ListIcon, SearchIcon } from "lucide-react"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { MountBar } from "@/components/layout/mount-bar"
import { useMe } from "@/lib/queries"
import { usePrefs, type ViewMode } from "@/stores/ui"

export function AppShell() {
  const me = useMe()
  const view = usePrefs((s) => s.view)
  const setView = usePrefs((s) => s.setView)
  const navigate = useNavigate()
  const loc = useLocation()

  return (
    <div className="flex min-h-svh flex-col">
      <header className="sticky top-0 z-30 border-b bg-background/95 backdrop-blur supports-backdrop-filter:bg-background/80">
        <div className="mx-auto flex h-12 w-full max-w-6xl items-center gap-3 px-4">
          <Link to="/" className="flex items-center gap-2 font-semibold tracking-tight focus-visible:outline-2 focus-visible:outline-ring rounded-md">
            <img src="/favicon.svg" alt="" className="size-6" />
            <span>Dropbox VFS</span>
          </Link>
          <div className="ml-auto flex items-center gap-1">
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label="검색"
                    data-active={loc.pathname === "/search" || undefined}
                    className="data-active:text-primary"
                    onClick={() => navigate({ to: "/search", search: { q: "", kind: "all" } })}
                  />
                }
              >
                <SearchIcon />
              </TooltipTrigger>
              <TooltipContent>검색</TooltipContent>
            </Tooltip>
            <ToggleGroup
              value={[view]}
              onValueChange={(v) => v[0] && setView(v[0] as ViewMode)}
              aria-label="보기 방식"
              spacing={0}
              variant="outline"
              size="sm"
            >
              <ToggleGroupItem value="list" aria-label="목록 보기">
                <ListIcon />
              </ToggleGroupItem>
              <ToggleGroupItem value="grid" aria-label="이미지 보기">
                <LayoutGridIcon />
              </ToggleGroupItem>
            </ToggleGroup>
          </div>
        </div>
      </header>

      <MountBar />

      {me.data?.is_default_password ? (
        <div className="mx-auto w-full max-w-6xl px-4 pt-3">
          <Alert>
            <AlertDescription>
              초기 비밀번호(1234)를 쓰고 있습니다.{" "}
              <Link to="/admin/profile" className="font-medium text-primary underline underline-offset-4">
                관리 &gt; 프로필
              </Link>
              에서 바꾸세요.
            </AlertDescription>
          </Alert>
        </div>
      ) : null}

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col px-4 py-4">
        <Outlet />
      </main>

      <footer className="border-t">
        <div className="mx-auto flex h-11 w-full max-w-6xl items-center justify-between px-4 text-sm text-muted-foreground">
          <span>{me.data?.username}</span>
          <Link to="/admin" className="rounded-sm hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring">
            관리
          </Link>
        </div>
      </footer>
    </div>
  )
}
