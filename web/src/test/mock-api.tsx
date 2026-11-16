import { render } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router'
import { vi } from 'vitest'
import { createAppRouter } from '@/router'

export interface Call {
  method: string
  url: URL
  headers: Headers
  /** The request body text, empty when there is none. */
  body: string
}

type Handler = Response | ((call: Call) => unknown) | Record<string, unknown> | unknown[]

/** A problem+json response, as the API sends it. */
export function problem(status: number, title: string, detail?: string) {
  return new Response(JSON.stringify({ type: 'about:blank', title, status, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

/**
 * Stubs global fetch. Keys are `/path` (GET) or `METHOD /path`; a value is the
 * JSON body, a Response, or a function of the call returning either. Unknown
 * paths answer 404. Returns the list of calls made.
 */
export function mockApi(routes: Record<string, Handler>) {
  const calls: Call[] = []
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(typeof input === 'string' && input.startsWith('/') ? location.origin + input : input, init)
    const url = new URL(request.url)
    const call = { method: request.method, url, headers: request.headers, body: await request.clone().text() }
    calls.push(call)
    const key = request.method === 'GET' ? url.pathname : `${request.method} ${url.pathname}`
    if (!(key in routes)) return Promise.resolve(problem(404, 'Not found', `no mock for ${key}`))
    const h = routes[key]
    const out = await (typeof h === 'function' ? (h as (c: Call) => unknown)(call) : h)
    if (out instanceof Response) return Promise.resolve(out)
    return Promise.resolve(
      new Response(JSON.stringify(out), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    )
  })
  vi.stubGlobal('fetch', fetchMock)
  return { calls, fetchMock, count: (path: string) => calls.filter((c) => c.url.pathname === path).length }
}

export function renderApp(path = '/ui/', client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  const router = createAppRouter(createMemoryHistory({ initialEntries: [path] }))
  const result = render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  return { ...result, client }
}

/** A controllable EventSource for the live-events tests. */
export class MockEventSource {
  static instances: MockEventSource[] = []
  static readonly CLOSED = 2
  readonly url: string
  readyState = 0
  onopen: (() => void) | null = null
  onmessage: ((m: MessageEvent<string>) => void) | null = null
  onerror: (() => void) | null = null
  closed = false

  constructor(url: string | URL) {
    this.url = String(url)
    MockEventSource.instances.push(this)
  }

  close() {
    this.closed = true
    this.readyState = 2
  }

  open() {
    this.readyState = 1
    this.onopen?.()
  }

  emit(record: unknown) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(record) }))
  }

  /** The browser is retrying (readyState CONNECTING) or gave up (CLOSED). */
  fail(fatal = false) {
    this.readyState = fatal ? 2 : 0
    this.onerror?.()
  }

  static reset() {
    MockEventSource.instances = []
  }

  static get last() {
    return MockEventSource.instances[MockEventSource.instances.length - 1]
  }
}

/** A problem+json response with extra members (`errors`, `required_scope`). */
export function problemWith(status: number, title: string, extra: Record<string, unknown> = {}) {
  return new Response(JSON.stringify({ type: 'about:blank', title, status, ...extra }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

/** A JSON response with a status other than 200 (`201` on create). */
export const jsonResponse = (status: number, body?: unknown) =>
  new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

/** A 200 JSON response with an `ETag`, as a GET of a watch, sink or subscription. */
export const etagged = (body: unknown, etag: string) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json', ETag: etag } })
