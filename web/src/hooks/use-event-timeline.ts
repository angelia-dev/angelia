import { useEffect, useMemo } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { EVENT_PAGE_SIZE, fetchEventsNewest, type EventFilters } from '@/api/queries'
import type { EventRecord } from '@/api/types'

/** Older pages fetched without asking while fewer than a page of events matched the filters. */
export const AUTO_PAGES = 5

/**
 * The timeline reads the log newest first (`order=desc`): the first page is the head, and
 * "older" pages follow with `before=<next_before>`. The first page's `next_after` is the highest
 * seq the server scanned, where the live stream picks up (`head`). A filtered page can be short
 * or empty while older events remain, so a few more pages are fetched until a page's worth
 * matched; then the user asks for more.
 */
export function useEventTimeline(filters: EventFilters) {
  const query = useInfiniteQuery({
    queryKey: ['events', filters],
    queryFn: ({ pageParam }) => fetchEventsNewest(filters, pageParam),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => (last.has_more && last.next_before != null ? last.next_before : undefined),
    // The live stream brings what is new; refetching every page from the head would only repeat it.
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })
  const { hasNextPage, isFetchingNextPage, isError, fetchNextPage } = query
  const pageCount = query.data?.pages.length ?? 0
  const loaded = useMemo<EventRecord[]>(() => query.data?.pages.flatMap((p) => p.items) ?? [], [query.data])

  useEffect(() => {
    if (hasNextPage && !isFetchingNextPage && !isError && pageCount > 0 && pageCount < AUTO_PAGES && loaded.length < EVENT_PAGE_SIZE) {
      void fetchNextPage()
    }
  }, [hasNextPage, isFetchingNextPage, isError, pageCount, loaded.length, fetchNextPage])

  const head = query.data?.pages[0]?.next_after ?? 0
  const ready = !query.isPending && !query.isError
  return { ...query, loaded, head, ready }
}

/** Loaded and live events merged by seq, newest first. */
export function mergeEvents(loaded: EventRecord[], live: EventRecord[]): EventRecord[] {
  const bySeq = new Map<number, EventRecord>()
  for (const e of loaded) bySeq.set(e.seq, e)
  for (const e of live) bySeq.set(e.seq, e)
  return [...bySeq.values()].sort((a, b) => b.seq - a.seq)
}
