import { useCallback, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { adoptResources, type AdoptionObject } from '@/api/adoptions'
import { hasRole, useMe } from '@/api/me'
import {
  deleteSink,
  deleteSubscription,
  deleteWatch,
  fetchSinkVersioned,
  fetchSubscriptionVersioned,
  fetchWatchVersioned,
  invalidateRoots,
} from '@/api/mutations'
import { fetchCapped, fetchSubscriptions, fetchWatches } from '@/api/queries'
import { useAllSinks } from '@/api/secrets'
import type { Sink, Subscription, Watch } from '@/api/types'
import { Dialog } from '@/components/dialog'
import { ConfirmDelete, ProblemSummary } from '@/components/form'
import { PatchBay, type BoardItem } from '@/components/patch-bay'
import { SinkForm } from '@/components/sink-form'
import { EmptyState, ErrorState, PageHeader, Skeleton } from '@/components/states'
import { SubscriptionForm } from '@/components/subscription-form'
import { Button } from '@/components/ui/button'
import { Stamp } from '@/components/ui/badge'
import { VersionedEdit } from '@/components/versioned'
import { WatchForm } from '@/components/watch-form'
import { configToYaml, routeName, watchPackage } from '@/lib/routes'
import type { SubscriptionFormState } from '@/lib/subscription-form'
import { cn } from '@/lib/utils'

/** Every query-key root the Configure page reads; a write refetches them all. */
const ROOTS = ['watches', 'watch', 'overview', 'sinks', 'sink-index', 'secrets', 'subscriptions', 'subscription-index', 'configure']
const refreshAll = (client: ReturnType<typeof useQueryClient>) => invalidateRoots(client, ROOTS)

function useConfig() {
  const watches = useQuery({ queryKey: ['watches', 'configure'], queryFn: async () => (await fetchCapped(fetchWatches(), 10)).items })
  const sinks = useAllSinks()
  const subscriptions = useQuery({
    queryKey: ['subscriptions', 'configure'],
    queryFn: async () => (await fetchCapped(fetchSubscriptions(), 10)).items,
  })
  return { watches, sinks, subscriptions }
}

type Selection =
  | BoardItem
  | { kind: 'new-watch' }
  | { kind: 'new-sink' }
  | { kind: 'new-subscription'; prefill: Partial<SubscriptionFormState> }

/** Saves a text as a file in the browser. */
function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/yaml' }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

const today = () => new Date().toISOString().slice(0, 10)

function CodePanel({ title, text }: { title: string; text: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="border-t-[3px] border-primary bg-code text-code-foreground">
      <div className="flex items-center gap-2 border-b border-white/10 px-3 py-1.5">
        <span className="wire-label text-chrome-muted">{title}</span>
        <button
          type="button"
          className="ml-auto font-mono text-xs text-canary hover:underline"
          onClick={() => void navigator.clipboard?.writeText(text).then(() => setCopied(true))}
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre className="max-h-[60vh] overflow-auto p-3 font-mono text-[0.78rem] leading-relaxed">{text}</pre>
    </div>
  )
}

/** The one-time hand-over of watch-file objects to the UI (POST /v1/adoptions). */
function MoveDialog({
  objects,
  backupYaml,
  onClose,
}: {
  objects: AdoptionObject[]
  backupYaml: string
  onClose: () => void
}) {
  const client = useQueryClient()
  const [backup, setBackup] = useState(true)
  const move = useMutation({
    mutationFn: () => {
      if (backup) download(`angelia-watch-file-objects-${today()}.yaml`, backupYaml)
      return adoptResources(objects)
    },
    onSuccess: () => void refreshAll(client),
  })
  const done = move.isSuccess
  return (
    <Dialog title={done ? 'Moved to the UI' : 'Manage these objects from the UI'} onClose={onClose} dismissible={!move.isPending}>
      {done ? (
        <div className="flex flex-col gap-3 text-sm">
          <p>
            {move.data.adopted.length} objects are now managed here. Their ids, known versions, held and pending deliveries are
            unchanged.
          </p>
          <div className="paper p-3">
            <p className="wire-label mb-1">Before the next restart</p>
            <p>
              Remove them from the watch file, or drop <code className="font-mono">--watches</code> from the start command. Until
              then Angelia starts normally, skips them in the file and logs a warning.
            </p>
          </div>
          <div className="flex justify-end">
            <Button onClick={onClose}>Done</Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-3 text-sm">
          <p className="text-muted-foreground">
            Each object is handed over in place: ids, known versions, cooldown holds and pending deliveries stay as they are, so no
            release is missed or sent twice.
          </p>
          <ul className="max-h-56 overflow-auto border-y border-border py-1" aria-label="Objects to move">
            {objects.map((o) => (
              <li key={`${o.kind}/${o.name}`} className="flex gap-3 py-1 font-mono text-xs">
                <span className="w-24 text-muted-foreground">{o.kind}</span>
                <span className="font-bold">{o.name}</span>
              </li>
            ))}
          </ul>
          <label className="flex items-center gap-2">
            <input type="checkbox" checked={backup} onChange={(e) => setBackup(e.target.checked)} />
            Download them as YAML first
          </label>
          {move.isError && <ProblemSummary error={move.error} />}
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={onClose} disabled={move.isPending}>
              Cancel
            </Button>
            <Button onClick={() => move.mutate()} disabled={move.isPending}>
              {move.isPending ? 'Moving…' : `Move ${objects.length} objects`}
            </Button>
          </div>
        </div>
      )}
    </Dialog>
  )
}

/** Delete for an object managed by the API. */
function DeleteObject({ item, onDone }: { item: BoardItem; onDone: () => void }) {
  const client = useQueryClient()
  const remove = useMutation({
    mutationFn: () =>
      item.kind === 'watch' ? deleteWatch(item.name) : item.kind === 'sink' ? deleteSink(item.name) : deleteSubscription(item.name),
    onSuccess: async () => {
      await refreshAll(client)
      onDone()
    },
  })
  const consequence =
    item.kind === 'subscription'
      ? 'Its pending deliveries become dead; delivered ones stay as history.'
      : item.kind === 'sink'
        ? 'The API refuses while a route still uses it.'
        : 'Events already recorded stay as history.'
  return <ConfirmDelete what={`${item.kind} ${item.name}`} consequence={consequence} busy={remove.isPending} error={remove.error} onConfirm={() => remove.mutate()} />
}

function Drawer({
  selection,
  watches,
  sinks,
  subscriptions,
  role,
  onClose,
}: {
  selection: Selection
  watches: Watch[]
  sinks: Sink[]
  subscriptions: Subscription[]
  role: { editor: boolean; admin: boolean }
  onClose: () => void
}) {
  const [tab, setTab] = useState<'form' | 'yaml'>('form')
  const client = useQueryClient()
  const done = () => {
    void refreshAll(client)
    onClose()
  }

  let heading: string
  let kindLabel: string
  let body: React.ReactNode
  let yaml: string | null = null
  let item: BoardItem | null = null
  let owner: 'api' | 'file' | null = null

  switch (selection.kind) {
    case 'new-watch':
      kindLabel = 'Watch'
      heading = 'New watch'
      body = <WatchForm onDone={done} onCancel={onClose} />
      break
    case 'new-sink':
      kindLabel = 'Sink'
      heading = 'New sink'
      body = <SinkForm onDone={done} onCancel={onClose} />
      break
    case 'new-subscription':
      kindLabel = 'Route'
      heading = 'New route'
      body = <SubscriptionForm prefill={selection.prefill} onDone={done} onCancel={onClose} />
      break
    default: {
      item = selection
      kindLabel = selection.kind === 'subscription' ? 'Route (subscription)' : selection.kind === 'watch' ? 'Watch' : 'Sink'
      heading = selection.name
      if (selection.kind === 'watch') {
        const w = watches.find((x) => x.name === selection.name)
        owner = w?.managed_by ?? null
        yaml = w ? configToYaml({ watches: [w] }) : null
      } else if (selection.kind === 'sink') {
        const k = sinks.find((x) => x.name === selection.name)
        owner = k?.managed_by ?? null
        yaml = k ? configToYaml({ sinks: [k] }) : null
      } else {
        const s = subscriptions.find((x) => x.name === selection.name)
        owner = s?.managed_by ?? null
        yaml = s ? configToYaml({ subscriptions: [s] }) : null
      }
      const canEdit = owner === 'api' && (selection.kind === 'sink' ? role.admin : role.editor)
      if (!canEdit) {
        body = (
          <p className="text-sm text-muted-foreground">
            {owner === 'file'
              ? 'This object comes from the watch file, so it is read-only here. Move the watch-file objects to the UI to edit it, or change the file and restart.'
              : selection.kind === 'sink'
                ? 'Sinks are managed by admins.'
                : 'Your role can read this object, not change it.'}
          </p>
        )
      } else if (selection.kind === 'watch') {
        body = (
          <VersionedEdit key={selection.name} kind="watch" name={selection.name} fetch={fetchWatchVersioned}>
            {(v, reload) => <WatchForm initial={v.data} etag={v.etag} onReload={reload} onDone={done} onCancel={onClose} />}
          </VersionedEdit>
        )
      } else if (selection.kind === 'sink') {
        body = (
          <VersionedEdit key={selection.name} kind="sink" name={selection.name} fetch={fetchSinkVersioned}>
            {(v, reload) => <SinkForm initial={v.data} etag={v.etag} onReload={reload} onDone={done} onCancel={onClose} />}
          </VersionedEdit>
        )
      } else {
        body = (
          <VersionedEdit key={selection.name} kind="subscription" name={selection.name} fetch={fetchSubscriptionVersioned}>
            {(v, reload) => <SubscriptionForm initial={v.data} etag={v.etag} onReload={reload} onDone={done} onCancel={onClose} />}
          </VersionedEdit>
        )
      }
      if (canEdit) {
        body = (
          <>
            {body}
            <div className="mt-2">
              <DeleteObject item={selection} onDone={onClose} />
            </div>
          </>
        )
      }
    }
  }

  return (
    <aside
      aria-label={`Editor: ${heading}`}
      className="flex w-full flex-col border-t-[3px] border-chrome bg-card lg:w-[460px] lg:flex-none lg:border-t-0 lg:border-l-[3px] dark:border-canary"
    >
      <div className="flex flex-col gap-2 border-b border-border px-5 pt-4">
        <div className="flex items-center gap-2">
          <span className="wire-label text-muted-foreground">{kindLabel}</span>
          {owner && <Stamp tone={owner === 'file' ? 'held' : 'ok'}>{owner === 'file' ? 'watch file' : 'managed by ui'}</Stamp>}
          <Button variant="ghost" size="sm" className="ml-auto" aria-label="Close the editor" onClick={onClose}>
            ✕
          </Button>
        </div>
        <h2 className="wire-title truncate text-[1.6rem]">{heading}</h2>
        {item && (
          <div role="tablist" aria-label="Editor view" className="flex gap-0">
            {(['form', 'yaml'] as const).map((t) => (
              <button
                key={t}
                type="button"
                role="tab"
                aria-selected={tab === t}
                onClick={() => setTab(t)}
                className={cn('wire-label px-4 py-2', tab === t ? 'bg-chrome text-canary' : 'text-foreground hover:text-gold-ink')}
              >
                {t === 'form' ? 'Form' : 'YAML'}
              </button>
            ))}
          </div>
        )}
      </div>
      <div className="flex-1 overflow-auto p-4 [&_.mb-6]:mb-0">
        {item && tab === 'yaml' ? (
          yaml ? (
            <div className="flex flex-col gap-2">
              <CodePanel title="watch-file entry" text={yaml} />
              <p className="text-xs text-muted-foreground">The same format as the watch file and `angelia apply`. Secret values are never shown: sinks name a stored secret or an environment variable.</p>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">Not found.</p>
          )
        ) : (
          body
        )}
      </div>
    </aside>
  )
}

export function ConfigurePage() {
  const me = useMe()
  const role = {
    editor: !!me.data && hasRole(me.data, 'editor'),
    admin: !!me.data && hasRole(me.data, 'admin'),
  }
  const { watches, sinks, subscriptions } = useConfig()
  const [selection, setSelection] = useState<Selection | null>(null)
  const [view, setView] = useState<'board' | 'yaml'>('board')
  const [moving, setMoving] = useState(false)

  const ws = useMemo(() => watches.data ?? [], [watches.data])
  const ks = useMemo(() => sinks.data ?? [], [sinks.data])
  const ss = useMemo(() => subscriptions.data ?? [], [subscriptions.data])

  const fileObjects: AdoptionObject[] = useMemo(
    () => [
      ...ks.filter((x) => x.managed_by === 'file').map((x) => ({ kind: 'sink' as const, name: x.name })),
      ...ws.filter((x) => x.managed_by === 'file').map((x) => ({ kind: 'watch' as const, name: x.name })),
      ...ss.filter((x) => x.managed_by === 'file').map((x) => ({ kind: 'subscription' as const, name: x.name })),
    ],
    [ws, ks, ss],
  )

  const onRoute = useCallback(
    (watch: string, sink?: string) => {
      const w = ws.find((x) => x.name === watch)
      if (!w) return
      const taken = new Set(ss.map((s) => s.name))
      setSelection({
        kind: 'new-subscription',
        prefill: { name: routeName(watch, sink ?? 'sink', taken), sink: sink ?? '', sources: [w.source], package: watchPackage(w) },
      })
    },
    [ws, ss],
  )

  const loading = watches.isPending || sinks.isPending || subscriptions.isPending
  const failed = watches.error ?? sinks.error ?? subscriptions.error
  const allYaml = configToYaml({ sinks: ks, watches: ws, subscriptions: ss })
  const selected = selection && (selection.kind === 'watch' || selection.kind === 'sink' || selection.kind === 'subscription') ? selection : null

  return (
    <>
      <PageHeader title="Configure">
        <div className="flex flex-wrap items-center gap-2">
          <div role="group" aria-label="View" className="flex overflow-hidden rounded-[2px] shadow-[inset_0_0_0_1.5px_var(--border)]">
            {(['board', 'yaml'] as const).map((v) => (
              <button
                key={v}
                type="button"
                aria-pressed={view === v}
                onClick={() => setView(v)}
                className={cn('wire-label h-9 px-4', view === v ? 'bg-primary text-primary-foreground' : 'text-foreground hover:text-gold-ink')}
              >
                {v === 'board' ? 'Board' : 'YAML'}
              </button>
            ))}
          </div>
          {role.editor && (
            <Button variant="outline" onClick={() => setSelection({ kind: 'new-watch' })}>
              + Watch
            </Button>
          )}
          {role.editor && (
            <Button variant="outline" onClick={() => setSelection({ kind: 'new-subscription', prefill: {} })}>
              + Route
            </Button>
          )}
          {role.admin && (
            <Button variant="outline" onClick={() => setSelection({ kind: 'new-sink' })}>
              + Sink
            </Button>
          )}
          <Button variant="outline" onClick={() => download(`angelia-config-${today()}.yaml`, allYaml)} disabled={loading}>
            Export YAML
          </Button>
        </div>
      </PageHeader>
      <p className="-mt-3 mb-5 max-w-3xl text-sm text-muted-foreground">
        The patch bay: watches on the left, routes in the middle, sinks on the right. Changes are saved to Angelia&rsquo;s database
        and take effect at once, with no restart.
      </p>

      {fileObjects.length > 0 && (
        <div className="mb-5 flex flex-wrap items-center gap-3 border-l-[6px] border-primary bg-chrome px-4 py-3 text-chrome-foreground">
          <Stamp tone="bulletin" className="text-canary">
            Bulletin
          </Stamp>
          <span className="text-sm">
            {fileObjects.length} object{fileObjects.length === 1 ? '' : 's'} come from the watch file and are read-only here.
          </span>
          {role.admin ? (
            <Button className="ml-auto" onClick={() => setMoving(true)}>
              Move them to the UI…
            </Button>
          ) : (
            <span className="ml-auto text-xs text-chrome-muted">An admin can move them to the UI.</span>
          )}
        </div>
      )}

      {loading ? (
        <Skeleton className="h-96 w-full" />
      ) : failed ? (
        <ErrorState error={failed} />
      ) : (
        <div className="flex flex-col gap-5 lg:flex-row lg:items-start">
          <div className="min-w-0 flex-1">
            {view === 'yaml' ? (
              <div className="flex flex-col gap-2">
                <CodePanel title="angelia-config.yaml" text={allYaml} />
                <p className="text-xs text-muted-foreground">
                  The whole configuration as one watch file, ready for `angelia apply`. Editing it here comes in a later release; use
                  the board or the drawer for now.
                </p>
              </div>
            ) : ws.length + ks.length + ss.length === 0 ? (
              <EmptyState title="Nothing configured yet">Add a watch, a sink, then route the watch to the sink.</EmptyState>
            ) : (
              <PatchBay
                watches={ws}
                sinks={ks}
                subscriptions={ss}
                selected={selected}
                onSelect={setSelection}
                onRoute={role.editor ? onRoute : undefined}
                canRoute={role.editor}
              />
            )}
          </div>
          {selection && (
            <Drawer
              key={'name' in selection ? `${selection.kind}:${selection.name}` : selection.kind === 'new-subscription' ? JSON.stringify(selection.prefill) : selection.kind}
              selection={selection}
              watches={ws}
              sinks={ks}
              subscriptions={ss}
              role={role}
              onClose={() => setSelection(null)}
            />
          )}
        </div>
      )}

      {moving && (
        <MoveDialog
          objects={fileObjects}
          backupYaml={configToYaml({
            sinks: ks.filter((x) => x.managed_by === 'file'),
            watches: ws.filter((x) => x.managed_by === 'file'),
            subscriptions: ss.filter((x) => x.managed_by === 'file'),
          })}
          onClose={() => setMoving(false)}
        />
      )}
    </>
  )
}
