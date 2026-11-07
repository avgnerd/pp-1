import { useQuery } from "@tanstack/react-query"
import { ChevronRightIcon, FileIcon, FolderIcon, LockIcon } from "lucide-react"
import { useMemo, useState } from "react"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { api, type ArchiveEntry, type Entry } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { cn } from "@/lib/utils"

interface Node {
  name: string
  path: string
  dir: boolean
  size: number
  children: Node[]
}

function buildTree(entries: ArchiveEntry[]): Node {
  const root: Node = { name: "", path: "", dir: true, size: 0, children: [] }
  const index = new Map<string, Node>([["", root]])
  const ensure = (p: string): Node => {
    const hit = index.get(p)
    if (hit) return hit
    const i = p.lastIndexOf("/")
    const parent = ensure(i < 0 ? "" : p.slice(0, i))
    const n: Node = { name: i < 0 ? p : p.slice(i + 1), path: p, dir: true, size: 0, children: [] }
    parent.children.push(n)
    index.set(p, n)
    return n
  }
  for (const e of entries) {
    const p = e.name.replace(/\/+$/, "")
    if (e.dir) {
      ensure(p)
      continue
    }
    const i = p.lastIndexOf("/")
    const parent = ensure(i < 0 ? "" : p.slice(0, i))
    parent.children.push({ name: i < 0 ? p : p.slice(i + 1), path: p, dir: false, size: e.size, children: [] })
  }
  const total = (n: Node): number => {
    if (!n.dir) return n.size
    n.children.sort((a, b) => (a.dir !== b.dir ? (a.dir ? -1 : 1) : a.name.localeCompare(b.name, "ko", { numeric: true })))
    n.size = n.children.reduce((s, c) => s + total(c), 0)
    return n.size
  }
  total(root)
  return root
}

export default function ArchivePreview({ entry }: { entry: Entry }) {
  const q = useQuery({ queryKey: ["archive", entry.path, entry.rev], queryFn: () => api.archive(entry.path), staleTime: 10 * 60_000 })
  const tree = useMemo(() => (q.data ? buildTree(q.data.listing.entries) : null), [q.data])

  if (q.isPending) return <Skeleton className="h-80 w-full" />
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>압축 파일 목록을 읽지 못했습니다</AlertTitle>
        <AlertDescription>{(q.error as Error).message}</AlertDescription>
      </Alert>
    )
  }
  const { listing, budget_mb } = q.data
  const files = listing.entries.filter((e) => !e.dir).length
  return (
    <div className="flex flex-col gap-2">
      <p className="text-sm text-muted-foreground tabular-nums">
        {listing.format.toUpperCase()} 압축 파일, 파일 {files.toLocaleString()}개, 풀면 {formatBytes(tree?.size ?? 0)}
      </p>
      {listing.encrypted ? (
        <Alert>
          <LockIcon />
          <AlertTitle>암호가 걸린 압축 파일입니다</AlertTitle>
          <AlertDescription>내용은 다운로드한 뒤 압축 프로그램에서 암호를 넣어 여세요.</AlertDescription>
        </Alert>
      ) : null}
      {listing.truncated ? (
        <Alert>
          <AlertTitle>목록 일부만 표시합니다</AlertTitle>
          <AlertDescription>
            이 형식은 처음부터 읽어야 해서 앞부분 {budget_mb}MB까지만 확인했습니다. 상한은 관리 &gt; 설정 &gt; 시스템에서 바꿀 수 있습니다.
          </AlertDescription>
        </Alert>
      ) : null}
      <div role="tree" aria-label="압축 파일 내용" className="max-h-[70svh] overflow-auto rounded-lg border p-1 text-sm">
        {tree?.children.map((n) => <TreeNode key={n.path} node={n} depth={0} />)}
      </div>
    </div>
  )
}

function TreeNode({ node, depth }: { node: Node; depth: number }) {
  const [open, setOpen] = useState(depth < 1)
  const pad = { paddingLeft: `${depth * 1.1 + 0.4}rem` }
  if (!node.dir) {
    return (
      <div role="treeitem" className="flex items-center gap-2 rounded-md py-1 pr-2 hover:bg-accent/50" style={pad}>
        <span className="size-3.5" />
        <FileIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate">{node.name}</span>
        <span className="text-muted-foreground tabular-nums">{formatBytes(node.size)}</span>
      </div>
    )
  }
  return (
    <div role="treeitem" aria-expanded={open}>
      <button type="button" onClick={() => setOpen(!open)} className="flex w-full items-center gap-2 rounded-md py-1 pr-2 text-left hover:bg-accent/50 focus-visible:outline-2 focus-visible:outline-ring" style={pad}>
        <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} aria-hidden />
        <FolderIcon className="size-4 shrink-0 fill-folder/25 text-folder" aria-hidden />
        <span className="min-w-0 flex-1 truncate font-medium">{node.name}</span>
        <span className="text-muted-foreground tabular-nums">{formatBytes(node.size)}</span>
      </button>
      {open ? (
        <div role="group">
          {node.children.map((c) => (
            <TreeNode key={c.path} node={c} depth={depth + 1} />
          ))}
        </div>
      ) : null}
    </div>
  )
}
