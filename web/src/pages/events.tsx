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
import { invalidHelp, KIND_PATTERN, NAME_OR_ID_PATTERN, validOrUndefined } from '@/lib/filters'
import { mergeEvents, useEventTimeline } from '@/hooks/use-event-timeline'
import { Bulletin, EventTape, QuietTape, RANGES, TapeLegend, type TapeRange } from '@/components/event-tape'
import { cn } from '@/lib/utils'

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
  const [range, setRange] = useState<TapeRange>('7d')
  const [picked, setPicked] = useState<number>()
  const debouncedWatch = useDebounced(watch.trim())
  const debouncedSource = useDebounced(source.trim())
  const filters = useMemo<EventFilters>(
    () => ({
      watch: validOrUndefined(debouncedWatch, NAME_OR_ID_PATTERN),
      type: (type || undefined) as ShortEventType | undefined,
      source: validOrUndefined(debouncedSource, KIND_PATTERN),
    }),
    [debouncedWatch, type, debouncedSource],
  )

  const timeline = useEventTimeline(filters)
  const stream = useEventStream({
    filters,
    after: timeline.head,
    enabled: timeline.ready && !paused,
  })
  const events = useMemo(() => mergeEvents(timeline.loaded, stream.events), [timeline.loaded, stream.events])
  const shown = events.slice(0, rows)
  const now = Date.now()
  const rangeInfo = RANGES.find((r) => r.value === range)!
  const inRange = events.filter((r) => Date.parse(r.event.time) >= now - rangeInfo.ms)
  // The picked release, else the newest one in the range.
  const selected = inRange.find((r) => r.seq === picked) ?? inRange[0]

  return (
    <>
      <PageHeader title="Events">
        <div className="flex flex-wrap items-center gap-3">
          <span
            role="status"
            className={cn(
              'inline-flex items-center gap-2 font-mono text-[0.78rem] font-bold tracking-[0.04em] uppercase',
              stream.status === 'live' && !paused ? 'text-destructive' : 'text-muted-foreground',
            )}
          >
            <span
              aria-hidden="true"
              className={cn(
                'size-2 rounded-full',
                stream.status === 'live' && !paused ? 'bg-destructive motion-safe:animate-pulse' : 'shadow-[inset_0_0_0_1.5px_currentColor]',
              )}
            />
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
        <TextFilter
          label="Watch (name or id)"
          value={watch}
          onChange={setWatch}
          help={invalidHelp(debouncedWatch, NAME_OR_ID_PATTERN, 'Use a lower case watch name or an upper case ULID id.')}
        />
        <SelectFilter
          label="Type"
          value={type}
          onChange={setType}
          options={[{ value: '', label: 'All types' }, ...TYPES.map((t) => ({ value: t, label: t }))]}
        />
        <TextFilter
          label="Source"
          value={source}
          onChange={setSource}
          placeholder="pypi"
          help={invalidHelp(debouncedSource, KIND_PATTERN, 'Use a source kind: lower case letters, digits and "-", e.g. pypi.')}
        />
      </FilterBar>

      {timeline.isPending ? (
        <TableSkeleton label="events" columns={5} />
      ) : timeline.isError ? (
        <ErrorState error={timeline.error} onRetry={() => void timeline.refetch()} />
      ) : events.length === 0 && !timeline.hasNextPage ? (
        <EmptyState title="No events yet">Events appear here as watches detect new releases.</EmptyState>
      ) : (
        <>
          <section aria-label="Tape" className="mb-6 grid gap-3">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <TapeLegend />
              <div role="group" aria-label="Tape range" className="inline-flex shadow-[inset_0_0_0_1.5px_var(--border)]">
                {RANGES.map((r) => (
                  <button
                    key={r.value}
                    type="button"
                    aria-pressed={range === r.value}
                    onClick={() => setRange(r.value)}
                    className={cn(
                      'h-8 px-3 text-sm font-semibold',
                      range === r.value ? 'bg-primary text-primary-foreground' : 'text-foreground hover:bg-muted',
                    )}
                  >
                    {r.label}
                  </button>
                ))}
              </div>
            </div>
            <EventTape events={events} range={range} now={now} selected={selected?.seq} onSelect={setPicked} />
            {selected ? <Bulletin record={selected} /> : <QuietTape newest={events[0]} rangeLabel={rangeInfo.label} />}
          </section>
          {events.length > 0 && (
            <Table aria-label="Events, newest first">
              <Head columns={['Seq', 'Type', 'Subject', 'Source', 'Time']} />
              <TableBody>
                {shown.map((r) => (
                  <EventRow key={r.seq} record={r} selected={r.seq === selected?.seq} onPick={() => setPicked(r.seq)} />
                ))}
              </TableBody>
            </Table>
          )}
          {events.length === 0 && (
            <p className="text-sm text-muted-foreground">No event matched among the newest events; older ones may match.</p>
          )}
          {(events.length > shown.length || timeline.hasNextPage) && (
            <div className="mt-4">
              <Button
                variant="outline"
                disabled={timeline.isFetchingNextPage}
                onClick={() => {
                  // Draw the rows already loaded first; fetch an older page only when they are all shown.
                  if (events.length <= shown.length) void timeline.fetchNextPage()
                  setRows((n) => n + ROWS)
                }}
              >
                {timeline.isFetchingNextPage ? 'Loading…' : 'Show older events'}
              </Button>
            </div>
          )}
        </>
      )}
    </>
  )
}

function EventRow({ record, selected, onPick }: { record: EventRecord; selected: boolean; onPick: () => void }) {
  const e = record.event
  return (
    <TableRow className={cn(selected && 'bg-primary/15')}>
      <TableCell>
        <button type="button" onClick={onPick} aria-pressed={selected} aria-label={`Show event ${record.seq} on the tape`} className="font-mono text-xs underline decoration-dotted">
          {record.seq}
        </button>
      </TableCell>
      <TableCell>
        <Badge variant={e.type.includes('yanked') ? 'destructive' : 'secondary'}>{shortType(e.type)}</Badge>
      </TableCell>
      <TableCell className="font-medium">
        <Link to="/events/$id" params={{ id: e.id }} className="text-link underline">
          {e.subject}
        </Link>
      </TableCell>
      <TableCell className="font-mono text-xs">{e.data.source}</TableCell>
      <TableCell className="font-mono text-xs whitespace-nowrap">
        <RelativeTime value={e.time} />
      </TableCell>
    </TableRow>
  )
}

export function EventDetailPage() {
  const { id } = useParams({ from: '/events/$id' })
  const query = useQuery({
    queryKey: ['event', id],
    queryFn: () => fetchEvent(id),
  })
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
        className="overflow-auto border-t-[3px] border-primary bg-code p-4 font-mono text-xs leading-relaxed text-code-foreground focus-visible:outline-2 focus-visible:outline-ring"
      >
        {JSON.stringify(e, null, 2)}
      </pre>
    </>
  )
}
