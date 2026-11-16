import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { hasRole, useMe } from '@/api/me'
import {
  deleteSink,
  deleteSubscription,
  fetchSinkVersioned,
  fetchSubscriptionVersioned,
  invalidateSinks,
  invalidateSubscriptions,
} from '@/api/mutations'
import type { Sink, Subscription } from '@/api/types'
import { ConfirmDelete } from '@/components/form'
import { SinkForm } from '@/components/sink-form'
import { SubscriptionForm } from '@/components/subscription-form'
import { VersionedEdit } from '@/components/versioned'
import { Button } from '@/components/ui/button'
import { fetchConsumers, fetchPlugins, fetchSinks, fetchSubscriptions, useCursorList } from '@/api/queries'
import { LabelList, ManagedByBadge, RelativeTime } from '@/components/badges'
import { EmptyState, Head, ListFrame, PageHeader } from '@/components/states'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'

function csv(values: readonly string[]) {
  return values.length ? values.join(', ') : 'any'
}

/** Edit and delete for an API-managed row; file-managed rows get nothing. */
function RowActions({
  managedBy,
  what,
  consequence,
  onEdit,
  remove,
}: {
  managedBy: 'api' | 'file'
  what: string
  consequence?: string
  onEdit: () => void
  remove: { mutate: () => void; isPending: boolean; error: unknown }
}) {
  if (managedBy === 'file') return <span className="text-xs text-muted-foreground">read-only</span>
  return (
    <div className="flex flex-col gap-2">
      <Button variant="outline" size="sm" onClick={onEdit}>
        Edit
        <span className="sr-only"> {what}</span>
      </Button>
      <ConfirmDelete what={what} consequence={consequence} busy={remove.isPending} error={remove.error} onConfirm={() => remove.mutate()} />
    </div>
  )
}

function SinkRowActions({ sink, onEdit }: { sink: Sink; onEdit: () => void }) {
  const client = useQueryClient()
  const remove = useMutation({ mutationFn: () => deleteSink(sink.name), onSuccess: () => invalidateSinks(client) })
  return (
    <RowActions
      managedBy={sink.managed_by}
      what={`sink ${sink.name}`}
      consequence="The API refuses while a subscription still uses it."
      onEdit={onEdit}
      remove={remove}
    />
  )
}

function SubscriptionRowActions({ subscription, onEdit }: { subscription: Subscription; onEdit: () => void }) {
  const client = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteSubscription(subscription.name),
    onSuccess: () => invalidateSubscriptions(client),
  })
  return (
    <RowActions
      managedBy={subscription.managed_by}
      what={`subscription ${subscription.name}`}
      consequence="Its pending deliveries become dead; delivered ones stay as history."
      onEdit={onEdit}
      remove={remove}
    />
  )
}

