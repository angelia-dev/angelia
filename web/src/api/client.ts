import createClient, { type Middleware } from 'openapi-fetch'
import type { paths } from './schema'

/** Marks a cookie-authenticated unsafe request as made by the UI (ADR 0033 §6). */
export const csrfMiddleware: Middleware = {
  onRequest({ request }) {
    if (request.method !== 'GET' && request.method !== 'HEAD' && request.method !== 'OPTIONS') {
      request.headers.set('X-Angelia-CSRF', '1')
    }
    return request
  },
}

export function createApiClient(baseUrl = '') {
  const client = createClient<paths>({ baseUrl, credentials: 'same-origin' })
  client.use(csrfMiddleware)
  return client
}

export const api = createApiClient()
