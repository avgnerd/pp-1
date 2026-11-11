import { useNavigate } from "@tanstack/react-router"
import { useWindowVirtualizer } from "@tanstack/react-virtual"
import { ArrowDownIcon, ArrowUpIcon, CopyIcon, DownloadIcon, FolderInputIcon, FolderOpenIcon, Trash2Icon, XIcon } from "lucide-react"
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react"

import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { LazyGallery as ImageGallery, rememberRatio } from "@/components/gallery/lazy-gallery"
import { urls, type Entry } from "@/lib/api"
import { fileKind, formatDate, hasPreview, thumbnailable } from "@/lib/format"
import { encodePath, toSplat } from "@/lib/paths"
import { useConfig } from "@/lib/queries"
import { cn } from "@/lib/utils"
import { usePrefs, useSelection, type SortKey } from "@/stores/ui"

import { ActionDialogs } from "./action-dialogs"
import { downloadEntries, isMutable, useActionDialog } from "./actions"
import { EntryIcon } from "./entry-icon"
import { EntryMenu } from "./entry-menu"
import { Pager } from "./pager"
import { SizeCell } from "./size-cell"
import { useListing } from "./use-listing"

const VIRTUALIZE_AT = 150

interface Props {
  path: string
  page: number
  onPage: (p: number) => void
  onCanonicalPage: (p: number) => void
}

export function FolderView({ path, page, onPage, onCanonicalPage }: Props) {
  const cfg = useConfig()
  const { view, sort, order, toggleSort } = usePrefs()
  const listing = useListing(path, cfg.data, sort, order, page)
  const resetFor = useSelection((s) => s.resetFor)
  const selected = useSelection((s) => s.selected)
  const setMany = useSelection((s) => s.setMany)
  const clearSel = useSelection((s) => s.clear)
  const [galleryIndex, setGalleryIndex] = useState<number | null>(null)

  useEffect(() => resetFor(path), [path, resetFor])

  const canonicalPage = listing.canonicalPage
  useEffect(() => {
    if (cfg.isSuccess && cfg.fetchStatus === "idle" && canonicalPage !== null && canonicalPage !== page) {
      onCanonicalPage(canonicalPage)
    }
  }, [cfg.isSuccess, cfg.fetchStatus, canonicalPage, page, onCanonicalPage])

  const entries = listing.entries
  const images = useMemo(() => entries.filter((e) => e.type === "file" && fileKind(e.name) === "image"), [entries])
  const selection = useMemo(() => entries.filter((e) => selected.has(e.path)), [entries, selected])
  const openImage = useCallback((e: Entry) => setGalleryIndex(images.findIndex((i) => i.path === e.path)), [images])

  if (listing.isError) {
    return (
      <Alert variant="destructive" className="mt-2">
        <AlertTitle>폴더를 열 수 없습니다</AlertTitle>
        <AlertDescription>{listing.error?.message}</AlertDescription>
      </Alert>
    )
  }

  const allChecked = entries.length > 0 && entries.every((e) => selected.has(e.path))
  const someChecked = !allChecked && entries.some((e) => selected.has(e.path))

  return (
    <div className="flex flex-col">
      {selection.length > 0 ? <SelectionBar selection={selection} onClear={clearSel} /> : null}

      {view === "list" ? (
        <div role="table" aria-label="파일 목록" aria-rowcount={listing.total} className="text-sm">
          <div role="row" className="grid grid-cols-[2rem_1.75rem_1fr_6.5rem] items-center gap-x-4 border-b px-2 py-1.5 text-xs text-muted-foreground sm:grid-cols-[2rem_1.75rem_1fr_6.5rem_9.5rem]">
            <span role="columnheader">
              <Checkbox
                checked={allChecked}
                indeterminate={someChecked}
                onCheckedChange={(v) => setMany(entries.map((e) => e.path), !!v)}
                aria-label="모두 선택"
                disabled={entries.length === 0}
              />
            </span>
            <span role="columnheader" aria-hidden />
            <SortHeader k="name" label="이름" sort={sort} order={order} onClick={toggleSort} />
            <SortHeader k="size" label="크기" sort={sort} order={order} onClick={toggleSort} className="justify-end" />
            <SortHeader k="modified" label="수정 시간" sort={sort} order={order} onClick={toggleSort} className="hidden sm:flex" />
          </div>
          {listing.isPending ? (
            <RowsSkeleton />
          ) : entries.length === 0 ? (
            <EmptyFolder />
          ) : entries.length >= VIRTUALIZE_AT ? (
            <VirtualRows entries={entries} selection={selection} onOpenImage={openImage} />
          ) : (
            entries.map((e) => <Row key={e.path} entry={e} selection={selection} checked={selected.has(e.path)} onOpenImage={openImage} />)
          )}
        </div>
      ) : listing.isPending ? (
        <GridSkeleton />
      ) : entries.length === 0 ? (
        <EmptyFolder />
      ) : (
        <Grid entries={entries} selection={selection} onOpenImage={openImage} />
      )}

      <Pager
        type={cfg.data?.pagination_type ?? "pages"}
        page={listing.page}
        pageCount={listing.pageCount}
        onPage={onPage}
        hasMore={listing.hasMore}
        loadMore={listing.loadMore}
        loading={listing.loadingMore}
        shown={entries.length}
        total={listing.total}
      />

      <ImageGallery images={images} index={galleryIndex} onIndexChange={setGalleryIndex} onClose={() => setGalleryIndex(null)} />
      <ActionDialogs />
    </div>
  )
}

