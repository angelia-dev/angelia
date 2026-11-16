import { useMemo, useState } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchEvent, type EventFilters } from '@/api/queries'
import type { EventRecord, ShortEventType } from '@/api/types'
import { RelativeTime } from '@/components/badges'
import { FilterBar, SelectFilter, TextFilter } from '@/components/filters'
import { EmptyState, ErrorState, Head, PageHeader, Skeleton, TableSkeleton } from '@/components/states'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'
import { useDebounced } from '@/hooks/use-debounced'
import { useEventStream, type StreamStatus } from '@/hooks/use-event-stream'
import { mergeEvents, useEventTimeline } from '@/hooks/use-event-timeline'

const TYPES: ShortEventType[] = ['release.published', 'release.yanked', 'release.prerelease']

/** `dev.angelia.release.published.v1` -> `release.published`. */
export function shortType(type: string): string {
  return type.replace(/^dev\.angelia\./, '').replace(/\.v\d+$/, '')
}

const STREAM_TEXT: Record<StreamStatus, string> = {
  idle: 'Not live',
  connecting: 'Connecting…',
  live: 'Live',
  reconnecting: 'Connection lost, reconnecting…',
  error: 'Live updates stopped',
  unsupported: 'Live updates are not supported by this browser',
}

/** How many rows are drawn before "Show older". */
const ROWS = 200

export function EventsPage() {
  const [watch, setWatch] = useState('')
  const [type, setType] = useState('')
  const [source, setSource] = useState('')
  const [paused, setPaused] = useState(false)
  const [rows, setRows] = useState(ROWS)
  const debouncedWatch = useDebounced(watch.trim())
  const debouncedSource = useDebounced(source.trim())
  const filters = useMemo<EventFilters>(
    () => ({
      watch: debouncedWatch || undefined,
      type: (type || undefined) as ShortEventType | undefined,
      source: debouncedSource || undefined,
    }),
    [debouncedWatch, type, debouncedSource],
  )

  const timeline = useEventTimeline(filters)
  const stream = useEventStream({ filters, after: timeline.lastSeq, enabled: timeline.atTail && !paused })
  const events = useMemo(() => mergeEvents(timeline.loaded, stream.events), [timeline.loaded, stream.events])
  const shown = events.slice(0, rows)

  return (
    <>
      <PageHeader title="Events">
        <div className="flex items-center gap-3">
          <span role="status" className="text-sm">
            <span aria-hidden="true">{stream.status === 'live' ? '● ' : '○ '}</span>
            {paused ? 'Paused' : STREAM_TEXT[stream.status]}
          </span>
          {stream.status === 'error' && !paused && (
            <Button variant="outline" size="sm" onClick={stream.reconnect}>
              Reconnect
            </Button>
          )}
          <Button variant="outline" size="sm" aria-pressed={paused} onClick={() => setPaused((p) => !p)}>
            {paused ? 'Resume' : 'Pause'}
          </Button>
        </div>
      </PageHeader>
      <FilterBar label="Filter events">
        <TextFilter label="Watch (name or id)" value={watch} onChange={setWatch} />
        <SelectFilter
          label="Type"
          value={type}
          onChange={setType}
          options={[{ value: '', label: 'All types' }, ...TYPES.map((t) => ({ value: t, label: t }))]}
        />
        <TextFilter label="Source" value={source} onChange={setSource} placeholder="pypi" />
      </FilterBar>

      {timeline.isPending ? (
        <TableSkeleton label="events" columns={5} />
      ) : timeline.isError ? (
        <ErrorState error={timeline.error} onRetry={() => void timeline.refetch()} />
      ) : events.length === 0 ? (
        <EmptyState title="No events yet">Events appear here as watches detect new releases.</EmptyState>
      ) : (
        <>
          <Table aria-label="Events, newest first">
            <Head columns={['Seq', 'Type', 'Subject', 'Source', 'Time']} />
            <TableBody>
              {shown.map((r) => (
                <EventRow key={r.seq} record={r} />
              ))}
            </TableBody>
          </Table>
          <div className="mt-4 flex flex-wrap items-center gap-3">
            {events.length > shown.length && (
              <Button variant="outline" onClick={() => setRows((n) => n + ROWS)}>
                Show older events
              </Button>
            )}
            {timeline.hasNextPage && (
              <>
                <Button variant="outline" disabled={timeline.isFetchingNextPage} onClick={() => void timeline.fetchNextPage()}>
                  {timeline.isFetchingNextPage ? 'Loading…' : 'Load newer events'}
                </Button>
                <span className="text-sm text-muted-foreground">
                  Events are loaded oldest first; live updates start once the newest event is loaded.
                </span>
              </>
            )}
          </div>
        </>
      )}
    </>
  )
}

function EventRow({ record }: { record: EventRecord }) {
  const e = record.event
  return (
    <TableRow>
      <TableCell>{record.seq}</TableCell>
      <TableCell>
        <Badge variant={e.type.includes('yanked') ? 'destructive' : 'secondary'}>{shortType(e.type)}</Badge>
      </TableCell>
      <TableCell className="font-medium">
        <Link to="/events/$id" params={{ id: e.id }} className="underline">
          {e.subject}
        </Link>
      </TableCell>
      <TableCell>{e.data.source}</TableCell>
      <TableCell>
        <RelativeTime value={e.time} />
      </TableCell>
    </TableRow>
  )
}

export function EventDetailPage() {
  const { id } = useParams({ from: '/events/$id' })
  const query = useQuery({ queryKey: ['event', id], queryFn: () => fetchEvent(id) })
  if (query.isPending) return <Skeleton className="h-40 w-full" />
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  const { event: e, seq } = query.data
  return (
    <>
      <PageHeader title={e.subject} />
      <p className="mb-4 text-sm">
        <Link to="/events" className="underline">
          All events
        </Link>
      </p>
      <Table aria-label="Event summary" className="mb-4">
        <TableBody>
          <TableRow>
            <th scope="row" className="w-40 p-3 text-left font-medium text-muted-foreground">
              Type
            </th>
            <TableCell>{shortType(e.type)}</TableCell>
          </TableRow>
          <TableRow>
            <th scope="row" className="p-3 text-left font-medium text-muted-foreground">
              Sequence
            </th>
            <TableCell>{seq}</TableCell>
          </TableRow>
          <TableRow>
            <th scope="row" className="p-3 text-left font-medium text-muted-foreground">
              Time
            </th>
            <TableCell>
              <RelativeTime value={e.time} />
            </TableCell>
          </TableRow>
          <TableRow>
            <th scope="row" className="p-3 text-left font-medium text-muted-foreground">
              Watch
            </th>
            <TableCell>
              {e.data.watch.name ? (
                <Link to="/watches/$name" params={{ name: e.data.watch.name }} className="underline">
                  {e.data.watch.name}
                </Link>
              ) : (
                e.data.watch.id
              )}
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
      <h2 className="mb-2 text-lg font-semibold">CloudEvent</h2>
      <pre
        tabIndex={0}
        aria-label="CloudEvent JSON"
        className="overflow-auto rounded-lg border border-border bg-muted p-4 text-xs focus-visible:outline-2 focus-visible:outline-primary"
      >
        {JSON.stringify(e, null, 2)}
      </pre>
    </>
  )
}
