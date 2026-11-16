import type { components } from '@/api/schema'

type S = components['schemas']

export const me = (over: Partial<S['Me']> = {}): S['Me'] => ({
  principal_id: 'github:1',
  name: 'octocat',
  scope: 'admin',
  kind: 'session',
  role: 'admin',
  user: { provider: 'github', subject: '1', login: 'octocat', name: 'The Octocat', avatar_url: 'https://avatars.example/1.png' },
  session_id: null,
  ...over,
})

export const watch = (name: string, over: Partial<S['Watch']> = {}, status: Partial<S['WatchStatus']> = {}): S['Watch'] => ({
  name,
  id: `ID-${name}`,
  managed_by: 'api',
  source: 'pypi',
  coordinates: { package: name },
  interval: '1h',
  scheme: 'pep440',
  labels: { team: 'platform' },
  policy: { prerelease: 'none', include: [], exclude: [], constraint: '', bump: [], cooldown: '0s', baseline: true },
  status: {
    baseline_done: true,
    effective_interval: '1h',
    last_checked_at: new Date(Date.now() - 3 * 60_000).toISOString(),
    next_check_at: new Date(Date.now() + 57 * 60_000).toISOString(),
    error_count: 0,
    ...status,
  },
  ...over,
})

export const page = <T>(items: T[], next_cursor: string | null = null) => ({ items, next_cursor })

export const eventRecord = (seq: number, over: { version?: string; time?: string; type?: string; source?: string } = {}): S['EventRecord'] => {
  const version = over.version ?? `1.${seq}.0`
  return {
    seq,
    ref_event_id: null,
    event: {
      specversion: '1.0',
      type: (over.type ?? 'dev.angelia.release.published.v1') as S['v1-2']['type'],
      source: 'angelia://test/watches/ID-requests',
      id: `EVT${String(seq).padStart(23, '0')}`,
      subject: `pypi:requests@${version}`,
      time: over.time ?? new Date(Date.now() - seq * 60_000).toISOString(),
      datacontenttype: 'application/json',
      dataschema: 'https://github.com/angelia-dev/spec/schemas/release/v1.json',
      data: {
        source: over.source ?? 'pypi',
        package: 'requests',
        version,
        normalized_version: version,
        previous_version: null,
        scheme: 'pep440',
        bump: null,
        backport: false,
        prerelease: false,
        yanked: false,
        published_at: null,
        detected_at: new Date().toISOString(),
        cooldown_satisfied_at: null,
        urls: { release: null, changelog: null, artifact: null },
        notes: null,
        watch: { id: 'ID-requests', name: 'requests', labels: {} },
        purl: null,
        $defs: { openToken: '', timestamp: '', ulid: '', urlOrNull: null },
      },
    },
  }
}

export const eventPage = (items: S['EventRecord'][], has_more = false) => ({
  items,
  next_after: items.length ? (items[items.length - 1] as S['EventRecord']).seq : 0,
  has_more,
})

export const delivery = (id: string, over: Partial<S['Delivery']> = {}): S['Delivery'] => ({
  id,
  event_id: 'EVT00000000000000000000001',
  subscription_id: 'SUB1',
  subscription_name: 'to-slack',
  sink: 'slack',
  idempotency_key: `key-${id}`,
  status: 'pending',
  attempts: 0,
  next_attempt_at: new Date(Date.now() + 60_000).toISOString(),
  held_until: null,
  last_error: null,
  ...over,
})

export const subscription = (name: string, sink: string, id = 'SUB1'): S['Subscription'] => ({
  name,
  id,
  managed_by: 'file',
  sink,
  filter: { source: [], labels: {}, bump: [], package: '', type: ['release.published'] },
  policy: { prerelease: null, cooldown: '0s' },
  template: '',
})

export const sink = (name: string, over: Partial<S['Sink']> = {}): S['Sink'] => ({
  name,
  id: `SINK-${name}`,
  managed_by: 'file',
  kind: 'webhook',
  config: { url: 'https://hooks.example/***' },
  url_redacted: true,
  masked_fields: ['secret_ref'],
  ...over,
})

export const plugin = (kind: string, iface: 'source' | 'sink', config_schema: object | null = null): S['Plugin'] => ({
  kind,
  interface: iface,
  runtime: 'builtin',
  version: null,
  abi: null,
  config_schema: config_schema as S['Plugin']['config_schema'],
  allowed_hosts: [],
})

export const secret = (name: string, over: Partial<S['Secret']> = {}): S['Secret'] => ({
  name,
  hosts: [],
  key_id: '0123456789abcdef',
  created_at: new Date(Date.now() - 86_400_000).toISOString(),
  updated_at: new Date(Date.now() - 3_600_000).toISOString(),
  referenced_by: [],
  ...over,
})
