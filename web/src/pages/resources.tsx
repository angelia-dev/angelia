import { fetchConsumers, fetchPlugins, fetchSinks, fetchSubscriptions, useCursorList } from '@/api/queries'
import { LabelList, ManagedByBadge, RelativeTime } from '@/components/badges'
import { EmptyState, Head, ListFrame, PageHeader } from '@/components/states'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'

function csv(values: readonly string[]) {
  return values.length ? values.join(', ') : 'any'
}

export function SinksPage() {
  const list = useCursorList(['sinks'], fetchSinks())
  return (
    <>
      <PageHeader title="Sinks" />
      <ListFrame
        label="sinks"
        query={list}
        count={list.items.length}
        columns={5}
        empty={<EmptyState title="No sinks">Add a sink to deliver events somewhere.</EmptyState>}
      >
        <Table aria-label="Sinks">
          <Head columns={['Name', 'Kind', 'Managed by', 'URL', 'Masked fields']} />
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
  return (
    <>
      <PageHeader title="Subscriptions" />
      <ListFrame
        label="subscriptions"
        query={list}
        count={list.items.length}
        columns={6}
        empty={<EmptyState title="No subscriptions">A subscription routes matching events to a sink.</EmptyState>}
      >
        <Table aria-label="Subscriptions">
          <Head columns={['Name', 'Sink', 'Managed by', 'Types', 'Sources', 'Labels', 'Cooldown']} />
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
