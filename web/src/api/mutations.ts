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

export type SinkWrite = S['SinkWrite']
export type SubscriptionWrite = S['SubscriptionWrite']
export type SecretWrite = S['SecretWrite']

/** Whether a resource of that name exists: 200 yes, 404 no, anything else is an error. */
export async function exists(call: Promise<{ error?: unknown; response: Response }>): Promise<boolean> {
  const { error, response } = await call
  if (response.ok) return true
  if (response.status === 404) return false
  const body = error as S['Problem'] | undefined
  throw new ApiError(response.status, body && typeof body === 'object' && 'title' in body ? body : undefined)
}

export const sinkExists = (name: string) => exists(api.GET('/v1/sinks/{name}', { params: { path: { name } } }))
export const subscriptionExists = (name: string) =>
  exists(api.GET('/v1/subscriptions/{name}', { params: { path: { name } } }))
export const secretExists = (name: string) => exists(api.GET('/v1/secrets/{name}', { params: { path: { name } } }))

export const putSink = (name: string, body: SinkWrite) =>
  unwrap(api.PUT('/v1/sinks/{name}', { params: { path: { name } }, body }))
export const deleteSink = (name: string) =>
  unwrapVoid(api.DELETE('/v1/sinks/{name}', { params: { path: { name } } }))

/** `dryRun` validates the body (template included) and writes nothing. */
export const putSubscription = (name: string, body: SubscriptionWrite, dryRun = false) =>
  unwrap(
    api.PUT('/v1/subscriptions/{name}', {
      params: { path: { name }, query: dryRun ? { dry_run: true } : {} },
      body,
    }),
  )
export const deleteSubscription = (name: string) =>
  unwrapVoid(api.DELETE('/v1/subscriptions/{name}', { params: { path: { name } } }))

export const putSecret = (name: string, body: SecretWrite) =>
  unwrap(api.PUT('/v1/secrets/{name}', { params: { path: { name } }, body }))
export const deleteSecret = (name: string) =>
  unwrapVoid(api.DELETE('/v1/secrets/{name}', { params: { path: { name } } }))

// A sink names secrets with `*_ref` keys, so a sink write changes the secrets' `referenced_by`.
export const invalidateSinks = (client: QueryClient) => invalidateRoots(client, ['sinks', 'sink-index', 'secrets'])
export const invalidateSubscriptions = (client: QueryClient) =>
  invalidateRoots(client, ['subscriptions', 'subscription-index'])
export const invalidateSecrets = (client: QueryClient) => invalidateRoots(client, ['secrets', 'sinks'])
