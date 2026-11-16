import { useQuery } from '@tanstack/react-query'
import { api } from './client'
import { fetchCapped } from './queries'
import { unwrap } from './problem'

/** One page of secrets: names and metadata, never a value. */
export const fetchSecrets = () => (cursor: string | undefined) =>
  unwrap(api.GET('/v1/secrets', { params: { query: { limit: 100, cursor } } }))

/** The names of the stored secrets, for the `*_ref` pickers (admin only). */
export function useSecretNames(enabled = true) {
  return useQuery({
    queryKey: ['secrets', 'names'],
    enabled,
    queryFn: async () => (await fetchCapped(fetchSecrets(), 10)).items.map((s) => s.name),
  })
}

/** Every sink (up to 10 pages), for the subscription form's sink picker. */
export function useAllSinks() {
  return useQuery({
    queryKey: ['sinks', 'all'],
    queryFn: async () =>
      (
        await fetchCapped(
          (cursor) => unwrap(api.GET('/v1/sinks', { params: { query: { limit: 100, cursor } } })),
          10,
        )
      ).items,
  })
}
