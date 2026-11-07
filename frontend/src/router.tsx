import { createRootRoute, createRoute, createRouter, lazyRouteComponent, Navigate, Outlet, redirect } from "@tanstack/react-router"

import { AppShell } from "@/components/layout/app-shell"
import { RequireAuth } from "@/components/layout/require-auth"
import { LoginPage } from "@/pages/login-page"
import { HomePage } from "@/pages/home-page"
import { BrowsePage } from "@/pages/browse-page"
import { SearchPage } from "@/pages/search-page"
import { DownloadPage } from "@/pages/download-page"
import { NotFound } from "@/pages/not-found"
import { Spinner } from "@/components/ui/spinner"
import { api } from "@/lib/api"
import { keys, queryClient } from "@/lib/queries"

// Admin screens are visited rarely; load them on demand.
const AdminLayout = lazyRouteComponent(() => import("@/pages/admin/admin-layout"))
const ProfilePage = lazyRouteComponent(() => import("@/pages/admin/profile-page"))
const SettingsPage = lazyRouteComponent(() => import("@/pages/admin/settings-page"))
const StoragesPage = lazyRouteComponent(() => import("@/pages/admin/storages-page"))
const BackupPage = lazyRouteComponent(() => import("@/pages/admin/backup-page"))

const rootRoute = createRootRoute({
  component: () => <Outlet />,
  notFoundComponent: NotFound,
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "login",
  validateSearch: (s: Record<string, unknown>): { next?: string; restored?: string } => ({
    next: typeof s.next === "string" ? s.next : undefined,
    restored: s.restored === "1" || s.restored === 1 || s.restored === true ? "1" : undefined,
  }),
  component: LoginPage,
})

// Everything else requires a session.
const authed = createRoute({
  getParentRoute: () => rootRoute,
  id: "authed",
  beforeLoad: async ({ location }) => {
    const me = await queryClient.ensureQueryData({ queryKey: keys.me, queryFn: api.me })
    if (!me.authenticated) {
      throw redirect({ to: "/login", search: { next: location.href }, replace: true })
    }
  },
  pendingComponent: () => (
    <div className="grid min-h-svh place-items-center text-muted-foreground">
      <Spinner />
    </div>
  ),
  component: () => (
    <RequireAuth>
      <Outlet />
    </RequireAuth>
  ),
})

const adminRoute = createRoute({
  getParentRoute: () => authed,
  path: "admin",
  component: AdminLayout,
})
const adminIndex = createRoute({
  getParentRoute: () => adminRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/admin/profile" })
  },
  component: () => <Navigate to="/admin/profile" />,
})
const adminProfile = createRoute({ getParentRoute: () => adminRoute, path: "profile", component: ProfilePage })
const adminSettings = createRoute({ getParentRoute: () => adminRoute, path: "settings", component: SettingsPage })
const adminStorages = createRoute({ getParentRoute: () => adminRoute, path: "storages", component: StoragesPage })
const adminBackupRoute = createRoute({ getParentRoute: () => adminRoute, path: "backup", component: BackupPage })

const shell = createRoute({
  getParentRoute: () => authed,
  id: "shell",
  component: AppShell,
})

const homeRoute = createRoute({
  getParentRoute: () => shell,
  path: "/",
  validateSearch: (s: Record<string, unknown>): { page?: number } => ({ page: Number(s.page) > 0 ? Number(s.page) : undefined }),
  component: HomePage,
})

export type SearchKind = "all" | "folder" | "file"
const searchRoute = createRoute({
  getParentRoute: () => shell,
  path: "search",
  validateSearch: (s: Record<string, unknown>): { q: string; kind: SearchKind; page?: number } => ({
    q: typeof s.q === "string" ? s.q : "",
    kind: (s.kind === "folder" || s.kind === "file" ? s.kind : "all") as SearchKind,
    page: Number(s.page) > 1 ? Number(s.page) : undefined,
  }),
  component: SearchPage,
})

const downloadRoute = createRoute({
  getParentRoute: () => shell,
  path: "download/$",
  component: DownloadPage,
})

const browseRoute = createRoute({
  getParentRoute: () => shell,
  path: "$",
  validateSearch: (s: Record<string, unknown>): { page?: number } => ({ page: Number(s.page) > 0 ? Number(s.page) : undefined }),
  component: BrowsePage,
})

const routeTree = rootRoute.addChildren([
  loginRoute,
  authed.addChildren([
    adminRoute.addChildren([adminIndex, adminProfile, adminSettings, adminStorages, adminBackupRoute]),
    shell.addChildren([homeRoute, searchRoute, downloadRoute, browseRoute]),
  ]),
])

export const router = createRouter({
  routeTree,
  defaultPreload: false,
  scrollRestoration: true,
})

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router
  }
}

export { browseRoute, homeRoute, searchRoute, downloadRoute, loginRoute, adminBackupRoute }
