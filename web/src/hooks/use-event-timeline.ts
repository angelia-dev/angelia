import { useEffect, useMemo } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { fetchEvents, type EventFilters } from '@/api/queries'
import type { EventRecord } from '@/api/types'

/** Pages fetched without asking before the user must press "Load newer events". */
export const AUTO_PAGES = 5

/**
 * The events API lists oldest first and pages by `seq`, and has no way to jump
 * to the newest event. The timeline therefore walks forward from the oldest
 * event kept, up to AUTO_PAGES pages, then waits for the user. Only once it has
 * reached the newest event (`atTail`) does the live stream take over.
 */
export function useEventTimeline(filters: EventFilters) {
  const query = useInfiniteQuery({
    queryKey: ['events', filters],
    queryFn: ({ pageParam }) => fetchEvents(filters, pageParam),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.has_more ? last.next_after : undefined),
  })
  const { hasNextPage, isFetchingNextPage, isError, fetchNextPage } = query
  const pageCount = query.data?.pages.length ?? 0

  useEffect(() => {
    if (hasNextPage && !isFetchingNextPage && !isError && pageCount > 0 && pageCount < AUTO_PAGES) {
      void fetchNextPage()
    }
  }, [hasNextPage, isFetchingNextPage, isError, pageCount, fetchNextPage])

  const loaded = useMemo<EventRecord[]>(() => query.data?.pages.flatMap((p) => p.items) ?? [], [query.data])
  const lastPage = query.data?.pages[pageCount - 1]
  const lastSeq = lastPage ? lastPage.next_after : 0
  const atTail = !query.isPending && !query.isError && !hasNextPage
  return { ...query, loaded, lastSeq, atTail }
}

/** Loaded and live events merged by seq, newest first. */
export function mergeEvents(loaded: EventRecord[], live: EventRecord[]): EventRecord[] {
  const bySeq = new Map<number, EventRecord>()
  for (const e of loaded) bySeq.set(e.seq, e)
  for (const e of live) bySeq.set(e.seq, e)
  return [...bySeq.values()].sort((a, b) => b.seq - a.seq)
}
