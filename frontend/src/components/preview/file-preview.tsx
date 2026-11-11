import { useQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { ChevronLeftIcon, ChevronRightIcon, DownloadIcon, LinkIcon, SquareArrowDownIcon } from "lucide-react"
import { lazy, Suspense, useMemo, useState } from "react"

import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { EntryIcon } from "@/components/browser/entry-icon"
import { copyLink, downloadEntries } from "@/components/browser/actions"
import { LazyGallery as ImageGallery } from "@/components/gallery/lazy-gallery"
import { api, urls, type Entry } from "@/lib/api"
import { fileKind, formatBytes, formatDate, hasPreview } from "@/lib/format"
import { parentPath, toSplat } from "@/lib/paths"
import { useConfig } from "@/lib/queries"
import { usePrefs } from "@/stores/ui"

import { DownloadCard } from "./download-card"

// Heavy viewers load only when a file of that kind is opened.
const MediaPlayer = lazy(() => import("@/components/player/media-player"))
const TextPreview = lazy(() => import("./text-preview"))
const ArchivePreview = lazy(() => import("./archive-preview"))

export function FilePreview({ entry }: { entry: Entry }) {
  const cfg = useConfig()
  const navigate = useNavigate()
  const { sort, order } = usePrefs()
  const kind = fileKind(entry.name)
  const siblings = useQuery({
    queryKey: ["siblings", parentPath(entry.path), sort, order],
    queryFn: () => api.siblings(entry.path, sort, order),
    staleTime: 60_000,
  })
  const archiveOn = cfg.data?.archive_preview ?? true
  const previewable = useMemo(() => (siblings.data ?? []).filter((e) => hasPreview(e.name, archiveOn)), [siblings.data, archiveOn])
  const idx = previewable.findIndex((e) => e.path === entry.path)
  const prev = idx > 0 ? previewable[idx - 1] : undefined
  const next = idx >= 0 && idx < previewable.length - 1 ? previewable[idx + 1] : undefined
  const go = (e?: Entry) => e && navigate({ to: "/$", params: { _splat: toSplat(e.path) } })

  return (
    <section className="flex flex-col gap-3" aria-label={entry.name}>
      <div className="flex flex-wrap items-start gap-3">
        <div className="flex min-w-0 flex-1 items-start gap-2">
          <EntryIcon entry={entry} className="mt-1 size-5" />
          <div className="min-w-0">
            <h1 className="text-base font-semibold break-all">{entry.name}</h1>
            <p className="text-sm text-muted-foreground tabular-nums">
              {formatBytes(entry.size)}, {formatDate(entry.modified)}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-1">
          <IconAction label="이전 파일" onClick={() => go(prev)} disabled={!prev}>
            <ChevronLeftIcon />
          </IconAction>
          <IconAction label="다음 파일" onClick={() => go(next)} disabled={!next}>
            <ChevronRightIcon />
          </IconAction>
          <IconAction label="링크 복사" onClick={() => copyLink(entry.path, cfg.data?.public_url ?? "")}>
            <LinkIcon />
          </IconAction>
          <Tooltip>
            <TooltipTrigger
              render={<Button variant="ghost" size="icon" aria-label="다운로드 탭" nativeButton={false} render={<Link to="/download/$" params={{ _splat: toSplat(entry.path) }} />} />}
            >
              <SquareArrowDownIcon />
            </TooltipTrigger>
            <TooltipContent>다운로드 탭</TooltipContent>
          </Tooltip>
          <Button onClick={() => downloadEntries([entry])}>
            <DownloadIcon data-icon="inline-start" />
            다운로드
          </Button>
        </div>
      </div>

      <Suspense fallback={<Skeleton className="aspect-video w-full" />}>
        <PreviewBody key={entry.path} entry={entry} kind={kind} siblings={siblings.data ?? []} archiveOn={archiveOn} />
      </Suspense>
    </section>
  )
}

function IconAction({ label, onClick, disabled, children }: { label: string; onClick: () => void; disabled?: boolean; children: React.ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger render={<Button variant="ghost" size="icon" aria-label={label} onClick={onClick} disabled={disabled} />}>{children}</TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  )
}

function PreviewBody({ entry, kind, siblings, archiveOn }: { entry: Entry; kind: ReturnType<typeof fileKind>; siblings: Entry[]; archiveOn: boolean }) {
  switch (kind) {
    case "video":
    case "audio":
      return <MediaPlayer entry={entry} siblings={siblings} />
    case "image":
      return <ImagePreview entry={entry} siblings={siblings} />
    case "text":
      return <TextPreview entry={entry} />
    case "pdf":
      return (
        <iframe
          title={entry.name}
          src={urls.raw(entry.path, { proxy: true })}
          className="h-[80svh] w-full rounded-lg border bg-white"
        />
      )
    case "office":
      // Documents arrive as PDF, spreadsheets as HTML; the server marks HTML with a sandbox CSP.
      return (
        <iframe
          title={entry.name}
          src={urls.officePreview(entry.path)}
          className="h-[80svh] w-full rounded-lg border bg-white"
        />
      )
    case "archive":
      return archiveOn ? <ArchivePreview entry={entry} /> : <DownloadCard entry={entry} note="관리자가 압축 파일 미리보기를 껐습니다." />
  }
  return <DownloadCard entry={entry} note="이 형식은 미리보기를 지원하지 않습니다." />
}

function ImagePreview({ entry, siblings }: { entry: Entry; siblings: Entry[] }) {
  const images = useMemo(() => {
    const list = siblings.filter((e) => fileKind(e.name) === "image")
    return list.length ? list : [entry]
  }, [siblings, entry])
  const [index, setIndex] = useState<number | null>(null)
  return (
    <>
      <button
        type="button"
        onClick={() => setIndex(Math.max(0, images.findIndex((i) => i.path === entry.path)))}
        className="grid place-items-center overflow-hidden rounded-lg border bg-black/40 focus-visible:outline-2 focus-visible:outline-ring"
        aria-label="크게 보기"
      >
        <img src={urls.raw(entry.path)} alt={entry.name} className="max-h-[78svh] w-auto object-contain" />
      </button>
      <ImageGallery images={images} index={index} onIndexChange={setIndex} onClose={() => setIndex(null)} />
    </>
  )
}