function SortHeader({ k, label, sort, order, onClick, className }: { k: SortKey; label: string; sort: SortKey; order: string; onClick: (k: SortKey) => void; className?: string }) {
  const active = sort === k
  return (
    <span role="columnheader" aria-sort={active ? (order === "asc" ? "ascending" : "descending") : "none"} className={cn("flex", className)}>
      <button
        type="button"
        onClick={() => onClick(k)}
        className={cn("inline-flex items-center gap-1 rounded-sm px-1 py-0.5 hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring", active && "text-foreground")}
      >
        {label}
        {active ? order === "asc" ? <ArrowUpIcon className="size-3" aria-hidden /> : <ArrowDownIcon className="size-3" aria-hidden /> : null}
      </button>
    </span>
  )
}

function useOpen() {
  const navigate = useNavigate()
  const cfg = useConfig()
  return useCallback(
    (e: Entry) => {
      if (e.type !== "file" || hasPreview(e.name, cfg.data?.archive_preview ?? true)) {
        navigate({ to: "/$", params: { _splat: toSplat(e.path) } })
      } else {
        navigate({ to: "/download/$", params: { _splat: toSplat(e.path) } })
      }
    },
    [navigate, cfg.data?.archive_preview],
  )
}

function entryHref(entry: Entry, archivePreview: boolean): string {
  const path = encodePath(entry.path)
  return entry.type === "file" && !hasPreview(entry.name, archivePreview) ? `/download${path}` : path
}

/** The leading icon: a small thumbnail for images (click opens the original in lightGallery). */
function LeadIcon({ entry, onOpenImage }: { entry: Entry; onOpenImage: (e: Entry) => void }) {
  const [failed, setFailed] = useState(false)
  const isImage = entry.type === "file" && fileKind(entry.name) === "image"
  if (!isImage) return <EntryIcon entry={entry} />
  return (
    <button
      type="button"
      onClick={() => onOpenImage(entry)}
      aria-label={`${entry.name} 원본 보기`}
      className="grid size-6 place-items-center overflow-hidden rounded-sm ring-1 ring-border hover:ring-primary focus-visible:outline-2 focus-visible:outline-ring"
    >
      {thumbnailable(entry.name) && !failed ? (
        <img
          src={urls.thumb(entry.path, entry.rev, "w128h128")}
          alt=""
          loading="lazy"
          decoding="async"
          className="size-full object-cover"
          onLoad={(ev) => rememberRatio(entry.path, ev.currentTarget)}
          onError={() => setFailed(true)}
        />
      ) : (
        <EntryIcon entry={entry} />
      )}
    </button>
  )
}

