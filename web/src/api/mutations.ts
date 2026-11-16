import type { QueryClient } from '@tanstack/react-query'
import { api } from './client'
import { ApiError, unwrap } from './problem'
import type { components } from './schema'

type S = components['schemas']

export type WatchWrite = S['WatchWrite']

/** A delete answers `204` with no body: success is the status, not `data`. */
export async function unwrapVoid(call: Promise<{ error?: unknown; response: Response }>): Promise<void> {
  const { error, response } = await call
  if (response.ok) return
  const body = error as S['Problem'] | undefined
  throw new ApiError(response.status, body && typeof body === 'object' && 'title' in body ? body : undefined)
}

export const putWatch = (name: string, body: WatchWrite) =>
  unwrap(api.PUT('/v1/watches/{name}', { params: { path: { name } }, body }))

export const deleteWatch = (name: string) =>
  unwrapVoid(api.DELETE('/v1/watches/{name}', { params: { path: { name } } }))

/** Refetches every active query whose key starts with one of the roots. */
export function invalidateRoots(client: QueryClient, roots: string[]) {
  return client.invalidateQueries({ predicate: (q) => roots.includes(String(q.queryKey[0])) })
}

/** Query-key roots that show watches; a write refetches them all. */
export const invalidateWatches = (client: QueryClient) => invalidateRoots(client, ['watches', 'watch', 'overview'])