export function SinksPage() {
  const list = useCursorList(['sinks'], fetchSinks())
  const me = useMe()
  const isAdmin = me.data ? hasRole(me.data, 'admin') : false
  const [form, setForm] = useState<'new' | Sink | null>(null)
  return (
    <>
      <PageHeader title="Sinks">{isAdmin && !form && <Button onClick={() => setForm('new')}>New sink</Button>}</PageHeader>
      {isAdmin && form === 'new' && <SinkForm key="new" onDone={() => setForm(null)} onCancel={() => setForm(null)} />}
      {isAdmin && form && form !== 'new' && (
        // The edit starts from the sink as it is now, and its ETag guards the save.
        <VersionedEdit key={form.name} kind="sink" name={form.name} fetch={fetchSinkVersioned}>
          {(v, reload) => <SinkForm initial={v.data} etag={v.etag} onReload={reload} onDone={() => setForm(null)} onCancel={() => setForm(null)} />}
        </VersionedEdit>
      )}
      <ListFrame
        label="sinks"
        query={list}
        count={list.items.length}
        columns={isAdmin ? 6 : 5}
        empty={<EmptyState title="No sinks">Add a sink to deliver events somewhere.</EmptyState>}
      >
        <Table aria-label="Sinks">
          <Head columns={['Name', 'Kind', 'Managed by', 'URL', 'Masked fields', ...(isAdmin ? ['Actions'] : [])]} />
          <TableBody>
            {list.items.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="font-medium">{s.name}</TableCell>
                <TableCell>{s.kind}</TableCell>
                <TableCell>
                  <ManagedByBadge value={s.managed_by} />
                </TableCell>
                <TableCell>
                  {typeof s.config.url === 'string' ? s.config.url : '—'}
                  {s.url_redacted && <Badge variant="outline" className="ml-2">URL redacted</Badge>}
                </TableCell>
                <TableCell>{s.masked_fields.length ? s.masked_fields.join(', ') : '—'}</TableCell>
                {isAdmin && (
                  <TableCell>
                    <SinkRowActions sink={s} onEdit={() => setForm(s)} />
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

export function SubscriptionsPage() {
  const list = useCursorList(['subscriptions'], fetchSubscriptions())
  const me = useMe()
  const canWrite = me.data ? hasRole(me.data, 'editor') : false
  const [form, setForm] = useState<'new' | Subscription | null>(null)
  return (
    <>
      <PageHeader title="Subscriptions">{canWrite && !form && <Button onClick={() => setForm('new')}>New subscription</Button>}</PageHeader>
      {canWrite && form === 'new' && <SubscriptionForm key="new" onDone={() => setForm(null)} onCancel={() => setForm(null)} />}
      {canWrite && form && form !== 'new' && (
        <VersionedEdit key={form.name} kind="subscription" name={form.name} fetch={fetchSubscriptionVersioned}>
          {(v, reload) => (
            <SubscriptionForm initial={v.data} etag={v.etag} onReload={reload} onDone={() => setForm(null)} onCancel={() => setForm(null)} />
          )}
        </VersionedEdit>
      )}
      <ListFrame
        label="subscriptions"
        query={list}
        count={list.items.length}
        columns={canWrite ? 8 : 7}
        empty={<EmptyState title="No subscriptions">A subscription routes matching events to a sink.</EmptyState>}
      >
        <Table aria-label="Subscriptions">
          <Head columns={['Name', 'Sink', 'Managed by', 'Types', 'Sources', 'Labels', 'Cooldown', ...(canWrite ? ['Actions'] : [])]} />
          <TableBody>
            {list.items.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="font-medium">{s.name}</TableCell>
                <TableCell>{s.sink}</TableCell>
                <TableCell>
                  <ManagedByBadge value={s.managed_by} />
                </TableCell>
                <TableCell>{csv(s.filter.type)}</TableCell>
                <TableCell>{csv(s.filter.source)}</TableCell>
                <TableCell>
                  <LabelList labels={s.filter.labels} />
                </TableCell>
                <TableCell>{s.policy.cooldown || '—'}</TableCell>
                {canWrite && (
                  <TableCell>
                    <SubscriptionRowActions subscription={s} onEdit={() => setForm(s)} />
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

export function ConsumersPage() {
  const list = useCursorList(['consumers'], fetchConsumers())
  return (
    <>
      <PageHeader title="Consumers" />
      <ListFrame
        label="consumers"
        query={list}
        count={list.items.length}
        columns={5}
        empty={<EmptyState title="No consumers">Consumers keep a cursor over the event log for pull clients.</EmptyState>}
      >
        <Table aria-label="Consumers">
          <Head columns={['Name', 'Watches', 'Types', 'Sources', 'Acknowledged seq', 'Acknowledged']} />
          <TableBody>
            {list.items.map((c) => (
              <TableRow key={c.id}>
                <TableCell className="font-medium">{c.name}</TableCell>
                <TableCell>{csv(c.filter.watch)}</TableCell>
                <TableCell>{csv(c.filter.type)}</TableCell>
                <TableCell>{csv(c.filter.source)}</TableCell>
                <TableCell>{c.acked_seq}</TableCell>
                <TableCell>
                  <RelativeTime value={c.acked_at} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </>
  )
}

export function PluginsPage() {
  const list = useCursorList(['plugins'], fetchPlugins())
  return (
    <>
      <PageHeader title="Plugins" />
      <ListFrame
        label="plugins"
        query={list}
        count={list.items.length}
        columns={5}
        empty={<EmptyState title="No plugins installed">Built-in sources and sinks are not listed here.</EmptyState>}
      >
        <Table aria-label="Plugins">
          <Head columns={['Kind', 'Interface', 'Runtime', 'Version', 'ABI', 'Allowed hosts']} />
          <TableBody>
            {list.items.map((p) => (
              <TableRow key={`${p.interface}/${p.kind}`}>
                <TableCell className="font-medium">{p.kind}</TableCell>
                <TableCell>{p.interface}</TableCell>
                <TableCell>{p.runtime}</TableCell>
                <TableCell>{p.version ?? '—'}</TableCell>
                <TableCell>{p.abi ?? '—'}</TableCell>
                <TableCell>{p.allowed_hosts.length ? p.allowed_hosts.map((h) => h.host).join(', ') : '—'}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </>
  )
}
