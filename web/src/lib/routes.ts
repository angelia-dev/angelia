import type { Sink, Subscription, Watch } from '@/api/types'
import { scalar } from '@/lib/yaml'

/**
 * The routing picture of the Configure board: which watches a subscription's filter matches
 * today. A subscription is a filter, not a link to one watch (docs/watch-file.md §2.5), so this
 * is computed from `filter.source`, `filter.labels` and `filter.package`; `bump` and `type` act
 * on each event and do not change which watches are routed.
 */

const PACKAGE_KEYS: Record<string, string> = {
  npm: 'name',
  'github-release': 'repo',
  'github-tag': 'repo',
  'gitlab-release': 'project',
}
const FALLBACK_KEYS = ['name', 'package', 'module', 'image', 'repo', 'project']

/** `data.package` of the watch's events, as the server derives it from the canonical coordinates. */
export function watchPackage(w: Pick<Watch, 'source' | 'coordinates'>): string {
  const c = w.coordinates
  const key = PACKAGE_KEYS[w.source]
  if (key) return c[key] ?? ''
  for (const k of FALLBACK_KEYS) if (c[k]) return c[k]
  return Object.keys(c)
    .sort()
    .map((k) => c[k])
    .join('/')
}

/** Go's `path.Match` as a RegExp: `*` and `?` stop at `/`, `[...]` classes, `\` escapes. */
export function globToRegExp(glob: string): RegExp | null {
  let re = '^'
  for (let i = 0; i < glob.length; i++) {
    const ch = glob[i]!
    if (ch === '*') re += '[^/]*'
    else if (ch === '?') re += '[^/]'
    else if (ch === '\\') {
      i++
      if (i >= glob.length) return null
      re += escapeRe(glob[i]!)
    } else if (ch === '[') {
      const end = glob.indexOf(']', i + 2)
      if (end < 0) return null
      let body = glob.slice(i + 1, end)
      if (body.startsWith('^')) body = '^' + body.slice(1).replace(/[\]\\]/g, '\\$&')
      else body = body.replace(/[\]\\^]/g, '\\$&')
      re += `[${body}]`
      i = end
    } else re += escapeRe(ch)
  }
  try {
    return new RegExp(re + '$')
  } catch {
    return null
  }
}

function escapeRe(s: string) {
  return s.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')
}

type RouteFilter = Pick<Subscription['filter'], 'source' | 'labels' | 'package'>

/** Whether a subscription filter routes the watch's events today. */
export function filterMatchesWatch(f: RouteFilter, w: Pick<Watch, 'source' | 'coordinates' | 'labels'>): boolean {
  if (f.source.length > 0 && !f.source.includes(w.source)) return false
  for (const [k, v] of Object.entries(f.labels)) if (w.labels[k] !== v) return false
  if (f.package) {
    const re = globToRegExp(f.package)
    if (!re || !re.test(watchPackage(w))) return false
  }
  return true
}

export interface Cord {
  watch: string
  subscription: string
}

/** Every watch to subscription cord of the board. */
export function cords(watches: Watch[], subscriptions: Subscription[]): Cord[] {
  const out: Cord[] = []
  for (const s of subscriptions) for (const w of watches) if (filterMatchesWatch(s.filter, w)) out.push({ watch: w.name, subscription: s.name })
  return out
}

/** A subscription name for a cord from a watch to a sink: lower case, at most 63 characters. */
export function routeName(watch: string, sink: string, taken: ReadonlySet<string>): string {
  const base = `${watch}-to-${sink}`.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').slice(0, 60).replace(/^[^a-z0-9]+/, '') || 'route'
  if (!taken.has(base)) return base
  for (let i = 2; ; i++) if (!taken.has(`${base}-${i}`)) return `${base}-${i}`
}

// --- YAML of the whole configuration, in the watch-file format (docs/watch-file.md). ---

type Json = string | number | boolean | null | Json[] | { [k: string]: Json }

function yamlValue(v: Json, indent: string): string {
  if (v === null) return 'null'
  if (typeof v === 'string') return scalar(v)
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  if (Array.isArray(v)) {
    if (v.length === 0) return '[]'
    if (v.every((x) => typeof x !== 'object' || x === null)) return `[${v.map((x) => yamlValue(x, '')).join(', ')}]`
    return '\n' + v.map((x) => `${indent}- ${yamlValue(x, indent + '  ').replace(/^\n\s*/, '')}`).join('\n')
  }
  const keys = Object.keys(v)
  if (keys.length === 0) return '{}'
  return (
    '\n' +
    keys
      .map((k) => {
        const val = yamlValue(v[k]!, indent + '  ')
        return `${indent}${scalar(k)}:${val.startsWith('\n') ? '' : ' '}${val}`
      })
      .join('\n')
  )
}

function entry(obj: Record<string, Json>): string {
  // A list item: the first key on the dash line, the others under it.
  const body = yamlValue(obj, '    ').replace(/^\n/, '')
  return body.replace(/^ {4}/, '  - ')
}

function isEmpty(v: unknown): boolean {
  return v === '' || v === null || v === undefined || (Array.isArray(v) && v.length === 0) || (typeof v === 'object' && v !== null && Object.keys(v).length === 0)
}

function compact(o: Record<string, unknown>): Record<string, Json> {
  const out: Record<string, Json> = {}
  for (const [k, v] of Object.entries(o)) if (!isEmpty(v)) out[k] = v as Json
  return out
}

export function sinkEntry(s: Pick<Sink, 'name' | 'kind' | 'config'>): Record<string, Json> {
  return { name: s.name, kind: s.kind, config: s.config as Json }
}

export function watchEntry(w: Watch): Record<string, Json> {
  return compact({
    name: w.name,
    source: w.source,
    coordinates: w.coordinates,
    interval: w.interval,
    scheme: w.scheme,
    labels: w.labels,
    policy: compact(w.policy as Record<string, unknown>),
  })
}

export function subscriptionEntry(s: Subscription): Record<string, Json> {
  const filter = compact({ ...s.filter, type: s.filter.type.length === 1 && s.filter.type[0] === 'release.published' ? [] : s.filter.type })
  return compact({
    name: s.name,
    sink: s.sink,
    filter,
    policy: compact({ ...s.policy, cooldown: s.policy.cooldown === '0s' || s.policy.cooldown === '0' ? '' : s.policy.cooldown }),
    template: s.template,
  })
}

/** One watch file holding every object given, in the order apply wants: sinks, watches, subscriptions. */
export function configToYaml(c: { sinks?: Sink[]; watches?: Watch[]; subscriptions?: Subscription[] }): string {
  const parts = ['version: 1']
  const section = (key: string, items: Record<string, Json>[]) => {
    if (items.length) parts.push('', `${key}:`, ...items.map(entry))
  }
  section('sinks', (c.sinks ?? []).map(sinkEntry))
  section('watches', (c.watches ?? []).map(watchEntry))
  section('subscriptions', (c.subscriptions ?? []).map(subscriptionEntry))
  return parts.join('\n') + '\n'
}
