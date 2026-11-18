import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchCapped, fetchDeliveries, fetchEventsNewest, fetchWatches } from '@/api/queries'
import type { Delivery, EventRecord, Watch } from '@/api/types'
import { DeliveryStatusBadge, ErrorCount, RelativeTime } from '@/components/badges'
import { EmptyState, ErrorState, Head, PageHeader, Skeleton } from '@/components/states'
import { Stamp } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'
import { bumpTone } from '@/lib/release'
import { formatRelative } from '@/lib/time'
import { cn } from '@/lib/utils'
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
/** Past this many watches the rack opens in its compact, grouped form. */
export const COMPACT_FROM = 24
const DAY_MS = 24 * 60 * 60 * 1000
const DENSITY_KEY = 'angelia.overview.density'

const poll = { refetchInterval: REFRESH_MS, staleTime: REFRESH_MS / 2 }

function countLabel(n: number, capped: boolean) {
  return capped ? `${n}+` : String(n)
}

/** One count of the dateline strip under the title. */
function Count({
  label,
  to,
  query,
  hint,
  alarm,
}: {
  label: string
  to: '/watches' | '/events' | '/deliveries'
  query: {
    isPending: boolean
    isError: boolean
    data?: { n: number; capped: boolean }
  }
  hint?: string
  /** Shown in Flash red when the count is above zero. */
  alarm?: boolean
}) {
  const hot = alarm && (query.data?.n ?? 0) > 0
  return (
    <div className="min-w-0 border-border px-4 py-3 not-first:border-l max-sm:not-first:border-l-0 max-sm:[&:nth-child(even)]:border-l">
      <div className="wire-label text-muted-foreground">{label}</div>
      <div>
        {query.isPending ? (
          <Skeleton className="mt-1 h-8 w-12" />
        ) : query.isError || !query.data ? (
          <span className="text-sm text-destructive">unavailable</span>
        ) : (
          <Link
            to={to}
            className={cn('wire-title text-[2rem] no-underline [font-stretch:66%] hover:underline', hot && 'text-destructive')}
          >
            {countLabel(query.data.n, query.data.capped)}
          </Link>
        )}
        {hint && query.data?.capped && <p className="mt-1 text-xs text-muted-foreground">{hint}</p>}
      </div>
    </div>
  )
}

/**
 * The newest events and how many were recorded in the last 24 hours: the log is read newest
 * first (`order=desc`) until an event older than a day, so the count is exact unless it passes
 * the safety bound (`capped`). The first page also gives each watch's latest release.
 */
async function loadEvents(): Promise<{
  recent: EventRecord[]
  latest: Record<string, EventRecord>
  last24h: number
  capped: boolean
}> {
  const day = Date.now() - DAY_MS
  let before: number | undefined
  let last24h = 0
  let recent: EventRecord[] | undefined
  const latest: Record<string, EventRecord> = {}
  for (let i = 0; i < EVENT_COUNT_PAGES; i++) {
    const page = await fetchEventsNewest({}, before, EVENT_COUNT_PAGE)
    if (recent === undefined) {
      recent = page.items.slice(0, 10)
      for (const r of page.items) {
        const name = r.event.data.watch.name
        if (name && !latest[name] && !r.event.type.includes('yanked')) latest[name] = r
      }
    }
    const inDay = page.items.filter((r) => Date.parse(r.event.time) >= day).length
    last24h += inDay
    if (inDay < page.items.length || !page.has_more || page.next_before == null) return { recent, latest, last24h, capped: false }
    before = page.next_before
  }
  return { recent: recent ?? [], latest, last24h, capped: true }
}

/** What the upstream object is called: `owner/name` for a repository, else the package name. */
export function target(w: Watch): string {
  const c = w.coordinates
  return c.repo ?? c.name ?? Object.values(c).join(' · ')
}

/** Share of the time between the last check and the next one that has passed, 0 to 1. */
function checkProgress(w: Watch, now: number): number | undefined {
  const last = w.status.last_checked_at ? Date.parse(w.status.last_checked_at) : NaN
  const next = w.status.next_check_at ? Date.parse(w.status.next_check_at) : NaN
  if (Number.isNaN(last) || Number.isNaN(next) || next <= last) return undefined
  return Math.min(1, Math.max(0, (now - last) / (next - last)))
}

function readDensity(): 'rack' | 'compact' | undefined {
  try {
    const v = localStorage.getItem(DENSITY_KEY)
    return v === 'rack' || v === 'compact' ? v : undefined
  } catch {
    return undefined
  }
}

/** The slip sticking out of a cubby: the watch's latest release and what happened to it. */
function ReleaseSlip({ record, trouble }: { record: EventRecord; trouble?: Delivery }) {
  const d = record.event.data
  const p = bumpTone(d.bump)
  return (
    <div className="paper relative z-[1] -mx-1 mt-4 -mb-5 grid -rotate-[0.8deg] gap-1 border-t border-dashed px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <Stamp tone={p.tone}>{p.label}</Stamp>
        <span className="font-mono text-xs text-muted-foreground">
          <RelativeTime value={record.event.time} />
        </span>
      </div>
      <div className="flex items-center justify-between gap-2">
        <Link to="/events/$id" params={{ id: record.event.id }} className="truncate text-sm font-bold text-foreground">
          {shortType(record.event.type).replace('release.', '')} {d.version}
        </Link>
        {trouble && <DeliveryStatusBadge status={trouble.status} />}
      </div>
    </div>
  )
}

