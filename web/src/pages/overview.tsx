import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchCapped, fetchDeliveries, fetchEventsNewest, fetchWatches } from '@/api/queries'
import type { EventRecord } from '@/api/types'
import { ErrorCount, RelativeTime } from '@/components/badges'
import { EmptyState, ErrorState, Head, PageHeader, Skeleton } from '@/components/states'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'
import { shortType } from '@/pages/events'

export const REFRESH_MS = 30_000
/** Pages fetched per count; a count that hits the cap is shown as "N+". */
export const COUNT_PAGES = 3
/** Events per page of the 24-hour count; most instances see fewer in a day, so one call is enough. */
export const EVENT_COUNT_PAGE = 100
/** A safety bound on the count: past EVENT_COUNT_PAGES pages it is shown as "N+". */
export const EVENT_COUNT_PAGES = 100
/** The count may take several requests, so it runs at most once a minute. */
export const EVENTS_REFRESH_MS = 60_000
const DAY_MS = 24 * 60 * 60 * 1000

const poll = { refetchInterval: REFRESH_MS, staleTime: REFRESH_MS / 2 }

function countLabel(n: number, capped: boolean) {
  return capped ? `${n}+` : String(n)
}

function StatCard({
  label,
  to,
  query,
  hint,
}: {
  label: string
  to: '/watches' | '/events' | '/deliveries'
  query: {
    isPending: boolean
    isError: boolean
    data?: { n: number; capped: boolean }
  }
  hint?: string
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-medium text-muted-foreground">{label}</CardTitle>
      </CardHeader>
      <CardContent>
        {query.isPending ? (
          <Skeleton className="h-8 w-16" />
        ) : query.isError || !query.data ? (
          <span className="text-sm text-destructive">unavailable</span>
        ) : (
          <Link to={to} className="text-3xl font-semibold underline-offset-4 hover:underline">
            {countLabel(query.data.n, query.data.capped)}
          </Link>
        )}
        {hint && query.data?.capped && <p className="mt-1 text-xs text-muted-foreground">{hint}</p>}
      </CardContent>
    </Card>
  )
}

/**
 * The newest events and how many were recorded in the last 24 hours: the log is read newest
 * first (`order=desc`) until an event older than a day, so the count is exact unless it passes
 * the safety bound (`capped`).
 */
async function loadEvents(): Promise<{
  recent: EventRecord[]
  last24h: number
  capped: boolean
}> {
  const day = Date.now() - DAY_MS
  let before: number | undefined
  let last24h = 0
  let recent: EventRecord[] | undefined
  for (let i = 0; i < EVENT_COUNT_PAGES; i++) {
    const page = await fetchEventsNewest({}, before, EVENT_COUNT_PAGE)
    recent ??= page.items.slice(0, 10)
    const inDay = page.items.filter((r) => Date.parse(r.event.time) >= day).length
    last24h += inDay
    if (inDay < page.items.length || !page.has_more || page.next_before == null) return { recent, last24h, capped: false }
    before = page.next_before
  }
  return { recent: recent ?? [], last24h, capped: true }
}

export function OverviewPage() {
  const watches = useQuery({
    queryKey: ['overview', 'watches'],
    queryFn: () => fetchCapped(fetchWatches({}, 100), COUNT_PAGES),
    ...poll,
  })
  const events = useQuery({
    queryKey: ['overview', 'events'],
    queryFn: loadEvents,
    refetchInterval: EVENTS_REFRESH_MS,
    staleTime: EVENTS_REFRESH_MS,
  })
  const pending = useQuery({
    queryKey: ['overview', 'pending'],
    queryFn: () => fetchCapped(fetchDeliveries({ status: ['pending', 'in_flight'] }, 100), COUNT_PAGES),
    ...poll,
  })
  const dead = useQuery({
    queryKey: ['overview', 'dead'],
    queryFn: () => fetchCapped(fetchDeliveries({ status: ['dead'] }, 100), COUNT_PAGES),
    ...poll,
  })

  const failing = watches.data?.items.filter((w) => w.status.error_count > 0) ?? []
  const stat = (q: { isPending: boolean; isError: boolean; data?: { capped: boolean } }, n: number | undefined) => ({
    isPending: q.isPending,
    isError: q.isError,
    data: q.data && n !== undefined ? { n, capped: q.data.capped } : undefined,
  })

  return (
    <>
      <PageHeader title="Overview" />
      <section aria-label="Counts" className="mb-6 grid gap-4 sm:grid-cols-2 xl:grid-cols-5">
        <StatCard
          label="Watches"
          to="/watches"
          query={stat(watches, watches.data?.items.length)}
          hint="More watches exist than were counted."
        />
        <StatCard
          label="Failing watches"
          to="/watches"
          query={stat(watches, failing.length)}
          hint="Counted among the first watches only."
        />
        <StatCard
          label="Events in the last 24 hours"
          to="/events"
          query={
            events.data
              ? {
                  isPending: false,
                  isError: false,
                  data: { n: events.data.last24h, capped: events.data.capped },
                }
              : { isPending: events.isPending, isError: events.isError }
          }
          hint="Counted up to a bound; see Events."
        />
        <StatCard label="Pending deliveries" to="/deliveries" query={stat(pending, pending.data?.items.length)} />
        <StatCard label="Dead letters" to="/deliveries" query={stat(dead, dead.data?.items.length)} />
      </section>

      <div className="grid gap-6 lg:grid-cols-2">
        <section aria-labelledby="recent-events">
          <h2 id="recent-events" className="mb-2 text-lg font-semibold">
            Latest events
          </h2>
          {events.isPending ? (
            <Skeleton className="h-40 w-full" />
          ) : events.isError ? (
            <ErrorState error={events.error} onRetry={() => void events.refetch()} />
          ) : events.data.recent.length === 0 ? (
            <EmptyState title="No events yet" />
          ) : (
            <Table aria-label="Latest events">
              <Head columns={['Type', 'Subject', 'Time']} />
              <TableBody>
                {events.data.recent.map((r) => (
                  <TableRow key={r.seq}>
                    <TableCell>{shortType(r.event.type)}</TableCell>
                    <TableCell>
                      <Link to="/events/$id" params={{ id: r.event.id }} className="underline">
                        {r.event.subject}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <RelativeTime value={r.event.time} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </section>

        <section aria-labelledby="failing-watches">
          <h2 id="failing-watches" className="mb-2 text-lg font-semibold">
            Failing watches
          </h2>
          {watches.isPending ? (
            <Skeleton className="h-40 w-full" />
          ) : watches.isError ? (
            <ErrorState error={watches.error} onRetry={() => void watches.refetch()} />
          ) : failing.length === 0 ? (
            <EmptyState title="No failing watches">Every watch checked without errors.</EmptyState>
          ) : (
            <Table aria-label="Failing watches">
              <Head columns={['Name', 'Source', 'Last check', 'Errors']} />
              <TableBody>
                {failing.map((w) => (
                  <TableRow key={w.id}>
                    <TableCell>
                      <Link to="/watches/$name" params={{ name: w.name }} className="underline">
                        {w.name}
                      </Link>
                    </TableCell>
                    <TableCell>{w.source}</TableCell>
                    <TableCell>
                      <RelativeTime value={w.status.last_checked_at} />
                    </TableCell>
                    <TableCell>
                      <ErrorCount count={w.status.error_count} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </section>
      </div>
    </>
  )
}
