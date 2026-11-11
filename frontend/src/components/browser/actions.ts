import { create } from "zustand"

import { api, urls, type Entry } from "@/lib/api"
import { copyText, shareLink, startDownload } from "@/lib/paths"
import { toast } from "@/components/ui/toast"

export type DialogKind = "rename" | "move" | "copy" | "delete"

interface DialogState {
  kind: DialogKind | null
  entries: Entry[]
  open: (kind: DialogKind, entries: Entry[]) => void
  close: () => void
}

/** Which file-operation dialog is open and for which entries. */
export const useActionDialog = create<DialogState>()((set) => ({
  kind: null,
  entries: [],
  open: (kind, entries) => set({ kind, entries }),
  close: () => set({ kind: null }),
}))

export function openInNewTab(path: string, publicUrl: string) {
  window.open(shareLink(path, publicUrl), "_blank", "noopener")
}

export async function copyLink(path: string, publicUrl: string) {
  const link = shareLink(path, publicUrl)
  const ok = await copyText(link)
  if (ok) toast.add({ title: "링크를 복사했습니다", description: link, type: "success", timeout: 3000 })
  else toast.add({ title: "링크를 복사하지 못했습니다", description: link, type: "error" })
}

/** One file downloads directly; folders or several items download as one ZIP. */
export async function downloadEntries(entries: Entry[]) {
  if (entries.length === 1 && entries[0].type === "file") {
    startDownload(urls.raw(entries[0].path, { download: true }), entries[0].name)
    return
  }
  try {
    const z = await api.zip(entries.map((e) => e.path))
    toast.add({ title: "압축 파일을 만드는 중입니다", description: `${z.name} 다운로드가 곧 시작됩니다`, type: "loading", timeout: 4000 })
    startDownload(z.url, z.name)
  } catch (e) {
    toast.add({ title: "압축 다운로드를 시작하지 못했습니다", description: (e as Error).message, type: "error" })
  }
}

/** Entries the app can rename, move, copy or delete (not mounts or virtual folders). */
export const isMutable = (e: Entry) => e.type === "file" || e.type === "folder"