/** One pigeonhole of the rack: a watch, its latest version and when it is checked next. */
function Cubby({ watch: w, record, trouble, now }: { watch: Watch; record?: EventRecord; trouble?: Delivery; now: number }) {
  const progress = checkProgress(w, now)
  const failing = w.status.error_count > 0
  const d = record?.event.data
  return (
    <article
      aria-label={`Watch ${w.name}`}
      className={cn(
        'relative -ml-px flex min-h-60 min-w-0 flex-col border border-t-[3px] border-border border-t-foreground bg-card px-4 pb-4',
        record && 'pb-0',
        failing && 'shadow-[inset_0_0_0_2px_var(--destructive)]',
      )}
    >
      <div className="-mx-4 mb-3 flex items-center justify-between gap-2 bg-chrome-2 px-4 py-1.5 font-mono text-[0.72rem] tracking-[0.08em] text-canary uppercase">
        <span className="truncate">
          {w.source} · {w.scheme}
        </span>
        <span>{w.managed_by}</span>
      </div>
      <h3 className="wire-subtitle text-xl [font-stretch:75%]">
        <Link to="/watches/$name" params={{ name: w.name }} className="text-foreground no-underline hover:underline">
          {w.name}
        </Link>
      </h3>
      <div className="font-mono text-xs break-all text-muted-foreground">{target(w)}</div>
      {d ? (
        <>
          <div className="wire-title mt-3 text-[2.4rem] break-all [font-stretch:64%]">{d.version}</div>
          <div className="font-mono text-xs text-muted-foreground">
            {d.previous_version ? `from ${d.previous_version} · ` : ''}
            {d.bump ?? 'new'}
            {d.prerelease ? ' · prerelease' : ''}
          </div>
        </>
      ) : (
        <p className="mt-3 text-sm text-muted-foreground">
          {w.status.baseline_done ? 'No new release since the baseline.' : 'Waiting for the first check (the baseline).'}
        </p>
      )}
      {failing && (
        <div className="mt-3">
          <ErrorCount count={w.status.error_count} />
        </div>
      )}
      <div className="mt-auto grid gap-1 pt-4 font-mono text-xs text-muted-foreground">
        <span>
          {w.status.next_check_at ? (
            <>
              next check <b className="text-foreground">{formatRelative(w.status.next_check_at, now)}</b>
            </>
          ) : (
            'not scheduled'
          )}{' '}
          · every {w.status.effective_interval}
        </span>
        {progress !== undefined && (
          <div
            role="meter"
            aria-label={`Time to the next check of ${w.name}`}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Math.round(progress * 100)}
            className="h-1.5 bg-muted"
          >
            <div className="h-full bg-primary" style={{ width: `${Math.round(progress * 100)}%` }} />
          </div>
        )}
      </div>
      {record && <ReleaseSlip record={record} trouble={trouble} />}
    </article>
  )
}

