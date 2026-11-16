import type { WatchWrite } from '@/api/mutations'
import type { Watch } from '@/api/types'
import { newPair, pairsFrom, pairsToMap, type FieldErrors, type Pair } from '@/components/form'
import { parseDuration } from '@/lib/duration'

export const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,62}$/
export const KIND_RE = /^[a-z0-9][a-z0-9-]{0,62}$/
export const LABEL_KEY_RE = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$/
// eslint-disable-next-line no-control-regex
const CONTROL_RE = /[\x00-\x1F\x7F]/

export const SCHEMES = ['semver', 'pep440', 'calver', 'lexical']
export const PRERELEASE = ['none', 'only', 'include'] as const
export const BUMPS = ['major', 'minor', 'patch'] as const
type Bump = (typeof BUMPS)[number]
type Prerelease = (typeof PRERELEASE)[number]

export interface WatchFormState {
  name: string
  source: string
  coordinates: Pair[]
  interval: string
  scheme: string // '' = default by source kind
  labels: Pair[]
  prerelease: Prerelease
  include: string // one regex per line
  exclude: string
  constraint: string
  bump: Bump[]
  cooldown: string
  baseline: boolean
}

export function emptyWatchForm(): WatchFormState {
  return {
    name: '',
    source: '',
    coordinates: [],
    interval: '1h',
    scheme: '',
    labels: [],
    prerelease: 'none',
    include: '',
    exclude: '',
    constraint: '',
    bump: [],
    cooldown: '0',
    baseline: true,
  }
}

export function watchToForm(w: Watch): WatchFormState {
  const p = w.policy
  return {
    name: w.name,
    source: w.source,
    coordinates: pairsFrom(w.coordinates),
    interval: w.interval,
    scheme: w.scheme,
    labels: pairsFrom(w.labels),
    prerelease: p.prerelease ?? 'none',
    include: (p.include ?? []).join('\n'),
    exclude: (p.exclude ?? []).join('\n'),
    constraint: p.constraint ?? '',
    bump: [...(p.bump ?? [])],
    cooldown: p.cooldown ?? '0',
    baseline: p.baseline ?? true,
  }
}

const lines = (s: string) => s.split('\n').map((l) => l.trim()).filter(Boolean)

/** The `PUT /v1/watches/{name}` body for the form; the name is the path. */
export function formToWrite(f: WatchFormState): WatchWrite {
  const body: WatchWrite = {
    source: f.source.trim(),
    coordinates: pairsToMap(f.coordinates),
    interval: f.interval.trim() || '1h',
    labels: pairsToMap(f.labels),
    policy: {
      prerelease: f.prerelease,
      include: lines(f.include),
      exclude: lines(f.exclude),
      constraint: f.constraint.trim(),
      bump: f.bump,
      cooldown: f.cooldown.trim() || '0',
      baseline: f.baseline,
    },
  }
  if (f.scheme) body.scheme = f.scheme
  return body
}

/** Client-side checks that mirror WatchWrite; keys are the JSON pointers the API uses. */
export function validateWatchForm(
  f: WatchFormState,
  opts: { creating: boolean; requiredCoordinates?: string[] },
): FieldErrors {
  const e: FieldErrors = {}
  const add = (p: string, m: string) => (e[p] ??= []).push(m)

  if (opts.creating && !NAME_RE.test(f.name)) add('/name', 'Use 1 to 63 characters: lower-case letters, digits, "." "_" "-", starting with a letter or digit.')
  if (!f.source.trim()) add('/source', 'Choose a source.')
  else if (!KIND_RE.test(f.source.trim())) add('/source', 'Not a valid source name.')

  const coords = pairsToMap(f.coordinates)
  if (Object.keys(coords).length === 0) add('/coordinates', 'At least one coordinate is required.')
  for (const k of opts.requiredCoordinates ?? []) if (!(coords[k] ?? '').trim()) add(`/coordinates/${k}`, 'Required.')
  for (const [k, v] of Object.entries(coords)) if (v === '' && !(opts.requiredCoordinates ?? []).includes(k)) add(`/coordinates/${k}`, 'Give a value or remove the entry.')

  const interval = parseDuration(f.interval.trim() || '1h')
  if (interval === null) add('/interval', 'Use a duration such as 30m or 6h.')
  else if (interval < 300 || interval > 720 * 3600) add('/interval', 'Must be between 5m and 720h.')

  if (f.scheme && !/^[a-z0-9][a-z0-9-]{0,62}$/.test(f.scheme)) add('/scheme', 'Not a valid scheme name.')

  const labels = f.labels.filter((p) => p.key.trim() !== '')
  if (labels.length > 32) add('/labels', 'At most 32 labels.')
  const seen = new Set<string>()
  for (const p of labels) {
    const k = p.key.trim()
    if (!LABEL_KEY_RE.test(k)) add('/labels', `Label key "${k}" is not valid (letters, digits, "._/-", at most 63 characters).`)
    if (seen.has(k)) add('/labels', `Label key "${k}" is used twice.`)
    seen.add(k)
    if (p.value.length > 253) add('/labels', `Value of label "${k}" is longer than 253 characters.`)
    if (CONTROL_RE.test(p.value)) add('/labels', `Value of label "${k}" has control characters.`)
  }

  for (const key of ['include', 'exclude'] as const) {
    const items = lines(f[key])
    if (items.length > 16) add(`/policy/${key}`, 'At most 16 patterns.')
  }
  const cooldown = parseDuration(f.cooldown.trim() || '0')
  if (cooldown === null) add('/policy/cooldown', 'Use a duration such as 24h, or 0.')
  else if (cooldown < 0 || cooldown > 8760 * 3600) add('/policy/cooldown', 'Must be between 0 and 8760h.')
  return e
}

/** The pointers the watch form shows next to a field; other errors go to the summary. */
export const WATCH_FIELD_POINTERS = [
  '/name',
  '/source',
  '/coordinates',
  '/interval',
  '/scheme',
  '/labels',
  '/policy/prerelease',
  '/policy/include',
  '/policy/exclude',
  '/policy/constraint',
  '/policy/bump',
  '/policy/cooldown',
  '/policy/baseline',
]

export { newPair }
