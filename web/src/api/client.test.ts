import { describe, expect, it } from 'vitest'
import { createApiClient } from './client'

describe('api client', () => {
  it('adds the CSRF header to unsafe methods only, and sends same-origin credentials', async () => {
    const seen: Request[] = []
    const client = createApiClient('http://localhost')
    const fetchFn = (req: Request) => {
      seen.push(req)
      return Promise.resolve(new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }))
    }
    await client.GET('/v1/watches', { fetch: fetchFn })
    await client.DELETE('/v1/tokens/{id}', { params: { path: { id: 'x' } }, fetch: fetchFn })
    expect(seen[0]?.headers.get('X-Angelia-CSRF')).toBeNull()
    expect(seen[1]?.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(seen[0]?.credentials).toBe('same-origin')
  })
})
