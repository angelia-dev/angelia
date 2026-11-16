import { useMemo, useRef, useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { invalidateSinks, putSink, sinkExists } from '@/api/mutations'
import { schemaFields, usePlugins, type SchemaField } from '@/api/plugins'
import { useSecretNames } from '@/api/secrets'
import type { Plugin, Sink } from '@/api/types'
import {
  Checkbox,
  PairsEditor,
  ProblemSummary,
  SelectInput,
  TextArea,
  TextInput,
  errorsFor,
  mergeErrors,
  problemFieldErrors,
  useFocusFirstError,
  type FieldErrors,
} from '@/components/form'
import { Skeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  editableFields,
  emptySinkForm,
  formToSinkWrite,
  isRefKey,
  sinkToForm,
  validateSinkForm,
  type SinkFormState,
} from '@/lib/sink-form'

class NameTaken extends Error {}

/** Create (no `initial`) or edit an API-managed sink. Admin only: the API enforces it too. */
export function SinkForm(props: { initial?: Sink; onDone: () => void; onCancel: () => void }) {
  const plugins = usePlugins('sink')
  // The form starts from the kind's schema, so wait for the list of kinds first.
  if (plugins.isPending) return <Skeleton className="mb-6 h-40 w-full" />
  return <SinkFormBody {...props} plugins={plugins.data ?? []} pluginsFailed={plugins.isError} />
}

function SinkFormBody({
  initial,
  onDone,
  onCancel,
  plugins,
  pluginsFailed,
}: {
  initial?: Sink
  onDone: () => void
  onCancel: () => void
  plugins: Plugin[]
  pluginsFailed: boolean
}) {
  const creating = !initial
  const client = useQueryClient()
  const secrets = useSecretNames()
  const [clientErrors, setClientErrors] = useState<FieldErrors>({})
  const [form, setForm] = useState<SinkFormState>(() =>
    initial ? sinkToForm(initial, schemaFields(plugins.find((p) => p.kind === initial.kind)?.config_schema)) : emptySinkForm(),
  )
  const kinds = plugins.map((p) => p.kind)
  const plugin = plugins.find((p) => p.kind === form.kind)
  const fields: SchemaField[] = useMemo(() => schemaFields(plugin?.config_schema), [plugin])
  const secretFields = fields.filter((f) => f.secret)

  const set = (patch: Partial<SinkFormState>) => setForm((f) => ({ ...f, ...patch }))
  const setValue = (key: string, v: string) => setForm((f) => ({ ...f, values: { ...f.values, [key]: v } }))

  const save = useMutation({
    mutationFn: async () => {
      if (creating && (await sinkExists(form.name))) throw new NameTaken()
      return putSink(initial?.name ?? form.name, formToSinkWrite(form, fields))
    },
    onSuccess: async () => {
      await invalidateSinks(client)
      onDone()
    },
  })

  const taken: FieldErrors = save.error instanceof NameTaken ? { '/name': ['A sink with this name already exists. Edit it from the list.'] } : {}
  const errors = mergeErrors(clientErrors, problemFieldErrors(save.error), taken)
  const claimed = ['/name', '/kind', ...editableFields(fields).map((f) => `/config/${f.key}`), ...(fields.length === 0 ? ['/config'] : [])]

  const formRef = useRef<HTMLFormElement>(null)
  const focusError = useFocusFirstError(formRef, save.isError)
  // Below the admin scope the server cuts URLs to scheme://host; saving that back would break the sink.
  const redacted = Boolean(initial?.url_redacted)

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (redacted) return
    const found = validateSinkForm(form, fields, { creating })
    setClientErrors(found)
    if (Object.keys(found).length === 0) save.mutate()
    else focusError()
  }

  function control(f: SchemaField) {
    const p = `/config/${f.key}`
    const hint = f.description
    if (f.type === 'object') {
      return (
        <PairsEditor
          key={f.key}
          legend={f.key}
          pairs={form.objects[f.key] ?? []}
          onChange={(pairs) => set({ objects: { ...form.objects, [f.key]: pairs } })}
          hint={hint}
          errors={errorsFor(errors, p)}
        />
      )
    }
    if (f.type === 'boolean') {
      return <Checkbox key={f.key} label={f.key} hint={hint} checked={form.values[f.key] === 'true'} onChange={(on) => setValue(f.key, String(on))} />
    }
    if (isRefKey(f.key)) {
      const current = form.values[f.key] ?? ''
      const names = [...new Set([...(secrets.data ?? []), ...(current ? [current] : [])])]
      return (
        <SelectInput
          key={f.key}
          label={f.key}
          required={f.required}
          value={current}
          onChange={(v) => setValue(f.key, v)}
          errors={errorsFor(errors, p)}
          hint={<>{hint} Pick a stored secret by name; the value is never shown. {secrets.isError && 'The list of secrets could not be loaded.'}</>}
          options={[{ value: '', label: '(none)' }, ...names.map((n) => ({ value: n, label: n }))]}
        />
      )
    }
    if (f.enum) {
      return (
        <SelectInput
          key={f.key}
          label={f.key}
          required={f.required}
          value={form.values[f.key] ?? ''}
          onChange={(v) => setValue(f.key, v)}
          errors={errorsFor(errors, p)}
          hint={hint}
          options={[{ value: '', label: f.default !== undefined ? `Default (${String(f.default)})` : '(unset)' }, ...f.enum.map((v) => ({ value: v, label: v }))]}
        />
      )
    }
    return (
      <TextInput
        key={f.key}
        label={f.key}
        required={f.required}
        value={form.values[f.key] ?? ''}
        onChange={(v) => setValue(f.key, v)}
        errors={errorsFor(errors, p)}
        placeholder={f.default !== undefined ? String(f.default) : undefined}
        hint={hint}
      />
    )
  }

  const title = creating ? 'New sink' : `Edit ${initial.name}`
  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <form ref={formRef} aria-label={title} noValidate onSubmit={onSubmit} className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">
            The configuration is replaced as a whole and the next delivery uses it. The API has no version check, so the last write
            wins. A sink never takes a secret value: name a stored secret (<code>*_ref</code>) or an environment variable of the
            instance (<code>*_env</code>).
          </p>
          <div className="grid gap-4 md:grid-cols-2">
            <TextInput label="Name" required value={form.name} disabled={!creating} onChange={(v) => set({ name: v })} errors={errorsFor(errors, '/name')} />
            <SelectInput
              label="Kind"
              required
              value={form.kind}
              disabled={!creating}
              onChange={(v) => set({ kind: v, values: {}, objects: {}, extra: {}, raw: '{}' })}
              errors={errorsFor(errors, '/kind')}
              options={[{ value: '', label: pluginsFailed ? 'Kinds could not be loaded' : 'Choose a kind' }, ...[...new Set([...kinds, ...(form.kind ? [form.kind] : [])])].sort().map((k) => ({ value: k, label: k }))]}
            />
          </div>
          {form.kind && fields.length > 0 && <div className="grid gap-4 md:grid-cols-2">{editableFields(fields).map(control)}</div>}
          {form.kind && plugin && fields.length === 0 && (
            <TextArea
              label="Configuration (JSON)"
              rows={6}
              value={form.raw}
              onChange={(v) => set({ raw: v })}
              errors={errorsFor(errors, '/config')}
              hint="This kind publishes no configuration schema. Name secrets with *_ref or *_env keys; never paste a secret value."
            />
          )}
          {secretFields.length > 0 && (
            <p className="text-xs text-muted-foreground">
              Keys the kind marks secret ({secretFields.map((f) => f.key).join(', ')}) cannot be set through the API.
            </p>
          )}
          {initial && initial.masked_fields.length > 0 && (
            <p className="text-xs text-muted-foreground">Masked by the server, not editable here: {initial.masked_fields.join(', ')}.</p>
          )}
          {redacted && (
            <p role="alert" className="text-sm text-destructive">
              The URLs of this sink were redacted for your token, so it cannot be saved from here without losing them.
            </p>
          )}
          <ProblemSummary error={save.error instanceof NameTaken ? undefined : save.error} claimed={claimed} />
          <div className="flex gap-2">
            <Button type="submit" disabled={save.isPending || redacted}>
              {save.isPending ? 'Saving…' : creating ? 'Create sink' : 'Save changes'}
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
