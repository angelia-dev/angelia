import { api } from './client'
import { unwrap } from './problem'
import type { components } from './schema'

export type AdoptionObject = components['schemas']['AdoptionObject']

/** Hands watch-file resources over to the API in place (admin only, spec item 55). */
export const adoptResources = (objects: AdoptionObject[], dryRun = false) =>
  unwrap(api.POST('/v1/adoptions', { body: { objects, dry_run: dryRun } }))