const Row = memo(function Row({ entry, selection, checked, onOpenImage, style }: { entry: Entry; selection: Entry[]; checked: boolean; onOpenImage: (e: Entry) => void; style?: React.CSSProperties }) {
  const toggle = useSelection((s) => s.toggle)
  const open = useOpen()
  const cfg = useConfig()
  return (
    <div
      role="row"
      style={style}
      data-checked={checked || undefined}
      className="grid grid-cols-[2rem_1.75rem_1fr_6.5rem] items-center gap-x-4 border-b border-border/60 px-2 py-1.5 hover:bg-accent/50 data-checked:bg-primary/8 sm:grid-cols-[2rem_1.75rem_1fr_6.5rem_9.5rem]"
    >
      <span role="cell">
        <Checkbox checked={checked} onCheckedChange={() => toggle(entry.path)} aria-label={`${entry.name} 선택`} />
      </span>
      <span role="cell" className="flex items-center">
        <LeadIcon entry={entry} onOpenImage={onOpenImage} />
      </span>
      <span role="cell" className="min-w-0">
        <EntryMenu entry={entry} selection={selection}>
          <a
            href={entryHref(entry, cfg.data?.archive_preview ?? true)}
            onClick={(ev) => {
              if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.button === 1) return
              ev.preventDefault()
              open(entry)
            }}
            className={cn(
              "block truncate rounded-sm hover:underline focus-visible:outline-2 focus-visible:outline-ring",
              entry.type !== "file" && "font-medium",
            )}
            title={entry.name}
          >
            {entry.name}
          </a>
        </EntryMenu>
      </span>
      <span role="cell" className="text-right tabular-nums">
        <SizeCell entry={entry} />
      </span>
      <span role="cell" className="hidden tabular-nums text-muted-foreground sm:block">
        {entry.type === "file" ? formatDate(entry.modified) : ""}
      </span>
    </div>
  )
})

function VirtualRows({ entries, selection, onOpenImage }: { entries: Entry[]; selection: Entry[]; onOpenImage: (e: Entry) => void }) {
  const selected = useSelection((s) => s.selected)
  const parentRef = useRef<HTMLDivElement>(null)
  const [offset, setOffset] = useState(0)
  useEffect(() => {
    const el = parentRef.current
    if (el) setOffset(el.getBoundingClientRect().top + window.scrollY)
  }, [])
  const v = useWindowVirtualizer({ count: entries.length, estimateSize: () => 37, overscan: 12, scrollMargin: offset })
  return (
    <div ref={parentRef} style={{ height: v.getTotalSize(), position: "relative" }}>
      {v.getVirtualItems().map((it) => {
        const e = entries[it.index]
        return (
          <Row
            key={e.path}
            entry={e}
            selection={selection}
            checked={selected.has(e.path)}
            onOpenImage={onOpenImage}
            style={{ position: "absolute", top: 0, left: 0, right: 0, transform: `translateY(${it.start - v.options.scrollMargin}px)` }}
          />
        )
      })}
    </div>
  )
}