/** The compact form for many watches: grouped by source, one dense row per watch. */
function CompactRack({ watches, latest, now }: { watches: Watch[]; latest: Record<string, EventRecord>; now: number }) {
  const groups = new Map<string, Watch[]>()
  for (const w of watches) groups.set(w.source, [...(groups.get(w.source) ?? []), w])
  return (
    <div className="grid gap-5 md:grid-cols-2 xl:grid-cols-3">
      {[...groups].map(([source, ws]) => (
        <section key={source} aria-label={`${source} watches`} className="min-w-0">
          <h3 className="wire-label flex justify-between border-b-2 border-foreground pb-1 text-gold-ink">
            <span>{source}</span>
            <span className="text-muted-foreground">{ws.length}</span>
          </h3>
          <ul>
            {ws.map((w) => (
              <li key={w.id} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2.5 border-b border-border py-1.5">
                <span
                  aria-label={w.status.error_count > 0 ? 'failing' : 'healthy'}
                  className={cn('size-2 rounded-full', w.status.error_count > 0 ? 'bg-destructive' : 'bg-ok')}
                />
                <span className="min-w-0">
                  <Link to="/watches/$name" params={{ name: w.name }} className="block truncate font-semibold text-foreground no-underline hover:underline">
                    {w.name}
                  </Link>
                  <span className="block truncate font-mono text-xs text-muted-foreground">{target(w)}</span>
                </span>
                <span className="text-right font-mono text-xs">
                  <span className="block font-bold">{latest[w.name]?.event.data.version ?? '—'}</span>
                  <span className="block text-muted-foreground">{w.status.next_check_at ? formatRelative(w.status.next_check_at, now) : '—'}</span>
                </span>
              </li>
            ))}
          </ul>
        </section>
      ))}
    </div>
  )
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
  const [chosen, setChosen] = useState(readDensity)
  const now = Date.now()

  const items = watches.data?.items ?? []
  const failing = items.filter((w) => w.status.error_count > 0)
  const density = chosen ?? (items.length > COMPACT_FROM ? 'compact' : 'rack')
  const latest = events.data?.latest ?? {}
  // A delivery that needs attention, by event id: dead first, then pending.
  const trouble: Record<string, Delivery> = {}
  for (const d of [...(pending.data?.items ?? []), ...(dead.data?.items ?? [])]) trouble[d.event_id] = d
  const deadItems = dead.data?.items ?? []

  function choose(v: 'rack' | 'compact') {
    setChosen(v)
    try {
      localStorage.setItem(DENSITY_KEY, v)
    } catch {
      // Storage may be blocked; the choice then lasts for this page only.
    }
  }

  const stat = (q: { isPending: boolean; isError: boolean; data?: { capped: boolean } }, n: number | undefined) => ({
    isPending: q.isPending,
    isError: q.isError,
    data: q.data && n !== undefined ? { n, capped: q.data.capped } : undefined,
  })

  return (
    <>
      <PageHeader title="Overview">
        {items.length > 0 && (
          <div role="group" aria-label="Rack density" className="inline-flex shadow-[inset_0_0_0_1.5px_var(--border)]">
            {(['rack', 'compact'] as const).map((v) => (
              <button
                key={v}
                type="button"
                aria-pressed={density === v}
                onClick={() => choose(v)}
                className={cn(
                  'h-8 px-3 text-sm font-semibold capitalize',
                  density === v ? 'bg-primary text-primary-foreground' : 'text-foreground hover:bg-muted',
                )}
              >
                {v}
              </button>
            ))}
          </div>
        )}
      </PageHeader>

      <section aria-label="Counts" className="mb-6 grid grid-cols-2 border border-border bg-card sm:grid-cols-3 xl:grid-cols-5">
        <Count label="Watches" to="/watches" query={stat(watches, watches.data?.items.length)} hint="More watches exist than were counted." />
        <Count label="Failing watches" to="/watches" query={stat(watches, failing.length)} alarm />
        <Count
          label="Events in the last 24 hours"
          to="/events"
          query={
            events.data
              ? { isPending: false, isError: false, data: { n: events.data.last24h, capped: events.data.capped } }
              : { isPending: events.isPending, isError: events.isError }
          }
          hint="Counted up to a bound; see Events."
        />
        <Count label="Pending deliveries" to="/deliveries" query={stat(pending, pending.data?.items.length)} />
        <Count label="Dead letters" to="/deliveries" query={stat(dead, dead.data?.items.length)} alarm />
      </section>

      {deadItems.length > 0 && (
        <div role="status" className="mb-6 flex flex-wrap items-center gap-3 border border-destructive bg-card px-4 py-3">
          <Stamp tone="flash">Flash</Stamp>
          <p className="min-w-48 flex-1">
            <b>
              {deadItems.length} dead {deadItems.length === 1 ? 'letter' : 'letters'}.
            </b>{' '}
            {deadItems.length === 1 ? 'A delivery' : 'Deliveries'} gave up after the last retry.
          </p>
          <Link to="/deliveries" className="font-semibold text-link">
            Review and replay
          </Link>
        </div>
      )}

      <section aria-label="Watches rack" className="mb-8">
        {watches.isPending ? (
          <Skeleton className="h-60 w-full" />
        ) : watches.isError ? (
          <ErrorState error={watches.error} onRetry={() => void watches.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState title="The rack is empty">
            Add a watch on the <Link to="/watches">Watches</Link> page, or in the watch file.
          </EmptyState>
        ) : density === 'compact' ? (
          <CompactRack watches={items} latest={latest} now={now} />
        ) : (
          <div className="grid w-fit max-w-full grid-cols-[repeat(auto-fit,minmax(16rem,21rem))] gap-y-8 pb-6 pl-px max-sm:w-full max-sm:grid-cols-1">
            {items.map((w) => {
              const record = latest[w.name]
              return <Cubby key={w.id} watch={w} record={record} trouble={record ? trouble[record.event.id] : undefined} now={now} />
            })}
          </div>
        )}
      </section>

      <div className="grid gap-6 lg:grid-cols-2">
        <section aria-labelledby="recent-events">
          <h2 id="recent-events" className="wire-subtitle mb-2 text-xl">
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
                    <TableCell className="font-mono text-xs">{shortType(r.event.type)}</TableCell>
                    <TableCell>
                      <Link to="/events/$id" params={{ id: r.event.id }} className="text-link underline">
                        {r.event.subject}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs whitespace-nowrap">
                      <RelativeTime value={r.event.time} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </section>

        <section aria-labelledby="failing-watches">
          <h2 id="failing-watches" className="wire-subtitle mb-2 text-xl">
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
                      <Link to="/watches/$name" params={{ name: w.name }} className="text-link underline">
                        {w.name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{w.source}</TableCell>
                    <TableCell className="font-mono text-xs">
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
