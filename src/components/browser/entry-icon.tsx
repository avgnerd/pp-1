import {
  FileArchiveIcon,
  FileAudioIcon,
  FileCodeIcon,
  FileIcon,
  FileImageIcon,
  FileSpreadsheetIcon,
  FileTextIcon,
  FileVideoIcon,
  FolderIcon,
  FolderSymlinkIcon,
  HardDriveIcon,
} from "lucide-react"

import type { Entry } from "@/lib/api"
import { extOf, fileKind } from "@/lib/format"
import { cn } from "@/lib/utils"

export function EntryIcon({ entry, className }: { entry: Pick<Entry, "name" | "type">; className?: string }) {
  const cls = cn("size-4 shrink-0", className)
  switch (entry.type) {
    case "mount":
      return <HardDriveIcon className={cn(cls, "text-folder")} aria-hidden />
    case "virtual":
      return <FolderSymlinkIcon className={cn(cls, "text-folder-muted")} aria-hidden />
    case "folder":
      return <FolderIcon className={cn(cls, "fill-folder/25 text-folder")} aria-hidden />
  }
  const kind = fileKind(entry.name)
  const ext = extOf(entry.name)
  switch (kind) {
    case "image":
      return <FileImageIcon className={cls} aria-hidden />
    case "video":
      return <FileVideoIcon className={cls} aria-hidden />
    case "audio":
      return <FileAudioIcon className={cls} aria-hidden />
    case "archive":
      return <FileArchiveIcon className={cls} aria-hidden />
    case "office":
      return ["xls", "xlsx", "xlsm", "gsheet"].includes(ext) ? (
        <FileSpreadsheetIcon className={cls} aria-hidden />
      ) : (
        <FileTextIcon className={cls} aria-hidden />
      )
    case "pdf":
      return <FileTextIcon className={cls} aria-hidden />
    case "text":
      return ["txt", "md", "log", "srt", "vtt", "ass", "smi"].includes(ext) ? (
        <FileTextIcon className={cls} aria-hidden />
      ) : (
        <FileCodeIcon className={cls} aria-hidden />
      )
  }
  return <FileIcon className={cls} aria-hidden />
}
