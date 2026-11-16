import { useRef, useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { hasRole, useMe } from '@/api/me'
import { invalidateSubscriptions, putSubscription } from '@/api/mutations'
import { usePlugins } from '@/api/plugins'
import { isPreconditionFailed } from '@/api/problem'
import { useAllSinks } from '@/api/secrets'
import type { Subscription } from '@/api/types'
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
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { TemplatePreview } from '@/components/template-preview'
import { ChangedElsewhere } from '@/components/versioned'
import {
  EVENT_TYPES,
  SUBSCRIPTION_FIELD_POINTERS,
  emptySubscriptionForm,
  formToSubscriptionWrite,
  subscriptionToForm,
  validateSubscriptionForm,
  type SubscriptionFormState,
} from '@/lib/subscription-form'
import { BUMPS, PRERELEASE } from '@/lib/watch-form'

function toggle<T>(list: T[], item: T, on: boolean): T[] {
  return on ? [...list, item] : list.filter((x) => x !== item)
}

/**
 * Create (no `initial`) or edit an API-managed subscription. Editors and admins. A create sends
 * `If-None-Match: *`, an edit `If-Match: etag`.
 */
export function SubscriptionForm({
  initial,
  etag,
  onReload,
  onDone,
  onCancel,
}: {
  initial?: Subscription
  etag?: string
  onReload?: () => void
  onDone: () => void
  onCancel: () => void
}) {
  const creating = !initial
  const client = useQueryClient()
  const me = useMe()
  // POST /v1/template-previews needs the admin role.
  const canPreview = !!me.data && hasRole(me.data, 'admin')
  const sinks = useAllSinks()
  const sources = usePlugins('source')
  const [form, setForm] = useState<SubscriptionFormState>(() => (initial ? subscriptionToForm(initial) : emptySubscriptionForm()))
  const [clientErrors, setClientErrors] = useState<FieldErrors>({})
  const set = (patch: Partial<SubscriptionFormState>) => setForm((f) => ({ ...f, ...patch }))

  const save = useMutation({
    mutationFn: () =>
      creating
        ? putSubscription(form.name, formToSubscriptionWrite(form), false, { create: true })
        : putSubscription(initial.name, formToSubscriptionWrite(form), false, { etag }),
    onSuccess: async () => {
      await invalidateSubscriptions(client)
      onDone()
    },
  })
  // The API checks the template against sample events when it validates a write: a dry run writes nothing.
  const check = useMutation({ mutationFn: () => putSubscription(initial?.name ?? form.name, formToSubscriptionWrite(form), true) })

  const conflict = isPreconditionFailed(save.error)
  const taken: FieldErrors = conflict && creating ? { '/name': ['A subscription with this name already exists. Edit it from the list.'] } : {}
  const errors = mergeErrors(clientErrors, problemFieldErrors(save.error), problemFieldErrors(check.error), taken)

  const formRef = useRef<HTMLFormElement>(null)
  const focusError = useFocusFirstError(formRef, save.isError || check.isError)

  function validate() {
    const found = validateSubscriptionForm(form, { creating })
    setClientErrors(found)
    if (Object.keys(found).length > 0) focusError()
    return Object.keys(found).length === 0
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    check.reset()
    if (validate()) save.mutate()
  }

  const sourceKinds = [...new Set([...(sources.data?.map((p) => p.kind) ?? []), ...form.sources])].sort()
  // An API subscription may only name a sink the API manages (409 cross-owner-reference otherwise).
  const usable = (sinks.data ?? []).filter((s) => s.managed_by === 'api' || s.name === form.sink)
  const title = creating ? 'New subscription' : `Edit ${initial.name}`
  const shownError = conflict ? undefined : (save.error ?? (check.error && !Object.keys(problemFieldErrors(check.error)).length ? check.error : undefined))

  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <form ref={formRef} aria-label={title} noValidate onSubmit={onSubmit} className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">
            Saving replaces the whole subscription; if someone else saved it since you opened this form, the save is refused and
            nothing is overwritten. Editing the filter or policy decides held deliveries again.
          </p>
          <div className="grid gap-4 md:grid-cols-2">
            <TextInput label="Name" required value={form.name} disabled={!creating} onChange={(v) => set({ name: v })} errors={errorsFor(errors, '/name')} />
            <SelectInput
              label="Sink"
              required
              value={form.sink}
              onChange={(v) => set({ sink: v })}
              errors={errorsFor(errors, '/sink')}
              hint="Only sinks managed through the API can be used here; sinks from the watch file cannot."
              options={[{ value: '', label: sinks.isPending ? 'Loading…' : 'Choose a sink' }, ...usable.map((s) => ({ value: s.name, label: `${s.name} (${s.kind})` }))]}
            />
          </div>

          <fieldset className="flex flex-col gap-3 rounded-lg border border-border p-3">
            <legend className="px-1 text-sm font-medium">Filter</legend>
            <fieldset>
              <legend className="mb-1 text-sm font-medium">Event types</legend>
              <div className="flex flex-wrap gap-4">
                {EVENT_TYPES.map((t) => (
                  <Checkbox key={t} label={t} checked={form.types.includes(t)} onChange={(on) => set({ types: toggle(form.types, t, on) })} />
                ))}
              </div>
              {errorsFor(errors, '/filter/type').length > 0 && (
                <p role="alert" tabIndex={-1} className="text-xs text-destructive">
                  {errorsFor(errors, '/filter/type').join('; ')}
                </p>
              )}
            </fieldset>
            <fieldset>
              <legend className="mb-1 text-sm font-medium">Source kinds</legend>
              <div className="flex flex-wrap gap-4">
                {sourceKinds.map((k) => (
                  <Checkbox key={k} label={k} checked={form.sources.includes(k)} onChange={(on) => set({ sources: toggle(form.sources, k, on) })} />
                ))}
              </div>
              <p className="mt-1 text-xs text-muted-foreground">None checked means any source.</p>
              {errorsFor(errors, '/filter/source').length > 0 && (
                <p role="alert" tabIndex={-1} className="text-xs text-destructive">
                  {errorsFor(errors, '/filter/source').join('; ')}
                </p>
              )}
            </fieldset>
            <PairsEditor legend="Watch label" pairs={form.labels} onChange={(p) => set({ labels: p })} errors={errorsFor(errors, '/filter/labels')} hint="The watch must carry all of these labels." />
            <fieldset>
              <legend className="mb-1 text-sm font-medium">Bump</legend>
              <div className="flex gap-4">
                {BUMPS.map((b) => (
                  <Checkbox key={b} label={b} checked={form.bump.includes(b)} onChange={(on) => set({ bump: toggle(form.bump, b, on) })} />
                ))}
              </div>
              <p className="mt-1 text-xs text-muted-foreground">None checked means any. Narrows the watch&apos;s own bump filter.</p>
              {errorsFor(errors, '/filter/bump').length > 0 && (
                <p role="alert" tabIndex={-1} className="text-xs text-destructive">
                  {errorsFor(errors, '/filter/bump').join('; ')}
                </p>
              )}
            </fieldset>
            <TextInput label="Package glob" value={form.package} onChange={(v) => set({ package: v })} errors={errorsFor(errors, '/filter/package')} placeholder="@types/*" hint="A glob on the package name. Empty means any." />
          </fieldset>

          <fieldset className="flex flex-col gap-3 rounded-lg border border-border p-3">
            <legend className="px-1 text-sm font-medium">Policy overrides</legend>
            <div className="grid gap-4 md:grid-cols-2">
              <SelectInput
                label="Pre-releases"
                value={form.prerelease}
                onChange={(v) => set({ prerelease: v as SubscriptionFormState['prerelease'] })}
                errors={errorsFor(errors, '/policy/prerelease')}
                hint="Narrows what the watch admits; it can never widen it."
                options={[{ value: '', label: "Inherit the watch's mode" }, ...PRERELEASE.map((p) => ({ value: p, label: p }))]}
              />
              <TextInput label="Cooldown" value={form.cooldown} onChange={(v) => set({ cooldown: v })} errors={errorsFor(errors, '/policy/cooldown')} hint="The larger of this and the watch's cooldown applies." />
            </div>
          </fieldset>

          <div className="flex flex-col gap-2">
            <TextArea
              label="Template"
              rows={6}
              value={form.template}
              onChange={(v) => {
                set({ template: v })
                check.reset()
              }}
              errors={errorsFor(errors, '/template')}
              hint="A Go text/template over the event, for example {{ .data.package }}. Empty sends the CloudEvents JSON."
            />
            <div className="flex items-center gap-3">
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={check.isPending}
                onClick={() => {
                  if (validate()) check.mutate()
                }}
              >
                {check.isPending ? 'Checking…' : 'Check template'}
              </Button>
              {check.isSuccess && (
                <span role="status" className="text-sm">
                  The API accepts this subscription and its template (nothing was saved).
                </span>
              )}
            </div>
            {canPreview && <TemplatePreview template={form.template} />}
          </div>

          {conflict && !creating && <ChangedElsewhere kind="subscription" onReload={onReload ?? onCancel} />}
          <ProblemSummary error={shownError} claimed={SUBSCRIPTION_FIELD_POINTERS} />
          <div className="flex gap-2">
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : creating ? 'Create subscription' : 'Save changes'}
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
