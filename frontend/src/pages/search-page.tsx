import { useNavigate, useSearch } from "@tanstack/react-router"
import { useWindowVirtualizer } from "@tanstack/react-virtual"
import { SearchIcon, SearchXIcon } from "lucide-react"
import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from "@/components/ui/input-group"
import { Progress } from "@/components/ui/progress"
import { Spinner } from "@/components/ui/spinner"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { EntryIcon } from "@/components/browser/entry-icon"
import { EntryMenu } from "@/components/browser/entry-menu"
import { ActionDialogs } from "@/components/browser/action-dialogs"
import { Pager } from "@/components/browser/pager"
import { SizeCell } from "@/components/browser/size-cell"
import { urls, type Entry } from "@/lib/api"
import { formatDate, hasPreview } from "@/lib/format"
import { encodePath, parentPath, toSplat } from "@/lib/paths"
import { useConfig, useSearchRevision } from "@/lib/queries"
import { subscribe } from "@/lib/sse"
import type { SearchKind } from "@/router"

interface StorageStatus {
  storage_id: number
  mount: string
  status: "done" | "error"
  count: number
  error?: string
  truncated?: boolean
}

type SearchState = { status: "running" | "done" } | { status: "error" | "interrupted"; message: string }

/** Searches every connected storage at once; results stream in as each account answers. */
export function SearchPage() {
  const { q, kind, page } = useSearch({ from: "/authed/shell/search" })
  const navigate = useNavigate()
  const [attempt, setAttempt] = useState(0)
  const revision = useSearchRevision().data
  return (
    <div className="flex flex-col gap-4">
      <SearchForm
        key={q}
        initial={q}
        kind={kind}
        onSubmit={(v) => {
          if (v === q) setAttempt((n) => n + 1)
          navigate({ to: "/search", search: { q: v, kind } })
        }}
        onKind={(k) => navigate({ to: "/search", search: { q, kind: k } })}
      />
      {q.trim() ? (
        <SearchResults
          key={JSON.stringify([kind, q, attempt, revision])}
          q={q}
          kind={kind}
          page={page ?? 1}
          onPage={(p) => {
            navigate({ to: "/search", search: { q, kind, page: p > 1 ? p : undefined } })
            window.scrollTo({ top: 0 })
          }}
        />
      ) : null}
      <ActionDialogs />
    </div>
  )
}

function SearchForm({ initial, kind, onSubmit, onKind }: { initial: string; kind: SearchKind; onSubmit: (q: string) => void; onKind: (k: SearchKind) => void }) {
  const [input, setInput] = useState(initial)
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        onSubmit(input.trim())
      }}
      className="flex flex-col gap-3 sm:flex-row sm:items-center"
      role="search"
    >
      <InputGroup className="sm:max-w-lg">
        <InputGroupAddon>
          <SearchIcon />
        </InputGroupAddon>
        <InputGroupInput value={input} onChange={(e) => setInput(e.target.value)} placeholder="모든 저장소에서 파일·폴더 이름 검색" aria-label="검색어" autoFocus />
        <InputGroupAddon align="inline-end">
          <InputGroupButton type="submit" variant="default">
            검색
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      <ToggleGroup value={[kind]} onValueChange={(v) => v[0] && onKind(v[0] as SearchKind)} variant="outline" size="sm" spacing={0} aria-label="검색 대상">
        <ToggleGroupItem value="all">전체</ToggleGroupItem>
        <ToggleGroupItem value="folder">폴더</ToggleGroupItem>
        <ToggleGroupItem value="file">파일</ToggleGroupItem>
      </ToggleGroup>
    </form>
  )
}

