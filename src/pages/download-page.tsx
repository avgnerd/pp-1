import { useQuery } from "@tanstack/react-query"
import { useParams } from "@tanstack/react-router"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { PathCrumbs } from "@/components/browser/path-crumbs"
import { DownloadCard } from "@/components/preview/download-card"
import { api } from "@/lib/api"
import { fromSplat } from "@/lib/paths"

/** /download/<virtual path>: the download tab, reachable directly or from a preview. */
export function DownloadPage() {
  const { _splat } = useParams({ from: "/authed/shell/download/$" })
  const path = fromSplat(_splat)
  const stat = useQuery({ queryKey: ["stat", path], queryFn: () => api.stat(path) })
  return (
    <div className="flex flex-col gap-3">
      <PathCrumbs path={path} />
      {stat.isPending ? (
        <Skeleton className="h-56 w-full max-w-xl" />
      ) : stat.isError ? (
        <Alert variant="destructive">
          <AlertTitle>파일을 찾을 수 없습니다</AlertTitle>
          <AlertDescription>{(stat.error as Error).message}</AlertDescription>
        </Alert>
      ) : stat.data.type !== "file" ? (
        <Alert>
          <AlertTitle>폴더입니다</AlertTitle>
          <AlertDescription>폴더는 목록에서 우클릭한 뒤 '압축해서 다운로드'로 받으세요.</AlertDescription>
        </Alert>
      ) : (
        <DownloadCard entry={stat.data} showPreviewLink />
      )}
    </div>
  )
}
