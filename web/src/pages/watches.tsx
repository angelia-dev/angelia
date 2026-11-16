import { useState } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { deleteWatch, fetchWatchVersioned, invalidateWatches } from '@/api/mutations'
import { hasRole, useMe } from '@/api/me'
import { fetchVersions, fetchWatch, fetchWatches, useCursorList } from '@/api/queries'
import type { Watch } from '@/api/types'
import { ErrorCount, LabelList, ManagedByBadge, RelativeTime } from '@/components/badges'
import { FilterBar, TextFilter } from '@/components/filters'
import { ConfirmDelete } from '@/components/form'
import { VersionedEdit } from '@/components/versioned'
import { WatchForm } from '@/components/watch-form'
import { Button } from '@/components/ui/button'
import { watchToYaml } from '@/lib/yaml'
import { EmptyState, ErrorState, Head, ListFrame, PageHeader, Skeleton } from '@/components/states'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableRow } from '@/components/ui/table'
import { useDebounced } from '@/hooks/use-debounced'
import { invalidHelp, KIND_PATTERN, LABEL_PATTERN, validOrUndefined } from '@/lib/filters'

export function coordinatesSummary(w: Pick<Watch, 'coordinates'>): string {
  const entries = Object.entries(w.coordinates)
  if (entries.length === 0) return '—'
  return entries.map(([k, v]) => `${k}: ${v}`).join(', ')
}

