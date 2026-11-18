import { useMemo, useRef, useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { invalidateWatches, putWatch } from '@/api/mutations'
import { schemaFields, usePlugins } from '@/api/plugins'
import { isPreconditionFailed } from '@/api/problem'
import type { Watch } from '@/api/types'
import {
  Checkbox,
  PairsEditor,
  ProblemSummary,
  SelectInput,
  TextArea,
  TextInput,
  errorsFor,
  mergeErrors,
  newPair,
  problemFieldErrors,
  useFocusFirstError,
  type FieldErrors,
} from '@/components/form'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ChangedElsewhere } from '@/components/versioned'
import {
  BUMPS,
  PRERELEASE,
  SCHEMES,
  WATCH_FIELD_POINTERS,
  emptyWatchForm,
  formToWrite,
  validateWatchForm,
  watchToForm,
  type WatchFormState,
} from '@/lib/watch-form'

/**
 * Create (no `initial`) or edit an API-managed watch. `onDone` gets the saved name. A create
 * sends `If-None-Match: *`; an edit sends `If-Match: etag`, and `onReload` reads the watch again
 * after someone else changed it.
 */
export function WatchForm({
  initial,
  etag,
  onReload,
  onDone,
  onCancel,
}: {
  initial?: Watch
  etag?: string
  onReload?: () => void
  onDone: (name: string) => void
  onCancel: () => void
}) {
  const creating = !initial
  const client = useQueryClient()
  const [form, setForm] = useState<WatchFormState>(() => (initial ? watchToForm(initial) : emptyWatchForm()))
  const [clientErrors, setClientErrors] = useState<FieldErrors>({})
  const plugins = usePlugins('source')
  const set = <K extends keyof WatchFormState>(k: K, v: WatchFormState[K]) => setForm((f) => ({ ...f, [k]: v }))

  const kinds = plugins.data?.map((p) => p.kind) ?? []
  const sourceOptions = [...new Set([...kinds, ...(form.source ? [form.source] : [])])].sort()
  const fields = useMemo(
    () => schemaFields(plugins.data?.find((p) => p.kind === form.source)?.config_schema),
    [plugins.data, form.source],
  )
  const extraCoordinates = form.coordinates.filter((c) => c.key && !fields.some((f) => f.key === c.key))
  const requiredCoordinates = fields.filter((f) => f.required).map((f) => f.key)

  const save = useMutation({
    mutationFn: () =>
      creating ? putWatch(form.name, formToWrite(form), { create: true }) : putWatch(initial.name, formToWrite(form), { etag }),
    onSuccess: async (saved) => {
      await invalidateWatches(client)
      onDone(saved.name)
    },
  })

  const serverErrors = problemFieldErrors(save.error)
  const conflict = isPreconditionFailed(save.error)
  const taken: FieldErrors = conflict && creating ? { '/name': ['A watch with this name already exists. Open it to edit it.'] } : {}
  const errors = mergeErrors(clientErrors, serverErrors, taken)

  // After a refused submit, move focus to the first field that has an error (WCAG 3.3.1).
  const formRef = useRef<HTMLFormElement>(null)
  const focusError = useFocusFirstError(formRef, save.isError)

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    const found = validateWatchForm(form, { creating, requiredCoordinates })
    setClientErrors(found)
    if (Object.keys(found).length > 0) {
      focusError()
      return
    }
    save.mutate()
  }

  const coordValue = (key: string) => form.coordinates.find((c) => c.key === key)?.value ?? ''
  // Clearing a schema field removes the coordinate, so an optional one left empty is not sent as "".
  const setCoord = (key: string, value: string) =>
    set(
      'coordinates',
      value === ''
        ? form.coordinates.filter((c) => c.key !== key)
        : form.coordinates.some((c) => c.key === key)
          ? form.coordinates.map((c) => (c.key === key ? { ...c, value } : c))
          : [...form.coordinates, newPair(key, value)],
    )

  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>{creating ? 'New watch' : `Edit ${initial.name}`}</CardTitle>
      </CardHeader>
      <CardContent>
        <form ref={formRef} aria-label={creating ? 'New watch' : `Edit ${initial.name}`} noValidate onSubmit={onSubmit} className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">
            Saving replaces the whole watch. If someone else saved it since you opened this form, the save is refused and nothing
            is overwritten.
          </p>
          <div className="grid gap-4 md:grid-cols-2">
            <TextInput
              label="Name"
              required
              value={form.name}
              disabled={!creating}
              onChange={(v) => set('name', v)}
              errors={errorsFor(errors, '/name')}
              hint={creating ? 'The stable key; it cannot be changed later.' : undefined}
            />
            {plugins.isError ? (
              <TextInput label="Source" required value={form.source} onChange={(v) => set('source', v)} errors={errorsFor(errors, '/source')} hint="The list of sources could not be loaded; type the kind." />
            ) : (
              <SelectInput
                label="Source"
                required
                value={form.source}
                onChange={(v) => set('source', v)}
                errors={errorsFor(errors, '/source')}
                options={[{ value: '', label: plugins.isPending ? 'Loading…' : 'Choose a source' }, ...sourceOptions.map((k) => ({ value: k, label: k }))]}
              />
            )}
          </div>

          <fieldset className="flex flex-col gap-3">
            <legend className="text-sm font-medium">Coordinates</legend>
            {/* Named fields only when the source publishes a coordinate schema; otherwise free key/value rows. */}
            {fields.length > 0 ? (
              <div className="grid gap-4 md:grid-cols-2">
                {fields.map((f) => (
                  <TextInput
                    key={f.key}
                    label={f.key}
                    required={f.required}
                    value={coordValue(f.key)}
                    onChange={(v) => setCoord(f.key, v)}
                    hint={f.description}
                    errors={errorsFor(errors, `/coordinates/${f.key}`)}
                  />
                ))}
                {extraCoordinates.map((c) => (
                  <TextInput key={c.key} label={c.key} value={c.value} onChange={(v) => setCoord(c.key, v)} errors={errorsFor(errors, `/coordinates/${c.key}`)} />
                ))}
              </div>
            ) : (
              <PairsEditor
                legend="Coordinate"
                pairs={form.coordinates}
                onChange={(p) => set('coordinates', p)}
                keyLabel="Key"
                valueLabel="Value"
                hint={form.source ? 'This source publishes no coordinate schema; enter its keys by hand.' : 'Choose a source first.'}
                errors={errorsFor(errors, '/coordinates')}
              />
            )}
            {fields.length > 0 && errors['/coordinates'] && (
              <p role="alert" tabIndex={-1} className="text-xs text-destructive">
                {errors['/coordinates'].join('; ')}
              </p>
            )}
          </fieldset>

          <div className="grid gap-4 md:grid-cols-2">
            <TextInput
              label="Interval"
              value={form.interval}
              onChange={(v) => set('interval', v)}
              errors={errorsFor(errors, '/interval')}
              hint="5m to 720h, for example 1h. Watches on one target share the smallest interval."
            />
            <SelectInput
              label="Version scheme"
              value={form.scheme}
              onChange={(v) => set('scheme', v)}
              errors={errorsFor(errors, '/scheme')}
              options={[
                { value: '', label: 'Default by source' },
                ...[...new Set([...SCHEMES, ...(form.scheme ? [form.scheme] : [])])].map((s) => ({ value: s, label: s })),
              ]}
            />
          </div>

          <PairsEditor legend="Label" pairs={form.labels} onChange={(p) => set('labels', p)} keyLabel="Key" valueLabel="Value" errors={errorsFor(errors, '/labels')} hint="At most 32. Subscriptions filter on labels." />

          <fieldset className="flex flex-col gap-3 rounded-[2px] border border-border p-3">
            <legend className="px-1 text-sm font-medium">Policy</legend>
            <div className="grid gap-4 md:grid-cols-2">
              <SelectInput
                label="Pre-releases"
                value={form.prerelease}
                onChange={(v) => set('prerelease', v as WatchFormState['prerelease'])}
                errors={errorsFor(errors, '/policy/prerelease')}
                options={PRERELEASE.map((p) => ({ value: p, label: p }))}
              />
              <TextInput
                label="Constraint"
                value={form.constraint}
                onChange={(v) => set('constraint', v)}
                errors={errorsFor(errors, '/policy/constraint')}
                placeholder=">=2 <3"
                hint="A version range checked by the scheme. Empty means any."
              />
              <TextArea label="Include" value={form.include} onChange={(v) => set('include', v)} errors={errorsFor(errors, '/policy/include')} hint="One RE2 regex per line, matched against the raw version. Empty means all." />
              <TextArea label="Exclude" value={form.exclude} onChange={(v) => set('exclude', v)} errors={errorsFor(errors, '/policy/exclude')} hint="One RE2 regex per line. Exclude wins over include." />
              <TextInput label="Cooldown" value={form.cooldown} onChange={(v) => set('cooldown', v)} errors={errorsFor(errors, '/policy/cooldown')} hint="0 to 8760h. A version is emitted once it is this old." />
              <fieldset>
                <legend className="mb-1 text-sm font-medium">Bump</legend>
                <div className="flex gap-4">
                  {BUMPS.map((b) => (
                    <Checkbox
                      key={b}
                      label={b}
                      checked={form.bump.includes(b)}
                      onChange={(on) => set('bump', on ? [...form.bump, b] : form.bump.filter((x) => x !== b))}
                    />
                  ))}
                </div>
                <p className="mt-1 text-xs text-muted-foreground">Keep only these bumps. None checked means all.</p>
                {errorsFor(errors, '/policy/bump').length > 0 && (
                  <p role="alert" tabIndex={-1} className="text-xs text-destructive">
                    {errorsFor(errors, '/policy/bump').join('; ')}
                  </p>
                )}
              </fieldset>
            </div>
            <Checkbox
              label="Take a baseline"
              checked={form.baseline}
              onChange={(v) => set('baseline', v)}
              hint="Versions known at the first check are recorded without events."
            />
          </fieldset>

          {conflict && !creating && <ChangedElsewhere kind="watch" onReload={onReload ?? onCancel} />}
          <ProblemSummary error={conflict ? undefined : save.error} claimed={WATCH_FIELD_POINTERS} />

          <div className="flex gap-2">
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : creating ? 'Create watch' : 'Save changes'}
            </Button>
            <Button type="button" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}
