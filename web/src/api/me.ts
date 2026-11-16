import { useQuery } from '@tanstack/react-query'

// GET /v1/me is added to the OpenAPI spec by the sign-in story (#262). Until
// then its response is typed here; replace this with the generated type then.
export interface Me {
  principal_id: string
  name: string
  scope: 'read' | 'write' | 'admin'
  kind: 'token' | 'session'
  role?: 'viewer' | 'editor' | 'admin'
  user?: {
    provider: string
    subject: string
    login: string
    name?: string
    avatar_url?: string
  }
}

export class NotSignedInError extends Error {
  constructor() {
    super('not signed in')
    this.name = 'NotSignedInError'
  }
}

export async function fetchMe(fetchFn: typeof fetch = fetch): Promise<Me> {
  const res = await fetchFn('/v1/me', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
  if (res.status === 401) throw new NotSignedInError()
  if (!res.ok) throw new Error(`GET /v1/me: ${res.status}`)
  return (await res.json()) as Me
}

export function useMe() {
  return useQuery({
    queryKey: ['me'],
    queryFn: () => fetchMe(),
    retry: false,
    staleTime: 60_000,
  })
}
