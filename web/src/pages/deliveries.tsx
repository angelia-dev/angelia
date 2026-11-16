import { useMemo, useState } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  fetchCapped,
  fetchDelivery,
  fetchDeliveries,
  fetchSubscriptions,
  useCursorList,
  type DeliveryFilters,
} from '@/api/queries'
import { useAllSinks } from '@/api/secrets'
import { hasRole, useMe } from '@/api/me'
import type { Delivery, DeliveryStatus } from '@/api/types'
import { DeliveryStatusBadge, RelativeTime } from '@/components/badges'
import { DateTimeFilter, FilterBar, SelectFilter } from '@/components/filters'
import { isReplayable, ReplayDeliveryDialog, ReplayRangeDialog } from '@/components/replay'
import { Button } from '@/components/ui/button'
import { EmptyState, ErrorState, Head, ListFrame, PageHeader, Skeleton } from '@/components/states'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'

const STATUS_OPTIONS = [
  { value: '', label: 'All statuses' },
  { value: 'pending', label: 'Pending' },
  { value: 'in_flight', label: 'In flight' },
  { value: 'delivered', label: 'Delivered' },
  { value: 'dead', label: 'Dead letters' },
]

/**
 * A `datetime-local` pair as the API's RFC 3339 bounds, read in the browser's time zone. An
 * `until` not after `since` is refused here (the API would answer 400).
 */
export function timeRange(since: string, until: string): { since?: string; until?: string; error?: string } {
  const toIso = (v: string) => {
    if (!v) return undefined
    const t = new Date(v)
    return Number.isNaN(t.getTime()) ? undefined : t.toISOString()
  }
  const s = toIso(since)
  const u = toIso(until)
  if (s && u && Date.parse(u) <= Date.parse(s)) return { since: s, error: 'Must be after the start.' }
  return { since: s, until: u }
}

/** subscription id -> subscription, for the subscription filter. Bounded to a few pages. */
export function useSubscriptionIndex() {
  return useQuery({
    queryKey: ['subscription-index'],
    queryFn: async () => {
      const { items } = await fetchCapped(fetchSubscriptions(), 5)
      return new Map(items.map((s) => [s.id, s]))
    },
    staleTime: 60_000,
  })
}

