import { useCallback, useEffect, useId, useRef, useState, type ReactNode, type RefObject } from 'react'
import { ApiError } from '@/api/problem'
import type { Problem } from '@/api/types'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

export const inputClass =
  'h-9 w-full rounded-md border border-border bg-card px-3 text-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary aria-[invalid=true]:border-destructive'

/** Field errors keyed by the JSON pointer of the request body, as the API reports them. */
export type FieldErrors = Record<string, string[]>

/** The `422` errors of a problem, grouped by pointer. */
export function problemFieldErrors(error: unknown): FieldErrors {
  const out: FieldErrors = {}
  if (!(error instanceof ApiError) || error.status !== 422) return out
  for (const e of error.problem?.errors ?? []) (out[e.pointer] ??= []).push(e.detail)
  return out
}

/** Messages for a pointer and everything below it (`/policy` covers `/policy/include/0`). */
export function errorsFor(errors: FieldErrors, pointer: string): string[] {
  return Object.entries(errors)
    .filter(([p]) => p === pointer || p.startsWith(pointer + '/'))
    .flatMap(([, msgs]) => msgs)
}

export function mergeErrors(...sets: FieldErrors[]): FieldErrors {
  const out: FieldErrors = {}
  for (const set of sets) for (const [p, m] of Object.entries(set)) out[p] = [...(out[p] ?? []), ...m]
  return out
}

/**
 * After a refused submit, moves focus to the first control with an error, or to an error
 * message (WCAG 3.3.1). Call `focusError()` when client checks fail; a failed write (`failed`
 * turning true) does it too.
 */
export function useFocusFirstError(formRef: RefObject<HTMLFormElement | null>, failed: boolean) {
  const [tick, setTick] = useState(0)
  useEffect(() => {
    if (tick === 0) return
    formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"], [role="alert"]')?.focus()
  }, [tick, formRef])
  useEffect(() => {
    if (failed) setTick((t) => t + 1)
  }, [failed])
  return useCallback(() => setTick((t) => t + 1), [])
}

export interface ControlProps {
  id: string
  'aria-describedby'?: string
  'aria-invalid'?: boolean
}

/** A labelled control with its hint and the errors of its pointer(s) next to it. */
export function Field({
  label,
  errors = [],
  hint,
  required,
  className,
  children,
}: {
  label: string
  errors?: string[]
  hint?: ReactNode
  required?: boolean
  className?: string
  children: (props: ControlProps) => ReactNode
}) {
  const id = useId()
  const describedBy = [hint ? `${id}-hint` : '', errors.length ? `${id}-err` : ''].filter(Boolean).join(' ')
  return (
    <div className={cn('flex flex-col gap-1', className)}>
      <label htmlFor={id} className="text-sm font-medium">
        {label}
        {required && <span aria-hidden="true"> *</span>}
      </label>
      {children({
        id,
        'aria-describedby': describedBy || undefined,
        'aria-invalid': errors.length ? true : undefined,
      })}
      {hint && (
        <p id={`${id}-hint`} className="text-xs text-muted-foreground">
          {hint}
        </p>
      )}
      {errors.length > 0 && (
        <p id={`${id}-err`} role="alert" className="text-xs text-destructive">
          {errors.join('; ')}
        </p>
      )}
    </div>
  )
}

