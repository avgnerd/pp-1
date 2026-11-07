import { useEffect, useRef } from "react"

import { Button } from "@/components/ui/button"
import {
  Pagination,
  PaginationContent,
  PaginationEllipsis,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination"
import { Spinner } from "@/components/ui/spinner"
import type { PaginationType } from "@/lib/api"

function pageList(page: number, count: number): (number | "…")[] {
  const set = new Set([1, count, page - 1, page, page + 1].filter((p) => p >= 1 && p <= count))
  const sorted = [...set].sort((a, b) => a - b)
  const out: (number | "…")[] = []
  sorted.forEach((p, i) => {
    if (i > 0 && p - sorted[i - 1] > 1) out.push("…")
    out.push(p)
  })
  return out
}

interface Props {
  type: PaginationType
  page: number
  pageCount: number
  onPage: (p: number) => void
  hasMore: boolean
  loadMore: () => void
  loading: boolean
  shown: number
  total: number
}

/** Paging controls for the four pagination types an admin can choose. */
export function Pager({ type, page, pageCount, onPage, hasMore, loadMore, loading, shown, total }: Props) {
  if (type === "all") return null
  if (type === "pages") {
    if (pageCount <= 1) return null
    const go = (p: number) => (e: React.MouseEvent) => {
      e.preventDefault()
      if (p >= 1 && p <= pageCount && p !== page) onPage(p)
    }
    return (
      <Pagination className="py-3">
        <PaginationContent>
          <PaginationItem>
            <PaginationPrevious href="#" text="이전" onClick={go(page - 1)} aria-disabled={page <= 1} className={page <= 1 ? "pointer-events-none opacity-50" : undefined} />
          </PaginationItem>
          {pageList(page, pageCount).map((p, i) =>
            p === "…" ? (
              <PaginationItem key={`e${i}`}>
                <PaginationEllipsis />
              </PaginationItem>
            ) : (
              <PaginationItem key={p}>
                <PaginationLink href="#" isActive={p === page} onClick={go(p)} aria-label={`${p}쪽`}>
                  {p}
                </PaginationLink>
              </PaginationItem>
            ),
          )}
          <PaginationItem>
            <PaginationNext
              href="#"
              text="다음"
              onClick={go(page + 1)}
              aria-disabled={page >= pageCount}
              className={page >= pageCount ? "pointer-events-none opacity-50" : undefined}
            />
          </PaginationItem>
        </PaginationContent>
      </Pagination>
    )
  }
  return <MoreLoader auto={type === "auto_more"} hasMore={hasMore} loadMore={loadMore} loading={loading} shown={shown} total={total} />
}

function MoreLoader({ auto, hasMore, loadMore, loading, shown, total }: { auto: boolean; hasMore: boolean; loadMore: () => void; loading: boolean; shown: number; total: number }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!auto || !hasMore || !ref.current) return
    const io = new IntersectionObserver((es) => es.some((e) => e.isIntersecting) && loadMore(), { rootMargin: "400px" })
    io.observe(ref.current)
    return () => io.disconnect()
  }, [auto, hasMore, loadMore])
  if (!hasMore) return null
  return (
    <div ref={ref} className="flex items-center justify-center gap-3 py-4 text-sm text-muted-foreground">
      {auto ? (
        loading ? (
          <>
            <Spinner /> 불러오는 중 ({shown.toLocaleString()} / {total.toLocaleString()})
          </>
        ) : (
          <span>
            {shown.toLocaleString()} / {total.toLocaleString()}
          </span>
        )
      ) : (
        <Button variant="outline" onClick={loadMore} disabled={loading}>
          {loading ? <Spinner data-icon="inline-start" /> : null}
          더 보기 ({shown.toLocaleString()} / {total.toLocaleString()})
        </Button>
      )}
    </div>
  )
}
