import { useMemo } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { api } from './client'
import { unwrap } from './problem'
import type { DeliveryStatus, ShortEventType } from './types'

/** Page size of the cursor-paged list calls. */
export const PAGE_SIZE = 100

export interface CursorPage<T> {
  items: T[]
  next_cursor: string | null
}

/** A cursor-paged list as an infinite query: one page now, "load more" for the next. */
export function useCursorList<T>(
  key: readonly unknown[],
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
) {
  const query = useInfiniteQuery({
    queryKey: key,
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  })
  const items = useMemo(() => query.data?.pages.flatMap((p) => p.items) ?? [], [query.data])
  return { ...query, items }
}

/** Fetches at most maxPages pages; `capped` says more exist than were fetched. */
export async function fetchCapped<T>(
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
  maxPages: number,
): Promise<{ items: T[]; capped: boolean }> {
  const items: T[] = []
  let cursor: string | undefined
  for (let i = 0; i < maxPages; i++) {
    const page = await fetchPage(cursor)
    items.push(...page.items)
    if (!page.next_cursor) return { items, capped: false }
    cursor = page.next_cursor
  }
  return { items, capped: true }
}

export interface WatchFilters {
  source?: string
  label?: string
}

export const fetchWatches = (filters: WatchFilters = {}, limit = PAGE_SIZE) =>
  (cursor: string | undefined) =>
    unwrap(
      api.GET('/v1/watches', {
        params: {
          query: {
            limit,
            cursor,
            source: filters.source ? [filters.source] : undefined,
            label: filters.label ? [filters.label] : undefined,
          },
        },
      }),
    )

export const fetchWatch = (name: string) =>
  unwrap(api.GET('/v1/watches/{name}', { params: { path: { name } } }))

export const fetchVersions = (name: string) => (cursor: string | undefined) =>
  unwrap(
    api.GET('/v1/watches/{name}/versions', {
      params: { path: { name }, query: { limit: PAGE_SIZE, cursor } },
    }),
  )

export interface EventFilters {
  watch?: string
  type?: ShortEventType
  source?: string
}

export const EVENT_PAGE_SIZE = 200

/**
 * Events newest first (`order=desc`): the head of the log, or the events below `before`
 * (the `next_before` of the previous page). `next_after` is the highest seq scanned, where the
 * live stream picks up.
 */
export const fetchEventsNewest = (filters: EventFilters, before?: number, limit = EVENT_PAGE_SIZE) =>
  unwrap(
    api.GET('/v1/events', {
      params: {
        query: {
          order: 'desc',
          before,
          limit,
          watch: filters.watch ? [filters.watch] : undefined,
          type: filters.type ? [filters.type] : undefined,
          source: filters.source ? [filters.source] : undefined,
        },
      },
    }),
  )

export const fetchEvent = (id: string) => unwrap(api.GET('/v1/events/{id}', { params: { path: { id } } }))

export interface DeliveryFilters {
  status?: DeliveryStatus[]
  /** A subscription id. */
  subscription?: string
  /** A sink name: the deliveries of the subscriptions that deliver to it now. */
  sink?: string
  /** RFC 3339; bounds the time the event was recorded, `since` inclusive, `until` exclusive. */
  since?: string
  until?: string
}

export const fetchDeliveries = (filters: DeliveryFilters = {}, limit = PAGE_SIZE) =>
  (cursor: string | undefined) =>
    unwrap(
      api.GET('/v1/deliveries', {
        params: {
          query: {
            limit,
            cursor,
            status: filters.status?.length ? filters.status : undefined,
            subscription: filters.subscription || undefined,
            sink: filters.sink || undefined,
            since: filters.since || undefined,
            until: filters.until || undefined,
          },
        },
      }),
    )

export const fetchDelivery = (id: string) =>
  unwrap(api.GET('/v1/deliveries/{id}', { params: { path: { id } } }))

export const fetchSinks = () => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/sinks', { params: { query: { limit: PAGE_SIZE, cursor } } }))

export const fetchSubscriptions = () => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/subscriptions', { params: { query: { limit: PAGE_SIZE, cursor } } }))

export const fetchConsumers = () => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/consumers', { params: { query: { limit: PAGE_SIZE, cursor } } }))

export const fetchPlugins = () => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/plugins', { params: { query: { limit: PAGE_SIZE, cursor } } }))

export const replayDelivery = (id: string) =>
  unwrap(api.POST('/v1/deliveries/{id}/replay', { params: { path: { id } }, body: {} }))

export interface ReplaySelector {
  event?: string
  subscription?: string
  since?: string
  until?: string
  status: ('dead' | 'delivered')[]
  dry_run: boolean
}

export const fetchTokens = (includeRevoked: boolean) => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/tokens', { params: { query: { limit: PAGE_SIZE, cursor, include_revoked: includeRevoked || undefined } } }))

export const createToken = (body: { name: string; scope: 'read' | 'write' | 'admin'; expires_at?: string }) =>
  unwrap(api.POST('/v1/tokens', { body }))

export const revokeToken = (id: string) =>
  unwrap(api.DELETE('/v1/tokens/{id}', { params: { path: { id } } }).then((r) => ({ ...r, data: r.data ?? (r.response.ok ? true : undefined) })))

export const fetchSessions = () => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/sessions', { params: { query: { limit: PAGE_SIZE, cursor } } }))

export const revokeSession = (id: string) =>
  unwrap(api.DELETE('/v1/sessions/{id}', { params: { path: { id } } }).then((r) => ({ ...r, data: r.data ?? (r.response.ok ? true : undefined) })))

/** The `ui.access` list (admin only). */
export const fetchAccess = () => unwrap(api.GET('/v1/access'))

export const createReplay = (selector: ReplaySelector) => unwrap(api.POST('/v1/replays', { body: selector }))
