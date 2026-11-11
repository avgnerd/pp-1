import { Link, Outlet, useNavigate } from "@tanstack/react-router"
import { ArrowLeftIcon, DatabaseBackup, DatabaseIcon, LogOutIcon, SlidersHorizontalIcon, UserIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { api } from "@/lib/api"
import { keys, queryClient, useMe } from "@/lib/queries"
import { cn } from "@/lib/utils"

// Order fixed by the brief: profile, settings, storages, backup.
const navItems = [
  { to: "/admin/profile", label: "프로필", icon: UserIcon },
  { to: "/admin/settings", label: "설정", icon: SlidersHorizontalIcon },
  { to: "/admin/storages", label: "저장소", icon: DatabaseIcon },
  { to: "/admin/backup", label: "백업 및 복원", icon: DatabaseBackup },
] as const

export default function AdminLayout() {
  const me = useMe()
  const navigate = useNavigate()
  const logout = async () => {
    await api.logout().catch(() => {})
    queryClient.setQueryData(keys.me, { authenticated: false })
    navigate({ to: "/login", search: { next: undefined } })
  }
  return (
    <div className="flex min-h-svh flex-col md:flex-row">
      <aside className="flex shrink-0 flex-col gap-1 border-b bg-sidebar p-3 md:sticky md:top-0 md:h-svh md:w-56 md:border-r md:border-b-0">
        <Link to="/" className="mb-3 flex items-center gap-2 rounded-md px-2 py-1.5 text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeftIcon className="size-4" aria-hidden />
          사이트로 돌아가기
        </Link>
        <p className="px-2 pb-1 text-sm font-semibold">관리</p>
        <nav aria-label="관리 메뉴" className="flex gap-1 md:flex-col">
          {navItems.map(({ to, label, icon: Icon }) => (
            <Link
              key={to}
              to={to}
              className={cn("flex items-center gap-2 rounded-md px-2 py-1.5 text-sm hover:bg-sidebar-accent focus-visible:outline-2 focus-visible:outline-ring")}
              activeProps={{ className: "bg-sidebar-accent text-sidebar-primary font-medium" }}
            >
              <Icon className="size-4" aria-hidden />
              {label}
            </Link>
          ))}
        </nav>
        <div className="mt-auto hidden flex-col gap-1 pt-4 md:flex">
          <p className="truncate px-2 text-xs text-muted-foreground">{me.data?.username}</p>
          <Button variant="ghost" size="sm" className="justify-start" onClick={logout}>
            <LogOutIcon data-icon="inline-start" />
            로그아웃
          </Button>
        </div>
      </aside>
      <main className="min-w-0 flex-1 px-4 py-6 md:px-8">
        <div className="mx-auto max-w-4xl">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
