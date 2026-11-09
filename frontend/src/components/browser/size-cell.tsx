import { Spinner } from "@/components/ui/spinner"
import type { Entry } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useFolderSize } from "@/lib/queries"

/**
 * File size, or for folders: "—" until the user asks for the size from the context menu,
 * then live progress, then the total.
 */
export function SizeCell({ entry }: { entry: Entry }) {
  if (entry.type === "file") return <span>{formatBytes(entry.size)}</span>
  return <FolderSize entry={entry} />
}

function FolderSize({ entry }: { entry: Entry }) {
  const fs = useFolderSize(entry.path).data
  if (fs?.status === "running" || fs?.status === "queued") {
    return (
      <span className="inline-flex items-center gap-1.5 text-muted-foreground" aria-live="polite">
        <Spinner className="size-3.5" />
        {fs.status === "queued" ? "대기 중" : formatBytes(fs.bytes)}
      </span>
    )
  }
  if (fs?.status === "done") {
    return <span title={`파일 ${fs.files.toLocaleString()}개, 폴더 ${fs.folders.toLocaleString()}개`}>{formatBytes(fs.bytes)}</span>
  }
  if (fs?.status === "error") {
    return (
      <span className="text-destructive" title={fs.error}>
        계산 실패
      </span>
    )
  }
  return <span className="text-muted-foreground">{entry.size != null ? formatBytes(entry.size) : "—"}</span>
}