export function TextInput({
  label,
  value,
  onChange,
  errors,
  hint,
  required,
  placeholder,
  disabled,
  type = 'text',
  autoComplete,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  errors?: string[]
  hint?: ReactNode
  required?: boolean
  placeholder?: string
  disabled?: boolean
  type?: 'text' | 'password'
  autoComplete?: string
}) {
  return (
    <Field label={label} errors={errors} hint={hint} required={required}>
      {(p) => (
        <input
          {...p}
          type={type}
          className={inputClass}
          value={value}
          placeholder={placeholder}
          disabled={disabled}
          autoComplete={autoComplete}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </Field>
  )
}

export function SelectInput({
  label,
  value,
  onChange,
  options,
  errors,
  hint,
  required,
  disabled,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string }[]
  errors?: string[]
  hint?: ReactNode
  required?: boolean
  disabled?: boolean
}) {
  return (
    <Field label={label} errors={errors} hint={hint} required={required}>
      {(p) => (
        <select {...p} className={inputClass} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      )}
    </Field>
  )
}

export function TextArea({
  label,
  value,
  onChange,
  errors,
  hint,
  rows = 3,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  errors?: string[]
  hint?: ReactNode
  rows?: number
}) {
  return (
    <Field label={label} errors={errors} hint={hint}>
      {(p) => (
        <textarea
          {...p}
          rows={rows}
          className={cn(inputClass, 'h-auto py-2 font-mono')}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </Field>
  )
}

export function Checkbox({ label, checked, onChange, hint }: { label: string; checked: boolean; onChange: (v: boolean) => void; hint?: string }) {
  const id = useId()
  return (
    <div className="flex items-start gap-2">
      <input id={id} type="checkbox" className="mt-1" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      <label htmlFor={id} className="text-sm">
        {label}
        {hint && <span className="block text-xs text-muted-foreground">{hint}</span>}
      </label>
    </div>
  )
}

export interface Pair {
  id: number
  key: string
  value: string
}

let pairSeq = 0
export const newPair = (key = '', value = ''): Pair => ({ id: ++pairSeq, key, value })
export const pairsFrom = (m: Record<string, string>): Pair[] => Object.entries(m).map(([k, v]) => newPair(k, v))

/** An editable list of key/value pairs (labels, free coordinates, inputs). */
export function PairsEditor({
  legend,
  pairs,
  onChange,
  errors = [],
  keyLabel = 'Key',
  valueLabel = 'Value',
  hint,
}: {
  legend: string
  pairs: Pair[]
  onChange: (p: Pair[]) => void
  errors?: string[]
  keyLabel?: string
  valueLabel?: string
  hint?: ReactNode
}) {
  const set = (id: number, patch: Partial<Pair>) => onChange(pairs.map((p) => (p.id === id ? { ...p, ...patch } : p)))
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="text-sm font-medium">{legend}</legend>
      {pairs.map((p) => (
        <div key={p.id} className="flex items-center gap-2">
          <input
            aria-label={`${legend} ${keyLabel.toLowerCase()}`}
            className={inputClass}
            value={p.key}
            placeholder={keyLabel}
            onChange={(e) => set(p.id, { key: e.target.value })}
          />
          <input
            aria-label={`${legend} ${valueLabel.toLowerCase()}`}
            className={inputClass}
            value={p.value}
            placeholder={valueLabel}
            onChange={(e) => set(p.id, { value: e.target.value })}
          />
          <Button type="button" variant="outline" size="sm" onClick={() => onChange(pairs.filter((x) => x.id !== p.id))}>
            Remove
            <span className="sr-only"> {legend.toLowerCase()} entry {p.key}</span>
          </Button>
        </div>
      ))}
      <div>
        <Button type="button" variant="outline" size="sm" onClick={() => onChange([...pairs, newPair()])}>
          Add {legend.toLowerCase()}
        </Button>
      </div>
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      {errors.length > 0 && (
        <p role="alert" tabIndex={-1} className="text-xs text-destructive">
          {errors.join('; ')}
        </p>
      )}
    </fieldset>
  )
}

/** Pairs as a map; a blank key is dropped. */
export function pairsToMap(pairs: Pair[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const p of pairs) if (p.key.trim() !== '') out[p.key.trim()] = p.value
  return out
}

/**
 * Why a write failed: a 403 names the scope, a 409 and other problems show title and
 * detail, and a 422 lists the errors no field claimed (`claimed` pointers have their own message).
 */
export function ProblemSummary({ error, claimed = [] }: { error: unknown; claimed?: string[] }) {
  if (!error) return null
  const problem: Problem | undefined = error instanceof ApiError ? error.problem : undefined
  const status = error instanceof ApiError ? error.status : undefined
  let title = problem?.title ?? 'Something went wrong'
  let detail = problem?.detail ?? (error instanceof Error ? error.message : String(error))
  if (status === 403) {
    title = 'Not allowed'
    detail = problem?.required_scope
      ? `This change needs the ${problem.required_scope} role. ${problem.detail ?? ''}`.trim()
      : (problem?.detail ?? 'Your role does not allow this change.')
  }
  const loose = (problem?.errors ?? []).filter((e) => !claimed.some((c) => e.pointer === c || e.pointer.startsWith(c + '/')))
  return (
    <div role="alert" tabIndex={-1} className="rounded-lg border border-destructive p-3 text-sm">
      <p className="font-semibold text-destructive">{title}</p>
      <p className="mt-1">{detail}</p>
      {status === 422 && loose.length > 0 && (
        <ul className="mt-2 list-disc pl-5">
          {loose.map((e, i) => (
            <li key={`${e.pointer}-${i}`}>
              {e.pointer && <code>{e.pointer}</code>}
              {e.pointer ? ': ' : ''}
              {e.detail}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** A delete button that asks first; the action runs only after "Confirm delete". */
export function ConfirmDelete({
  what,
  consequence,
  onConfirm,
  busy,
  error,
}: {
  what: string
  consequence?: string
  onConfirm: () => void
  busy?: boolean
  error?: unknown
}) {
  const [asking, setAsking] = useState(false)
  // Focus moves into the confirmation (on Cancel, the safe choice) and back to Delete when it closes.
  const deleteRef = useRef<HTMLButtonElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const opened = useRef(false)
  useEffect(() => {
    if (asking) cancelRef.current?.focus()
    else if (opened.current) deleteRef.current?.focus()
    opened.current = opened.current || asking
  }, [asking])
  const descId = useId()
  if (!asking) {
    return (
      <Button ref={deleteRef} variant="destructive" onClick={() => setAsking(true)}>
        Delete
        <span className="sr-only"> {what}</span>
      </Button>
    )
  }
  return (
    <div
      role="alertdialog"
      aria-label={`Confirm delete of ${what}`}
      aria-describedby={descId}
      className="flex flex-col gap-2 rounded-lg border border-destructive p-3 text-sm"
      onKeyDown={(e) => {
        if (e.key === 'Escape' && !busy) setAsking(false)
      }}
    >
      <p id={descId}>
        Delete <strong>{what}</strong>? {consequence ?? 'This cannot be undone.'}
      </p>
      {error ? <ProblemSummary error={error} /> : null}
      <div className="flex gap-2">
        <Button variant="destructive" size="sm" disabled={busy} onClick={onConfirm}>
          {busy ? 'Deleting…' : 'Confirm delete'}
        </Button>
        <Button ref={cancelRef} variant="outline" size="sm" disabled={busy} onClick={() => setAsking(false)}>
          Cancel
        </Button>
      </div>
    </div>
  )
}
