import { useQuery } from "@tanstack/react-query"
import { useNavigate, useParams, useSearch } from "@tanstack/react-router"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { FolderView } from "@/components/browser/folder-view"
import { PathCrumbs } from "@/components/browser/path-crumbs"
import { FilePreview } from "@/components/preview/file-preview"
import { api } from "@/lib/api"
import { fromSplat } from "@/lib/paths"

/** Any virtual path: a folder shows the browser, a file shows its preview. */
export function BrowsePage() {
  const { _splat } = useParams({ from: "/authed/shell/$" })
  const { page } = useSearch({ from: "/authed/shell/$" })
  const navigate = useNavigate()
  const path = fromSplat(_splat)
  const stat = useQuery({ queryKey: ["stat", path], queryFn: () => api.stat(path), staleTime: 30_000 })

  return (
    <div className="flex flex-col gap-3">
      <PathCrumbs path={path} />
      {stat.isPending ? (
        <Skeleton className="h-64 w-full" />
      ) : stat.isError ? (
        <Alert variant="destructive">
          <AlertTitle>열 수 없습니다</AlertTitle>
          <AlertDescription>{(stat.error as Error).message}</AlertDescription>
        </Alert>
      ) : stat.data.type === "file" ? (
        <FilePreview entry={stat.data} />
      ) : (
        <FolderView
          key={path}
          path={path}
          page={page ?? 1}
          onCanonicalPage={(p) => {
            navigate({ to: "/$", params: { _splat: _splat ?? "" }, search: { page: p > 1 ? p : undefined }, replace: true })
          }}
          onPage={(p) => {
            navigate({ to: "/$", params: { _splat: _splat ?? "" }, search: { page: p > 1 ? p : undefined } })
            window.scrollTo({ top: 0 })
          }}
        />
      )}
    </div>
  )
}
