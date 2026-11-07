import {
  CalculatorIcon,
  CopyIcon,
  DownloadIcon,
  ExternalLinkIcon,
  FolderInputIcon,
  LinkIcon,
  PencilIcon,
  Trash2Icon,
  XCircleIcon,
} from "lucide-react"
import type { ReactElement } from "react"

import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuGroup,
  ContextMenuItem,
  ContextMenuLabel,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import type { Entry } from "@/lib/api"
import { useConfig, useFolderSize, useFolderSizeActions } from "@/lib/queries"

import { copyLink, downloadEntries, isMutable, openInNewTab, useActionDialog } from "./actions"

interface Props {
  entry: Entry
  /** Entries currently checked; when the right-clicked entry is one of them, bulk actions use all. */
  selection: Entry[]
  children: ReactElement
}

/** Right-click menu on a file or folder name. */
export function EntryMenu({ entry, selection, children }: Props) {
  const cfg = useConfig()
  const publicUrl = cfg.data?.public_url ?? ""
  const openDialog = useActionDialog((s) => s.open)
  const fs = useFolderSize(entry.path)
  const sizeActions = useFolderSizeActions()

  const bulk = selection.length > 1 && selection.some((s) => s.path === entry.path) ? selection : [entry]
  const mutable = bulk.every(isMutable)
  const sameStorage = bulk.every((e) => e.storage_id && e.storage_id === bulk[0].storage_id)
  const isDir = entry.type !== "file"
  const sizing = fs.data?.status === "running" || fs.data?.status === "queued"
  const canSize = entry.type === "folder" || entry.type === "mount"

  return (
    <ContextMenu>
      <ContextMenuTrigger render={children} />
      <ContextMenuContent className="min-w-52">
        {bulk.length > 1 ? <ContextMenuLabel>{bulk.length}개 선택됨</ContextMenuLabel> : null}
        <ContextMenuGroup>
          <ContextMenuItem onClick={() => openInNewTab(entry.path, publicUrl)} disabled={bulk.length > 1}>
            <ExternalLinkIcon />새 탭에서 열기
          </ContextMenuItem>
          <ContextMenuItem onClick={() => openDialog("rename", [entry])} disabled={bulk.length > 1 || !isMutable(entry)}>
            <PencilIcon />
            이름 바꾸기
          </ContextMenuItem>
          <ContextMenuItem onClick={() => openDialog("move", bulk)} disabled={!mutable || !sameStorage}>
            <FolderInputIcon />
            이동
          </ContextMenuItem>
          <ContextMenuItem onClick={() => openDialog("copy", bulk)} disabled={!mutable || !sameStorage}>
            <CopyIcon />
            복사
          </ContextMenuItem>
        </ContextMenuGroup>
        <ContextMenuSeparator />
        <ContextMenuGroup>
          <ContextMenuItem onClick={() => copyLink(entry.path, publicUrl)} disabled={bulk.length > 1}>
            <LinkIcon />
            링크 복사
          </ContextMenuItem>
          <ContextMenuItem onClick={() => downloadEntries(bulk)}>
            <DownloadIcon />
            {bulk.length > 1 || isDir ? "압축해서 다운로드" : "다운로드"}
          </ContextMenuItem>
          {canSize ? (
            sizing ? (
              <ContextMenuItem onClick={() => sizeActions.cancel(entry.path)}>
                <XCircleIcon />
                계산 취소
              </ContextMenuItem>
            ) : (
              <ContextMenuItem onClick={() => sizeActions.start(entry.path)} disabled={bulk.length > 1}>
                <CalculatorIcon />
                폴더 크기
              </ContextMenuItem>
            )
          ) : null}
        </ContextMenuGroup>
        <ContextMenuSeparator />
        <ContextMenuGroup>
          <ContextMenuItem variant="destructive" onClick={() => openDialog("delete", bulk)} disabled={!mutable}>
            <Trash2Icon />
            삭제
          </ContextMenuItem>
        </ContextMenuGroup>
      </ContextMenuContent>
    </ContextMenu>
  )
}