export function DeliveriesPage() {
  const [status, setStatus] = useState('dead')
  const [subscription, setSubscription] = useState('')
  const [sinkName, setSinkName] = useState('')
  const [since, setSince] = useState('')
  const [until, setUntil] = useState('')
  const [replaying, setReplaying] = useState<Delivery>()
  const [replayRange, setReplayRange] = useState(false)
  const statuses = (status ? [status] : []) as DeliveryStatus[]
  const range = timeRange(since, until)
  const filters: DeliveryFilters = { status: statuses, subscription, sink: sinkName, since: range.since, until: range.until }
  const list = useCursorList(['deliveries', status, subscription, sinkName, range.since, range.until], fetchDeliveries(filters))
  const subs = useSubscriptionIndex()
  const sinks = useAllSinks()
  const me = useMe()
  const canWrite = !!me.data && hasRole(me.data, 'editor')
  const subList = useMemo(() => [...(subs.data?.values() ?? [])], [subs.data])
  const sinkNames = useMemo(() => [...new Set((sinks.data ?? []).map((s) => s.name))].sort(), [sinks.data])
  const items = list.items

  return (
    <>
      <PageHeader title="Deliveries">
        {canWrite && (
          <Button variant="outline" onClick={() => setReplayRange(true)}>
            Replay range
          </Button>
        )}
      </PageHeader>
      <FilterBar label="Filter deliveries">
        <SelectFilter label="Status" value={status} onChange={setStatus} options={STATUS_OPTIONS} />
        <SelectFilter
          label="Subscription"
          value={subscription}
          onChange={setSubscription}
          options={[{ value: '', label: 'All subscriptions' }, ...subList.map((s) => ({ value: s.id, label: s.name }))]}
        />
        <SelectFilter
          label="Sink"
          value={sinkName}
          onChange={setSinkName}
          options={[{ value: '', label: 'All sinks' }, ...[...new Set([...sinkNames, ...(sinkName ? [sinkName] : [])])].map((n) => ({ value: n, label: n }))]}
        />
        <DateTimeFilter label="Event recorded from" value={since} onChange={setSince} />
        <DateTimeFilter label="Event recorded before" value={until} onChange={setUntil} help={range.error} />
      </FilterBar>
      {replaying && <ReplayDeliveryDialog delivery={replaying} onClose={() => setReplaying(undefined)} />}
      {replayRange && <ReplayRangeDialog onClose={() => setReplayRange(false)} />}
      <ListFrame
        label="deliveries"
        query={list}
        count={items.length}
        columns={canWrite ? 8 : 7}
        empty={
          <EmptyState title={status === 'dead' ? 'No dead letters' : 'No deliveries'}>
            {status ? 'No delivery has this status.' : 'Deliveries appear when an event matches a subscription.'}
          </EmptyState>
        }
      >
        <Table aria-label="Deliveries">
          <Head columns={['Status', 'Sink', 'Subscription', 'Attempts', 'Last error', 'Next attempt', 'Delivery', ...(canWrite ? ['Actions'] : [])]} />
          <TableBody>
            {items.map((d) => (
              <TableRow key={d.id}>
                <TableCell>
                  <DeliveryStatusBadge status={d.status} />
                </TableCell>
                <TableCell>{d.sink ?? '—'}</TableCell>
                <TableCell>{d.subscription_name ?? '(deleted)'}</TableCell>
                <TableCell>{d.attempts}</TableCell>
                <TableCell className="max-w-64 truncate" title={d.last_error ?? undefined}>
                  {d.last_error ?? '—'}
                </TableCell>
                <TableCell>
                  <RelativeTime value={d.next_attempt_at} />
                </TableCell>
                <TableCell>
                  <Link to="/deliveries/$id" params={{ id: d.id }} className="underline">
                    {d.id.slice(-8)}
                  </Link>
                </TableCell>
                {canWrite && (
                  <TableCell>
                    {isReplayable(d) && (
                      <Button variant="outline" size="sm" aria-label={`Replay delivery ${d.id}`} onClick={() => setReplaying(d)}>
                        Replay
                      </Button>
                    )}
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </>
  )
}

export function DeliveryDetailPage() {
  const { id } = useParams({ from: '/deliveries/$id' })
  const query = useQuery({ queryKey: ['delivery', id], queryFn: () => fetchDelivery(id) })
  const me = useMe()
  const [replaying, setReplaying] = useState(false)
  if (query.isPending) return <Skeleton className="h-40 w-full" />
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  const d = query.data
  const canWrite = !!me.data && hasRole(me.data, 'editor')
  const rows: [string, React.ReactNode][] = [
    ['Status', <DeliveryStatusBadge key="s" status={d.status} />],
    ['Sink', d.sink ?? '—'],
    ['Subscription', d.subscription_name ?? '(deleted)'],
    ['Attempts', d.attempts],
    ['Next attempt', <RelativeTime key="n" value={d.next_attempt_at} />],
    ['Held until', <RelativeTime key="h" value={d.held_until} />],
    ['Last error', d.last_error ?? '—'],
    ['Idempotency key', <code key="i">{d.idempotency_key}</code>],
    [
      'Event',
      <Link key="e" to="/events/$id" params={{ id: d.event_id }} className="underline">
        {d.event_id}
      </Link>,
    ],
  ]
  return (
    <>
      <PageHeader title={`Delivery ${d.id}`}>
        {canWrite && isReplayable(d) && <Button onClick={() => setReplaying(true)}>Replay</Button>}
      </PageHeader>
      {replaying && <ReplayDeliveryDialog delivery={d} onClose={() => setReplaying(false)} />}
      <p className="mb-4 text-sm">
        <Link to="/deliveries" className="underline">
          All deliveries
        </Link>
      </p>
      <Table aria-label="Delivery details">
        <TableBody>
          {rows.map(([term, value]) => (
            <TableRow key={term}>
              <th scope="row" className="w-48 p-3 text-left font-medium text-muted-foreground">
                {term}
              </th>
              <TableCell className="break-all">{value}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </>
  )
}