export function WatchesPage() {
  const [source, setSource] = useState('')
  const [label, setLabel] = useState('')
  const debouncedSource = useDebounced(source.trim())
  const debouncedLabel = useDebounced(label.trim())
  // Only values that match the spec's patterns are sent.
  const filters = {
    source: validOrUndefined(debouncedSource, KIND_PATTERN) ?? '',
    label: validOrUndefined(debouncedLabel, LABEL_PATTERN) ?? '',
  }
  const list = useCursorList(['watches', filters], fetchWatches(filters))
  const filtered = Boolean(filters.source || filters.label)
  const me = useMe()
  const canWrite = me.data ? hasRole(me.data, 'editor') : false
  const [creating, setCreating] = useState(false)

  return (
    <>
      <PageHeader title="Watches">
        {canWrite && !creating && <Button onClick={() => setCreating(true)}>New watch</Button>}
      </PageHeader>
      {canWrite && creating && <WatchForm onDone={() => setCreating(false)} onCancel={() => setCreating(false)} />}
      <FilterBar label="Filter watches">
        <TextFilter
          label="Source"
          value={source}
          onChange={setSource}
          placeholder="pypi"
          help={invalidHelp(debouncedSource, KIND_PATTERN, 'Use a source kind: lower case letters, digits and "-", e.g. pypi.')}
        />
        <TextFilter
          label="Label (key=value)"
          value={label}
          onChange={setLabel}
          placeholder="team=platform"
          help={invalidHelp(
            debouncedLabel,
            LABEL_PATTERN,
            'Use key=value; the key starts with a letter or digit and has up to 63 characters from A-Z a-z 0-9 . _ / -.',
          )}
        />
      </FilterBar>
      <ListFrame
        label="watches"
        query={list}
        count={list.items.length}
        columns={8}
        empty={
          <EmptyState title={filtered ? 'No watch matches these filters' : 'No watches yet'}>
            {filtered ? 'Clear a filter to see more.' : 'Watches appear here once they are added through the config file or the API.'}
          </EmptyState>
        }
      >
        <Table aria-label="Watches">
          <Head columns={['Name', 'Source', 'Coordinates', 'Labels', 'Managed by', 'Last check', 'Next check', 'Errors']} />
          <TableBody>
            {list.items.map((w) => (
              <TableRow key={w.id}>
                <TableHead scope="row" className="font-medium">
                  <Link to="/watches/$name" params={{ name: w.name }} className="underline">
                    {w.name}
                  </Link>
                </TableHead>
                <TableCell>{w.source}</TableCell>
                <TableCell className="max-w-64 truncate" title={coordinatesSummary(w)}>
                  {coordinatesSummary(w)}
                </TableCell>
                <TableCell>
                  <LabelList labels={w.labels} />
                </TableCell>
                <TableCell>
                  <ManagedByBadge value={w.managed_by} />
                </TableCell>
                <TableCell>
                  <RelativeTime value={w.status.last_checked_at} />
                </TableCell>
                <TableCell>
                  <RelativeTime value={w.status.next_check_at} />
                </TableCell>
                <TableCell>
                  <ErrorCount count={w.status.error_count} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </>
  )
}

function Fact({ term, children }: { term: string; children: React.ReactNode }) {
  return (
    <TableRow>
      <TableHead scope="row" className="w-48 font-medium text-muted-foreground">
        {term}
      </TableHead>
      <TableCell>{children}</TableCell>
    </TableRow>
  )
}

function list(values: string[]) {
  return values.length ? values.join(', ') : '—'
}

export function WatchDetailPage() {
  const { name } = useParams({ from: '/watches/$name' })
  const watch = useQuery({
    queryKey: ['watch', name],
    queryFn: () => fetchWatch(name),
  })
  const versions = useCursorList(['watch-versions', name], fetchVersions(name))
  const me = useMe()
  const canWrite = me.data ? hasRole(me.data, 'editor') : false
  const [editing, setEditing] = useState(false)
  const [showYaml, setShowYaml] = useState(false)
  const client = useQueryClient()
  const navigate = useNavigate()
  const remove = useMutation({
    mutationFn: () => deleteWatch(name),
    onSuccess: async () => {
      // Leave the page and drop the deleted watch first; refetching it while it is shown would flash a 404.
      await navigate({ to: '/watches' })
      client.removeQueries({ queryKey: ['watch', name] })
      client.removeQueries({ queryKey: ['watch-versions', name] })
      await invalidateWatches(client)
    },
  })

  if (watch.isPending) return <Skeleton className="h-40 w-full" />
  if (watch.isError) return <ErrorState error={watch.error} onRetry={() => void watch.refetch()} />
  const w = watch.data
  const p = w.policy

  return (
    <>
      <PageHeader title={w.name}>
        <ManagedByBadge value={w.managed_by} />
      </PageHeader>
      {w.managed_by === 'file' && (
        <p className="mb-4 text-sm text-muted-foreground">
          This watch is managed by the watch file, so it is read-only here. Change the file and apply it instead.
        </p>
      )}
      {w.managed_by === 'api' && (
        <div className="mb-4 flex flex-wrap items-start gap-2">
          {canWrite && !editing && <Button onClick={() => setEditing(true)}>Edit</Button>}
          <Button variant="outline" aria-pressed={showYaml} onClick={() => setShowYaml((v) => !v)}>
            {showYaml ? 'Hide YAML' : 'Show as YAML'}
          </Button>
          {canWrite && (
            <ConfirmDelete
              what={`watch ${w.name}`}
              consequence="Its events and deliveries stay as history until retention prunes them."
              busy={remove.isPending}
              error={remove.error}
              onConfirm={() => remove.mutate()}
            />
          )}
        </div>
      )}
      {showYaml && w.managed_by === 'api' && <YamlView yaml={watchToYaml(w)} />}
      {editing && w.managed_by === 'api' && canWrite && (
        <VersionedEdit kind="watch" name={w.name} fetch={fetchWatchVersioned}>
          {(v, reload) => (
            <WatchForm initial={v.data} etag={v.etag} onReload={reload} onDone={() => setEditing(false)} onCancel={() => setEditing(false)} />
          )}
        </VersionedEdit>
      )}
      <p className="mb-4 text-sm">
        <Link to="/watches" className="underline">
          All watches
        </Link>
      </p>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Status</CardTitle>
          </CardHeader>
          <CardContent>
            <Table aria-label="Watch status">
              <TableBody>
                <Fact term="Source">{w.source}</Fact>
                <Fact term="Coordinates">{coordinatesSummary(w)}</Fact>
                <Fact term="Labels">
                  <LabelList labels={w.labels} />
                </Fact>
                <Fact term="Interval">{w.interval}</Fact>
                <Fact term="Effective interval">{w.status.effective_interval}</Fact>
                <Fact term="Version scheme">{w.scheme}</Fact>
                <Fact term="Last check">
                  <RelativeTime value={w.status.last_checked_at} />
                </Fact>
                <Fact term="Next check">
                  <RelativeTime value={w.status.next_check_at} />
                </Fact>
                <Fact term="Baseline">{w.status.baseline_done ? 'done' : 'not yet taken'}</Fact>
                <Fact term="Errors">
                  <ErrorCount count={w.status.error_count} />
                </Fact>
              </TableBody>
            </Table>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Policy</CardTitle>
          </CardHeader>
          <CardContent>
            <Table aria-label="Watch policy">
              <TableBody>
                <Fact term="Pre-releases">{p.prerelease}</Fact>
                <Fact term="Include">{list(p.include)}</Fact>
                <Fact term="Exclude">{list(p.exclude)}</Fact>
                <Fact term="Constraint">{p.constraint || '—'}</Fact>
                <Fact term="Bump">{list(p.bump)}</Fact>
                <Fact term="Cooldown">{p.cooldown || '—'}</Fact>
                <Fact term="Baseline on create">{p.baseline ? 'yes' : 'no'}</Fact>
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      </div>
      <h2 className="mt-6 mb-2 text-lg font-semibold">Versions</h2>
      <ListFrame
        label="versions"
        query={versions}
        count={versions.items.length}
        columns={5}
        empty={<EmptyState title="No versions seen yet">The first check has not found any version.</EmptyState>}
      >
        <Table aria-label="Versions">
          <Head columns={['Version', 'Normalized', 'State', 'Published', 'First seen']} />
          <TableBody>
            {versions.items.map((v) => (
              <TableRow key={v.version}>
                <TableHead scope="row" className="font-medium">
                  {v.version}
                </TableHead>
                <TableCell>{v.normalized_version}</TableCell>
                <TableCell>
                  {[v.prerelease && 'pre-release', v.yanked && 'yanked', v.removed_at && 'removed'].filter(Boolean).join(', ') || 'release'}
                </TableCell>
                <TableCell>
                  <RelativeTime value={v.published_at} />
                </TableCell>
                <TableCell>
                  <RelativeTime value={v.first_seen_at} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </>
  )
}

function YamlView({ yaml }: { yaml: string }) {
  const [copied, setCopied] = useState(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(yaml)
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }
  return (
    <Card className="mb-4">
      <CardHeader>
        <CardTitle>Watch file entry</CardTitle>
      </CardHeader>
      <CardContent>
        <p className="mb-2 text-sm text-muted-foreground">
          Add this to the file you pass to <code>--watches</code> to manage the watch from Git. Remove the API watch first, or the
          names clash (<code>409 managed-by-file</code>).
        </p>
        <pre aria-label="Watch file YAML" className="overflow-x-auto rounded-md bg-muted p-3 text-xs">
          {yaml}
        </pre>
        <Button variant="outline" size="sm" className="mt-2" onClick={() => void copy()}>
          {copied ? 'Copied' : 'Copy'}
        </Button>
      </CardContent>
    </Card>
  )
}
