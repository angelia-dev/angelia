import { useQuery } from '@tanstack/react-query'
import { api } from './client'
import { ApiError, unwrap } from './problem'
import type { Me, UiRole } from './types'

export type { Me, UiRole }

export class NotSignedInError extends Error {
  constructor() {
    super('not signed in')
    this.name = 'NotSignedInError'
  }
}

export async function fetchMe(): Promise<Me> {
  try {
    return await unwrap(api.GET('/v1/me'))
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) throw new NotSignedInError()
    throw err
  }
}

export function useMe() {
  return useQuery({ queryKey: ['me'], queryFn: fetchMe, retry: false, staleTime: 60_000 })
}

const RANK: Record<UiRole, number> = { viewer: 0, editor: 1, admin: 2 }

/** The role of a principal; an API token (no UI role) is judged by its scope. */
export function effectiveRole(me: Me): UiRole {
  if (me.role) return me.role
  return me.scope === 'admin' ? 'admin' : me.scope === 'write' ? 'editor' : 'viewer'
}

export function hasRole(me: Me, min: UiRole): boolean {
  return RANK[effectiveRole(me)] >= RANK[min]
}
