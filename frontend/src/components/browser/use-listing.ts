import { keepPreviousData, useInfiniteQuery, useQuery } from "@tanstack/react-query"

import { api, type ClientConfig, type Entry } from "@/lib/api"
import type { SortKey, SortOrder } from "@/stores/ui"

export interface Listing {
  entries: Entry[]
  total: number
  isPending: boolean
  isError: boolean
  error: Error | null
  isFetching: boolean
  // pages
  page: number
  pageCount: number
  canonicalPage: number | null
  // more / auto_more
  hasMore: boolean
  loadMore: () => void
  loadingMore: boolean
}

/**
 * Loads a folder according to the admin's pagination type:
 * all = everything at once, pages = one page per ?page=, more / auto_more = growing list.
 */
export function useListing(path: string, cfg: ClientConfig | undefined, sort: SortKey, order: SortOrder, page: number): Listing {
  const type = cfg?.pagination_type ?? "pages"
  const per = cfg?.per_page ?? 30
  const paged = type === "pages" || type === "all"
  const offset = type === "pages" ? (page - 1) * per : 0
  const limit = type === "pages" ? per : 0

  const single = useQuery({
    queryKey: ["list", path, sort, order, offset, limit],
    queryFn: ({ signal }) => api.list(path, { sort, order, offset, limit }, signal),
    enabled: !!cfg && paged,
    placeholderData: keepPreviousData,
  })

  const infinite = useInfiniteQuery({
    queryKey: ["list", path, sort, order, "inf", per],
    queryFn: ({ pageParam, signal }) => api.list(path, { sort, order, offset: pageParam, limit: per }, signal),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.offset + last.entries.length < last.total ? last.offset + last.entries.length : undefined),
    enabled: !!cfg && !paged,
  })

  if (paged) {
    const total = single.data?.total ?? 0
    return {
      entries: single.data?.entries ?? [],
      total,
      isPending: !cfg || single.isPending,
      isError: single.isError,
      error: single.error as Error | null,
      isFetching: single.isFetching,
      page,
      pageCount: type === "pages" ? Math.max(1, Math.ceil(total / per)) : 1,
      canonicalPage: cfg && type === "pages" && single.isSuccess && !single.isPlaceholderData && single.fetchStatus === "idle"
        ? Math.min(page, Math.max(1, Math.ceil(total / per)))
        : null,
      hasMore: false,
      loadMore: () => {},
      loadingMore: false,
    }
  }
  const pages = infinite.data?.pages ?? []
  return {
    entries: pages.flatMap((p) => p.entries),
    total: pages[0]?.total ?? 0,
    isPending: !cfg || infinite.isPending,
    isError: infinite.isError,
    error: infinite.error as Error | null,
    isFetching: infinite.isFetching,
    page: 1,
    pageCount: 1,
    canonicalPage: null,
    hasMore: !!infinite.hasNextPage,
    loadMore: () => {
      if (infinite.hasNextPage && !infinite.isFetchingNextPage) infinite.fetchNextPage()
    },
    loadingMore: infinite.isFetchingNextPage,
  }
}