function Grid({ entries, selection, onOpenImage }: { entries: Entry[]; selection: Entry[]; onOpenImage: (e: Entry) => void }) {
  const selected = useSelection((s) => s.selected)
  const toggle = useSelection((s) => s.toggle)
  const open = useOpen()
  const cfg = useConfig()
  return (
    <ul className="grid grid-cols-[repeat(auto-fill,minmax(9.5rem,1fr))] gap-3 pt-3">
      {entries.map((e) => {
        const isImage = e.type === "file" && fileKind(e.name) === "image"
        const checked = selected.has(e.path)
        return (
          <li key={e.path} data-checked={checked || undefined} className="group relative flex flex-col gap-1.5 rounded-lg p-1.5 hover:bg-accent/50 data-checked:bg-primary/10">
            <button
              type="button"
              onClick={() => (isImage ? onOpenImage(e) : open(e))}
              aria-label={isImage ? `${e.name} 원본 보기` : `${e.name} 열기`}
              className="grid aspect-square place-items-center overflow-hidden rounded-md bg-muted/60 focus-visible:outline-2 focus-visible:outline-ring"
            >
              {isImage && thumbnailable(e.name) ? (
                <img
                  src={urls.thumb(e.path, e.rev, "w256h256")}
                  alt=""
                  loading="lazy"
                  decoding="async"
                  className="size-full object-cover"
                  onLoad={(ev) => rememberRatio(e.path, ev.currentTarget)}
                />
              ) : (
                <EntryIcon entry={e} className="size-10" />
              )}
            </button>
            <div className="absolute top-2.5 left-2.5 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100 data-checked:opacity-100" data-checked={checked || undefined}>
              <Checkbox checked={checked} onCheckedChange={() => toggle(e.path)} aria-label={`${e.name} 선택`} className="bg-background" />
            </div>
            <EntryMenu entry={e} selection={selection}>
              <a
                href={entryHref(e, cfg.data?.archive_preview ?? true)}
                onClick={(ev) => {
                  if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.button === 1) return
                  ev.preventDefault()
                  open(e)
                }}
                className="line-clamp-2 rounded-sm px-0.5 text-xs break-all hover:underline focus-visible:outline-2 focus-visible:outline-ring"
                title={e.name}
              >
                {e.name}
              </a>
            </EntryMenu>
          </li>
        )
      })}
    </ul>
  )
}

function SelectionBar({ selection, onClear }: { selection: Entry[]; onClear: () => void }) {
  const openDialog = useActionDialog((s) => s.open)
  const mutable = selection.every(isMutable)
  const sameStorage = selection.every((e) => e.storage_id && e.storage_id === selection[0].storage_id)
  return (
    <div className="sticky top-12 z-20 -mx-2 mb-2 flex flex-wrap items-center gap-1 rounded-lg border bg-card px-2 py-1.5 shadow-sm">
      <span className="px-1 text-sm font-medium">{selection.length}개 선택</span>
      <Button variant="ghost" size="sm" onClick={() => downloadEntries(selection)}>
        <DownloadIcon data-icon="inline-start" />
        {selection.length === 1 && selection[0].type === "file" ? "다운로드" : "압축해서 다운로드"}
      </Button>
      <Button variant="ghost" size="sm" onClick={() => openDialog("move", selection)} disabled={!mutable || !sameStorage}>
        <FolderInputIcon data-icon="inline-start" />
        이동
      </Button>
      <Button variant="ghost" size="sm" onClick={() => openDialog("copy", selection)} disabled={!mutable || !sameStorage}>
        <CopyIcon data-icon="inline-start" />
        복사
      </Button>
      <Button variant="ghost" size="sm" onClick={() => openDialog("delete", selection)} disabled={!mutable} className="text-destructive">
        <Trash2Icon data-icon="inline-start" />
        삭제
      </Button>
      <Button variant="ghost" size="icon-sm" onClick={onClear} aria-label="선택 해제" className="ml-auto">
        <XIcon />
      </Button>
    </div>
  )
}

function EmptyFolder() {
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <FolderOpenIcon />
        </EmptyMedia>
        <EmptyTitle>이 폴더는 비어 있습니다</EmptyTitle>
        <EmptyDescription>
          Dropbox에서 파일을 넣으면 캐시 만료 후 여기에 나타납니다. 바로 보려면 관리 &gt; 설정 &gt; 시스템에서 캐시를 비우세요.
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}

function RowsSkeleton() {
  return (
    <div className="flex flex-col gap-2 py-2">
      {Array.from({ length: 8 }, (_, i) => (
        <Skeleton key={i} className="h-7 w-full" />
      ))}
    </div>
  )
}

function GridSkeleton() {
  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(9.5rem,1fr))] gap-3 pt-3">
      {Array.from({ length: 12 }, (_, i) => (
        <Skeleton key={i} className="aspect-square w-full" />
      ))}
    </div>
  )
}
