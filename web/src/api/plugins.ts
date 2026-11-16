import { useQuery } from '@tanstack/react-query'
import { api } from './client'
import { fetchCapped } from './queries'
import { unwrap } from './problem'
import type { Plugin } from './types'

/** Every source or sink kind of the instance (`GET /v1/plugins`), at most 10 pages. */
export function usePlugins(iface: 'source' | 'sink') {
  return useQuery({
    queryKey: ['plugins', iface],
    staleTime: 5 * 60_000,
    queryFn: async (): Promise<Plugin[]> =>
      (
        await fetchCapped(
          (cursor) =>
            unwrap(api.GET('/v1/plugins', { params: { query: { limit: 100, cursor, interface: iface } } })),
          10,
        )
      ).items,
  })
}

/** The properties of a kind's `config_schema`, in schema order, with `required` and secret marks. */
export interface SchemaField {
  key: string
  description?: string
  required: boolean
  secret: boolean
  type?: string
  enum?: string[]
  default?: unknown
  pattern?: string
}

export function schemaFields(schema: Plugin['config_schema'] | undefined): SchemaField[] {
  if (!schema || typeof schema !== 'object') return []
  const props = (schema as { properties?: Record<string, Record<string, unknown>> }).properties ?? {}
  const required = new Set((schema as { required?: string[] }).required ?? [])
  return Object.entries(props).map(([key, p]) => ({
    key,
    description: typeof p.description === 'string' ? p.description : undefined,
    required: required.has(key),
    secret: p['x-angelia-secret'] === true,
    type: typeof p.type === 'string' ? p.type : undefined,
    enum: Array.isArray(p.enum) ? (p.enum as string[]) : undefined,
    default: p.default,
    pattern: typeof p.pattern === 'string' ? p.pattern : undefined,
  }))
}
