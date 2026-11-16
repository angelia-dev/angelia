import type { SinkWrite } from '@/api/mutations'
import type { SchemaField } from '@/api/plugins'
import type { Sink } from '@/api/types'
import { pairsFrom, pairsToMap, type FieldErrors, type Pair } from '@/components/form'
import { NAME_RE } from '@/lib/watch-form'

/** The `*_ref` keys name stored secrets and need the admin scope; they get a picker. */
export const isRefKey = (key: string) => key.endsWith('_ref')

export interface SinkFormState {
  name: string
  kind: string
  /** Scalar config values as text; booleans are 'true' / 'false', unset is ''. */
  values: Record<string, string>
  /** Object-valued config keys (for example `inputs`) as key/value pairs. */
  objects: Record<string, Pair[]>
  /** Keys the kind's schema does not describe, carried over unchanged on edit. */
  extra: Record<string, unknown>
  /** The whole config as JSON, for a kind that publishes no schema. */
  raw: string
}

export const emptySinkForm = (): SinkFormState => ({ name: '', kind: '', values: {}, objects: {}, extra: {}, raw: '{}' })

const isStringMap = (v: unknown): v is Record<string, string> =>
  typeof v === 'object' && v !== null && !Array.isArray(v) && Object.values(v).every((x) => typeof x === 'string')

/** The form for an existing sink; masked and redacted values are never carried into it. */
export function sinkToForm(s: Sink, fields: SchemaField[]): SinkFormState {
  const f = emptySinkForm()
  f.name = s.name
  f.kind = s.kind
  const masked = new Set(s.masked_fields)
  const known = new Map(fields.map((x) => [x.key, x]))
  const rawConfig: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(s.config)) {
    if (masked.has(k) || v === '[REDACTED]') continue
    rawConfig[k] = v
    const field = known.get(k)
    if (field && typeof v === 'object' && v !== null && isStringMap(v)) f.objects[k] = pairsFrom(v)
    else if (field && (typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean')) f.values[k] = String(v)
    else if (fields.length > 0) f.extra[k] = v
  }
  f.raw = JSON.stringify(rawConfig, null, 2)
  return f
}

/** Config keys of a schema that the form edits (secret-marked keys cannot be set through the API). */
export const editableFields = (fields: SchemaField[]) => fields.filter((x) => !x.secret)

/** The `config` of the write body; throws nothing, `validateSinkForm` has run first. */
export function formToConfig(f: SinkFormState, fields: SchemaField[]): Record<string, unknown> {
  if (fields.length === 0) {
    try {
      const parsed: unknown = JSON.parse(f.raw)
      return typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {}
    } catch {
      return {}
    }
  }
  const config: Record<string, unknown> = { ...f.extra }
  for (const field of editableFields(fields)) {
    if (field.type === 'object') {
      const map = pairsToMap(f.objects[field.key] ?? [])
      if (Object.keys(map).length > 0) config[field.key] = map
      continue
    }
    const v = (f.values[field.key] ?? '').trim()
    if (v === '') continue
    if (field.type === 'boolean') config[field.key] = v === 'true'
    else if (field.type === 'integer' || field.type === 'number') config[field.key] = Number(v)
    else config[field.key] = v
  }
  return config
}

export const formToSinkWrite = (f: SinkFormState, fields: SchemaField[]): SinkWrite => ({
  kind: f.kind,
  config: formToConfig(f, fields),
})

/** Checks that mirror the kind's `config_schema`; keys are JSON pointers as in the API's errors. */
export function validateSinkForm(f: SinkFormState, fields: SchemaField[], opts: { creating: boolean }): FieldErrors {
  const e: FieldErrors = {}
  const add = (p: string, m: string) => (e[p] ??= []).push(m)
  if (opts.creating && !NAME_RE.test(f.name)) add('/name', 'Use 1 to 63 characters: lower-case letters, digits, "." "_" "-", starting with a letter or digit.')
  if (!f.kind) add('/kind', 'Choose a kind.')
  if (fields.length === 0) {
    try {
      const parsed: unknown = JSON.parse(f.raw)
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) add('/config', 'The configuration must be a JSON object.')
    } catch {
      add('/config', 'The configuration is not valid JSON.')
    }
    return e
  }
  for (const field of editableFields(fields)) {
    const p = `/config/${field.key}`
    if (field.type === 'object') {
      const pairs = f.objects[field.key] ?? []
      if (pairs.some((x) => x.key.trim() === '' && x.value !== '')) add(p, 'Every entry needs a key.')
      continue
    }
    const v = (f.values[field.key] ?? '').trim()
    if (v === '') {
      if (field.required) add(p, 'Required.')
      continue
    }
    if ((field.type === 'integer' || field.type === 'number') && (Number.isNaN(Number(v)) || (field.type === 'integer' && !Number.isInteger(Number(v))))) {
      add(p, field.type === 'integer' ? 'Enter a whole number.' : 'Enter a number.')
    }
    if (field.enum && !field.enum.includes(v)) add(p, `Choose one of: ${field.enum.join(', ')}.`)
    if (field.pattern && field.type !== 'integer') {
      try {
        if (!new RegExp(field.pattern).test(v)) add(p, 'Not in the expected format.')
      } catch {
        // a pattern JavaScript cannot compile is left to the server
      }
    }
  }
  return e
}
