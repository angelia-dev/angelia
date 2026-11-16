import type { SubscriptionWrite } from '@/api/mutations'
import type { ShortEventType, Subscription } from '@/api/types'
import { pairsFrom, pairsToMap, type FieldErrors, type Pair } from '@/components/form'
import { parseDuration } from '@/lib/duration'
import { BUMPS, LABEL_KEY_RE, NAME_RE, PRERELEASE } from '@/lib/watch-form'

export const EVENT_TYPES = ['release.published', 'release.yanked', 'release.prerelease'] as const
export const TEMPLATE_MAX = 32768

export interface SubscriptionFormState {
  name: string
  sink: string
  sources: string[]
  labels: Pair[]
  bump: (typeof BUMPS)[number][]
  package: string
  types: string[]
  prerelease: '' | (typeof PRERELEASE)[number] // '' inherits the watch's mode
  cooldown: string
  template: string
}

export const emptySubscriptionForm = (): SubscriptionFormState => ({
  name: '',
  sink: '',
  sources: [],
  labels: [],
  bump: [],
  package: '',
  types: ['release.published'],
  prerelease: '',
  cooldown: '0',
  template: '',
})

export const subscriptionToForm = (s: Subscription): SubscriptionFormState => ({
  name: s.name,
  sink: s.sink,
  sources: [...s.filter.source],
  labels: pairsFrom(s.filter.labels),
  bump: [...s.filter.bump],
  package: s.filter.package,
  types: [...s.filter.type],
  prerelease: s.policy.prerelease ?? '',
  cooldown: s.policy.cooldown,
  template: s.template,
})

/** The `PUT /v1/subscriptions/{name}` body; the name is the path. */
export const formToSubscriptionWrite = (f: SubscriptionFormState): SubscriptionWrite => ({
  sink: f.sink,
  filter: {
    source: f.sources,
    labels: pairsToMap(f.labels),
    bump: f.bump,
    package: f.package.trim(),
    type: f.types as ShortEventType[],
  },
  policy: { prerelease: f.prerelease === '' ? null : f.prerelease, cooldown: f.cooldown.trim() || '0' },
  template: f.template,
})

/** Checks that mirror SubscriptionWrite; keys are the JSON pointers the API uses. */
export function validateSubscriptionForm(f: SubscriptionFormState, opts: { creating: boolean }): FieldErrors {
  const e: FieldErrors = {}
  const add = (p: string, m: string) => (e[p] ??= []).push(m)
  if (opts.creating && !NAME_RE.test(f.name)) add('/name', 'Use 1 to 63 characters: lower-case letters, digits, "." "_" "-", starting with a letter or digit.')
  if (!f.sink) add('/sink', 'Choose a sink.')
  if (f.types.length === 0) add('/filter/type', 'Choose at least one event type.')
  const seen = new Set<string>()
  for (const p of f.labels.filter((x) => x.key.trim() !== '')) {
    const k = p.key.trim()
    if (!LABEL_KEY_RE.test(k)) add('/filter/labels', `Label key "${k}" is not valid.`)
    if (seen.has(k)) add('/filter/labels', `Label key "${k}" is used twice.`)
    seen.add(k)
    if (p.value.length > 253) add('/filter/labels', `Value of label "${k}" is longer than 253 characters.`)
  }
  if (f.labels.length > 32) add('/filter/labels', 'At most 32 labels.')
  const cooldown = parseDuration(f.cooldown.trim() || '0')
  if (cooldown === null) add('/policy/cooldown', 'Use a duration such as 24h, or 0.')
  else if (cooldown > 8760 * 3600) add('/policy/cooldown', 'At most 8760h.')
  if (f.template.length > TEMPLATE_MAX) add('/template', `At most ${TEMPLATE_MAX} characters.`)
  return e
}

export const SUBSCRIPTION_FIELD_POINTERS = [
  '/name',
  '/sink',
  '/filter/source',
  '/filter/labels',
  '/filter/bump',
  '/filter/package',
  '/filter/type',
  '/policy/prerelease',
  '/policy/cooldown',
  '/template',
]