function SearchResults({ q, kind, page, onPage }: { q: string; kind: SearchKind; page: number; onPage: (p: number) => void }) {
  const navigate = useNavigate()
  const cfg = useConfig()
  const [results, setResults] = useState<Entry[]>([])
  const [statuses, setStatuses] = useState<StorageStatus[]>([])
  const [total, setTotal] = useState(0)
  const [state, setState] = useState<SearchState>({ status: "running" })
  const [truncated, setTruncated] = useState(false)
  const running = state.status === "running"
  const error = "message" in state ? state.message : null
  const type = cfg.data?.pagination_type ?? "pages"
  const per = cfg.data?.per_page ?? 30
  // Count "more" steps rather than rows so the first screen always follows the loaded per_page.
  const [moreSteps, setMoreSteps] = useState(0)

  useEffect(() => {
    let finished = false
    const close = subscribe(
      urls.search(q, kind),
      {
        start: (d) => {
          if (!finished) setTotal(d.storages ?? 0)
        },
        results: (d) => {
          if (!finished) setResults((r) => r.concat(d.entries ?? []))
        },
        storage: (d) => {
          if (!finished) setStatuses((s) => s.concat(d))
        },
        done: (d) => {
          if (finished) return
          finished = true
          setTruncated(d.truncated === true)
          setState({ status: "done" })
          close()
        },
        error: (d) => {
          if (finished) return
          finished = true
          setState({ status: "error", message: d.error })
          close()
        },
      },
      () => {
        if (!finished) {
          finished = true
          setState({ status: "interrupted", message: "검색 연결이 끊겼습니다. 검색 버튼을 눌러 다시 검색하세요. 이미 받은 결과는 아래에 표시됩니다." })
        }
      },
    )
    return () => {
      finished = true
      close()
    }
  }, [q, kind])

  const failed = statuses.filter((s) => s.status === "error")
  const partial = truncated || statuses.some((s) => s.truncated === true)
  const sorted = useMemo(() => [...results].sort((a, b) => (a.type === b.type ? a.path.localeCompare(b.path, "ko", { numeric: true }) : a.type === "file" ? 1 : -1)), [results])

  // Same pagination rules as folders and the storage list, applied to the streamed results.
  const pageCount = Math.max(1, Math.ceil(sorted.length / per))
  const curPage = Math.min(page, pageCount)
  useEffect(() => {
    if (state.status !== "done" || !cfg.data || cfg.isFetching || type !== "pages" || page === curPage) return
    navigate({
      to: "/search",
      search: (prev) => ({ ...prev, q, kind, page: curPage > 1 ? curPage : undefined }),
      replace: true,
      resetScroll: false,
    })
  }, [state.status, cfg.data, cfg.isFetching, type, page, curPage, navigate, q, kind])

  const visible = useMemo(() => {
    if (type === "all") return sorted
    if (type === "pages") return sorted.slice((curPage - 1) * per, curPage * per)
    return sorted.slice(0, per * (moreSteps + 1))
  }, [sorted, type, curPage, per, moreSteps])

  const open = useCallback(
    (e: Entry) => {
      if (e.type !== "file" || hasPreview(e.name, cfg.data?.archive_preview ?? true)) navigate({ to: "/$", params: { _splat: toSplat(e.path) } })
      else navigate({ to: "/download/$", params: { _splat: toSplat(e.path) } })
    },
    [navigate, cfg.data?.archive_preview],
  )

  return (
    <>
      <div className="flex flex-col gap-1.5 text-sm text-muted-foreground" aria-live="polite">
        <div className="flex items-center gap-2">
          {running ? <Spinner /> : null}
          <span>
            {state.status === "running" ? "검색 중" : state.status === "done" ? "검색 완료" : state.status === "interrupted" ? "검색 중단" : "검색 실패"}: 저장소 {statuses.length}/{total}, 결과 {results.length.toLocaleString()}개
          </span>
        </div>
        {running && total > 0 ? <Progress value={(statuses.length / total) * 100} className="max-w-lg" /> : null}
      </div>

      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      {partial ? (
        <Alert>
          <AlertDescription>저장소별 검색 한도에 도달해 일부 결과만 표시합니다. 현재 받은 결과는 {results.length.toLocaleString()}개입니다. 검색어를 더 구체적으로 입력해 다시 검색하세요.</AlertDescription>
        </Alert>
      ) : null}
      {failed.length > 0 ? (
        <Alert>
          <AlertDescription>
            {failed.length}개 저장소에서 검색하지 못했습니다: {failed.slice(0, 3).map((f) => `${f.mount} (${f.error})`).join(", ")}
            {failed.length > 3 ? ` 외 ${failed.length - 3}개` : ""}
          </AlertDescription>
        </Alert>
      ) : null}

      {q && state.status === "done" && results.length === 0 && failed.length === 0 && !partial ? (
        <Empty className="py-14">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <SearchXIcon />
            </EmptyMedia>
            <EmptyTitle>'{q}'에 맞는 항목이 없습니다</EmptyTitle>
            <EmptyDescription>이름의 일부만 넣거나 검색 대상을 '전체'로 바꿔 보세요. Dropbox 검색 색인은 새 파일을 반영하는 데 몇 분 걸릴 수 있습니다.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {visible.length > 0 ? (
        visible.length >= VIRTUALIZE_AT ? (
          <VirtualResults entries={visible} onOpen={open} archivePreview={cfg.data?.archive_preview ?? true} />
        ) : (
          <ul className="flex flex-col divide-y divide-border/60 border-y text-sm">
            {visible.map((e) => (
              <ResultRow key={e.path} entry={e} onOpen={open} archivePreview={cfg.data?.archive_preview ?? true} />
            ))}
          </ul>
        )
      ) : null}
      <Pager
        type={type}
        page={curPage}
        pageCount={pageCount}
        onPage={onPage}
        hasMore={visible.length < sorted.length}
        loadMore={() => setMoreSteps((n) => n + 1)}
        loading={false}
        shown={visible.length}
        total={sorted.length}
      />
    </>
  )
}

const VIRTUALIZE_AT = 150

/** Long result lists ("모두" or a grown "더 보기") render only the rows near the viewport. */
function VirtualResults({ entries, onOpen, archivePreview }: { entries: Entry[]; onOpen: (e: Entry) => void; archivePreview: boolean }) {
  const ref = useRef<HTMLUListElement>(null)
  const [offset, setOffset] = useState(0)
  useLayoutEffect(() => {
    if (ref.current) setOffset(ref.current.getBoundingClientRect().top + window.scrollY)
  }, [])
  const v = useWindowVirtualizer({ count: entries.length, estimateSize: () => 53, overscan: 10, scrollMargin: offset })
  return (
    <ul ref={ref} className="relative border-y text-sm" style={{ height: v.getTotalSize() }}>
      {v.getVirtualItems().map((it) => (
        <ResultRow
          key={entries[it.index].path}
          entry={entries[it.index]}
          onOpen={onOpen}
          archivePreview={archivePreview}
          index={it.index}
          measure={v.measureElement}
          style={{ position: "absolute", top: 0, left: 0, right: 0, transform: `translateY(${it.start - v.options.scrollMargin}px)` }}
        />
      ))}
    </ul>
  )
}

const ResultRow = memo(function ResultRow({
  entry: e,
  onOpen,
  archivePreview,
  style,
  index,
  measure,
}: {
  entry: Entry
  onOpen: (e: Entry) => void
  archivePreview: boolean
  style?: React.CSSProperties
  index?: number
  measure?: (el: Element | null) => void
}) {
  const navigate = useNavigate()
  return (
    <li
      ref={measure}
      data-index={index}
      style={style}
      className="grid grid-cols-[1.25rem_1fr_6rem] items-center gap-x-3 border-b border-border/60 px-2 py-2 last:border-b-0 hover:bg-accent/50 sm:grid-cols-[1.25rem_1fr_6rem_9.5rem]"
    >
      <EntryIcon entry={e} />
      <div className="min-w-0">
        <EntryMenu entry={e} selection={[]}>
          <a
            href={(e.type === "file" && !hasPreview(e.name, archivePreview) ? "/download" : "") + encodePath(e.path)}
            onClick={(ev) => {
              if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.altKey || ev.button !== 0) return
              ev.preventDefault()
              onOpen(e)
            }}
            className="block truncate font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring"
          >
            {e.name}
          </a>
        </EntryMenu>
        <a
          href={encodePath(parentPath(e.path))}
          onClick={(ev) => {
            if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.altKey || ev.button !== 0) return
            ev.preventDefault()
            navigate({ to: "/$", params: { _splat: toSplat(parentPath(e.path)) } })
          }}
          className="block truncate text-xs text-muted-foreground hover:text-foreground"
        >
          {parentPath(e.path)}
        </a>
      </div>
      <span className="text-right tabular-nums">
        <SizeCell entry={e} />
      </span>
      <span className="hidden tabular-nums text-muted-foreground sm:block">{formatDate(e.modified)}</span>
    </li>
  )
})
