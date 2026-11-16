import { useState } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  fetchCapped,
  fetchDelivery,
  fetchDeliveries,
  fetchSubscriptions,
  useCursorList,
} from '@/api/queries'
import type { DeliveryStatus } from '@/api/types'
import { DeliveryStatusBadge, RelativeTime } from '@/components/badges'
import { FilterBar, SelectFilter } from '@/components/filters'
import { EmptyState, ErrorState, Head, ListFrame, PageHeader, Skeleton } from '@/components/states'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'

const STATUS_OPTIONS = [
  { value: '', label: 'All statuses' },
  { value: 'pending', label: 'Pending' },
  { value: 'in_flight', label: 'In flight' },
  { value: 'delivered', label: 'Delivered' },
  { value: 'dead', label: 'Dead letters' },
]

/** subscription id -> { name, sink }, so a delivery can show its sink. Bounded to a few pages. */
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
  const [status, setStatus] = useState('')
  const statuses = (status ? [status] : []) as DeliveryStatus[]
  const list = useCursorList(['deliveries', status], fetchDeliveries(statuses))
  const subs = useSubscriptionIndex()

  return (
    <>
      <PageHeader title="Deliveries" />
      <FilterBar label="Filter deliveries">
        <SelectFilter label="Status" value={status} onChange={setStatus} options={STATUS_OPTIONS} />
      </FilterBar>
      <ListFrame
        label="deliveries"
        query={list}
        count={list.items.length}
        columns={7}
        empty={
          <EmptyState title={status === 'dead' ? 'No dead letters' : 'No deliveries'}>
            {status ? 'No delivery has this status.' : 'Deliveries appear when an event matches a subscription.'}
          </EmptyState>
        }
      >
        <Table aria-label="Deliveries">
          <Head columns={['Status', 'Sink', 'Subscription', 'Attempts', 'Last error', 'Next attempt', 'Delivery']} />
          <TableBody>
            {list.items.map((d) => (
              <TableRow key={d.id}>
                <TableCell>
                  <DeliveryStatusBadge status={d.status} />
                </TableCell>
                <TableCell>{subs.data?.get(d.subscription_id)?.sink ?? '—'}</TableCell>
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
  const subs = useSubscriptionIndex()
  if (query.isPending) return <Skeleton className="h-40 w-full" />
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  const d = query.data
  const sub = subs.data?.get(d.subscription_id)
  const rows: [string, React.ReactNode][] = [
    ['Status', <DeliveryStatusBadge key="s" status={d.status} />],
    ['Sink', sub?.sink ?? '—'],
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
      <PageHeader title={`Delivery ${d.id}`} />
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
