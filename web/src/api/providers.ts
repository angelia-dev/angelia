import { useQuery } from '@tanstack/react-query'

/** A sign-in provider as GET /ui/auth/providers lists it: its route name and button label. */
export interface Provider {
  name: string
  label: string
}

/** Where the provider list comes from: a server route outside /v1, so not in the OpenAPI spec. */
export const PROVIDERS_PATH = '/ui/auth/providers'

const NAME = /^[a-z][a-z0-9-]{0,31}$/

/** Where a provider's sign-in starts: a browser redirect owned by the server. */
export const loginPath = (name: string) => `/ui/auth/login/${encodeURIComponent(name)}`

/** The providers, in the server's order; entries that are not a name and a label are dropped. */
export async function fetchProviders(): Promise<Provider[]> {
  const res = await globalThis.fetch(PROVIDERS_PATH, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
  if (!res.ok) throw new Error(`The sign-in options could not be loaded (${res.status}).`)
  const body: unknown = await res.json()
  if (!Array.isArray(body)) throw new Error('The sign-in options could not be read.')
  return body.flatMap((p: unknown) => {
    if (typeof p !== 'object' || p === null) return []
    const { name, label } = p as Record<string, unknown>
    if (typeof name !== 'string' || !NAME.test(name)) return []
    return [{ name, label: typeof label === 'string' && label.trim() ? label : name }]
  })
}

export function useProviders() {
  return useQuery({ queryKey: ['auth-providers'], queryFn: fetchProviders, retry: 1, staleTime: Infinity })
}
