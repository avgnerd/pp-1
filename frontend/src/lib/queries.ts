import { QueryClient, useQuery, useQueryClient } from "@tanstack/react-query"
import { useCallback } from "react"

import { api, type Entry } from "@/lib/api"
import { urls } from "@/lib/api"
import { subscribe } from "@/lib/sse"

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: (count, err: unknown) => {
        const status = (err as { status?: number } | null)?.status
        return status && status < 500 ? false : count < 1
      },
      refetchOnWindowFocus: false,
    },
  },
})

export const keys = {
  me: ["me"] as const,
  config: ["config"] as const,
  mounts: ["mounts"] as const,
  searchRevision: ["search-revision"] as const,
  list: (path: string) => ["list", path] as const,
  folderSize: (path: string) => ["fsize", path] as const,
}

export function useMe() {
  return useQuery({ queryKey: keys.me, queryFn: api.me, staleTime: 60_000 })
}

export function useConfig() {
  return useQuery({ queryKey: keys.config, queryFn: api.config, staleTime: 60_000 })
}

export function useMounts() {
  return useQuery({ queryKey: keys.mounts, queryFn: api.mounts, staleTime: 60_000 })
}

export function useSearchRevision() {
  return useQuery({ queryKey: keys.searchRevision, queryFn: () => 0, initialData: 0, enabled: false })
}

/**
 * After a rename/move/copy/delete: refetch the affected folder listings and forget
 * on-demand folder sizes of every ancestor and descendant of the changed paths.
 */
export function afterChange(changed: string[]) {
  queryClient.setQueryData<number>(keys.searchRevision, (revision = 0) => revision + 1)
  const isRelated = (a: string, b: string) => a === b || a.startsWith(b === "/" ? "/" : b + "/") || b.startsWith(a === "/" ? "/" : a + "/")
  queryClient.removeQueries({
    queryKey: ["fsize"],
    predicate: (q) => {
      const p = q.queryKey[1] as string
      return changed.some((c) => isRelated(p, c))
    },
  })
  queryClient.invalidateQueries({
    queryKey: ["list"],
    predicate: (q) => {
      const p = q.queryKey[1] as string
      return changed.some((c) => isRelated(p, c))
    },
  })
}

export interface FolderSizeState {
  status: "running" | "queued" | "done" | "error"
  bytes: number
  files: number
  folders: number
  error?: string
}

const running = new Map<string, () => void>()

/** On-demand folder size: nothing is computed until start() is called from the context menu. */
export function useFolderSizeActions() {
  const qc = useQueryClient()
  const start = useCallback(
    (path: string) => {
      if (running.has(path)) return
      qc.setQueryData<FolderSizeState>(keys.folderSize(path), { status: "running", bytes: 0, files: 0, folders: 0 })
      let finished = false
      const close = subscribe(
        urls.folderSize(path),
        {
          progress: (d) =>
            qc.setQueryData<FolderSizeState>(keys.folderSize(path), {
              status: d.queued ? "queued" : "running",
              bytes: d.bytes ?? 0,
              files: d.files ?? 0,
              folders: d.folders ?? 0,
            }),
          done: (d) => {
            finished = true
            running.get(path)?.()
            running.delete(path)
            qc.setQueryData<FolderSizeState>(keys.folderSize(path), { status: "done", bytes: d.bytes, files: d.files, folders: d.folders })
          },
          error: (d) => {
            finished = true
            running.get(path)?.()
            running.delete(path)
            qc.setQueryData<FolderSizeState>(keys.folderSize(path), { status: "error", bytes: 0, files: 0, folders: 0, error: d.error })
          },
        },
        () => {
          running.delete(path)
          if (!finished) {
            const cur = qc.getQueryData<FolderSizeState>(keys.folderSize(path))
            if (cur && cur.status !== "done") {
              qc.setQueryData<FolderSizeState>(keys.folderSize(path), { ...cur, status: "error", error: "연결이 끊겨 계산을 멈췄습니다" })
            }
          }
        },
      )
      running.set(path, close)
    },
    [qc],
  )
  const cancel = useCallback(
    (path: string) => {
      running.get(path)?.()
      running.delete(path)
      qc.removeQueries({ queryKey: keys.folderSize(path) })
    },
    [qc],
  )
  const isRunning = useCallback((path: string) => running.has(path), [])
  return { start, cancel, isRunning }
}

export function useFolderSize(path: string) {
  return useQuery<FolderSizeState | null>({
    queryKey: keys.folderSize(path),
    queryFn: () => null,
    enabled: false,
    staleTime: Infinity,
    gcTime: 30 * 60_000,
  })
}

/** Size to show for a row: file size, a cached server-side folder size, or the on-demand result. */
export function displaySize(e: Entry, fs: FolderSizeState | null | undefined): number | null {
  if (e.type === "file") return e.size
  if (fs?.status === "done") return fs.bytes
  return e.size ?? null
}
