import { act, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { eventPage, eventRecord, me } from '@/test/fixtures'
import { MockEventSource, mockApi, problem, renderApp } from '@/test/mock-api'

beforeEach(() => {
  MockEventSource.reset()
  vi.stubGlobal('EventSource', MockEventSource)
})
afterEach(() => vi.unstubAllGlobals())

describe('events timeline', () => {
  it('lists events newest first and pages forward with `after` until the tail', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': ({ url }) =>
        url.searchParams.get('after') === '0' ? eventPage([eventRecord(1), eventRecord(2)], true) : eventPage([eventRecord(3)]),
    })
    renderApp('/ui/events')
    const table = await screen.findByRole('table', {
      name: 'Events, newest first',
    })
    await vi.waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(4))
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((r) => within(r).getAllByRole('cell')[0]?.textContent)).toEqual(['3', '2', '1'])
    expect(api.calls.filter((c) => c.url.pathname === '/v1/events').map((c) => c.url.searchParams.get('after'))).toEqual(['0', '2'])
    expect(rows[0]).toHaveTextContent('release.published')
  })

  it('stops auto-paging after a few pages and offers "Load newer events"', async () => {
    let next = 0
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': () => eventPage([eventRecord(++next)], true),
    })
    renderApp('/ui/events')
    const more = await screen.findByRole('button', {
      name: 'Load newer events',
    })
    await vi.waitFor(() => expect(api.count('/v1/events')).toBe(5))
    expect(MockEventSource.instances).toHaveLength(0) // live updates wait for the newest event
    await userEvent.click(more)
    await vi.waitFor(() => expect(api.count('/v1/events')).toBe(6))
  })

  it('offers "Load newer events" instead of the empty state when a page is empty but has more', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': ({ url }) =>
        url.searchParams.get('after') === '0' ? { items: [], next_after: 40, has_more: true } : eventPage([eventRecord(41)]),
    })
    renderApp('/ui/events')
    const more = await screen.findByRole('button', {
      name: 'Load newer events',
    })
    expect(screen.queryByText('No events yet')).not.toBeInTheDocument()
    expect(screen.getByText(/loaded oldest first/)).toBeInTheDocument()
    await vi.waitFor(() => expect(api.count('/v1/events')).toBe(2)) // auto-paging continues
    await vi.waitFor(() => expect(screen.getByRole('table', { name: 'Events, newest first' })).toBeInTheDocument())
    expect(more).not.toBeNull()
  })

  it('does not send an invalid watch or source filter', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': eventPage([eventRecord(1)]),
    })
    renderApp('/ui/events')
    await screen.findByRole('table')
    await userEvent.type(screen.getByLabelText('Source'), 'PyPI!')
    expect(await screen.findByText(/Use a source kind/)).toBeInTheDocument()
    expect(api.calls.every((c) => !c.url.searchParams.has('source'))).toBe(true)
  })

  it('passes the watch, type and source filters', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': eventPage([eventRecord(1)]),
    })
    renderApp('/ui/events')
    await screen.findByRole('table')
    await userEvent.type(screen.getByLabelText('Watch (name or id)'), 'requests')
    await userEvent.selectOptions(screen.getByLabelText('Type'), 'release.yanked')
    await userEvent.type(screen.getByLabelText('Source'), 'pypi')
    await vi.waitFor(() => {
      const q = api.calls.filter((c) => c.url.pathname === '/v1/events').at(-1)?.url.searchParams
      expect(q?.getAll('watch')).toEqual(['requests'])
      expect(q?.getAll('type')).toEqual(['release.yanked'])
      expect(q?.getAll('source')).toEqual(['pypi'])
    })
  })

  it('shows an empty state and a problem', async () => {
    mockApi({ '/v1/me': me(), '/v1/events': eventPage([]) })
    renderApp('/ui/events')
    expect(await screen.findByText('No events yet')).toBeInTheDocument()
  })

  it('shows the problem detail when listing fails', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/events': problem(410, 'Cursor expired', 'oldest seq is 40'),
    })
    renderApp('/ui/events')
    expect(await screen.findByRole('alert')).toHaveTextContent('oldest seq is 40')
  })
})

describe('live updates', () => {
  async function renderLive() {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': eventPage([eventRecord(1), eventRecord(2)]),
    })
    renderApp('/ui/events')
    await screen.findByRole('table')
    await vi.waitFor(() => expect(MockEventSource.instances).toHaveLength(1))
    return api
  }

  it('opens the stream after the last seq with the same-origin cookie and prepends new events', async () => {
    await renderLive()
    const es = MockEventSource.last as MockEventSource
    expect(es.url).toBe('/v1/events/stream?after=2')
    expect(screen.getByRole('status')).toHaveTextContent('Connecting')
    act(() => es.open())
    expect(screen.getByText('Live')).toBeInTheDocument()
    act(() => es.emit(eventRecord(3, { version: '9.9.9' })))
    const rows = within(screen.getByRole('table')).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('requests@9.9.9')
    expect(rows).toHaveLength(3)
    act(() => es.emit(eventRecord(3, { version: '9.9.9' }))) // a repeat is ignored
    expect(within(screen.getByRole('table')).getAllByRole('row')).toHaveLength(4)
  })

  it('pauses and resumes from the last seq seen', async () => {
    await renderLive()
    const first = MockEventSource.last as MockEventSource
    act(() => first.open())
    act(() => first.emit(eventRecord(3)))
    await userEvent.click(screen.getByRole('button', { name: 'Pause' }))
    expect(first.closed).toBe(true)
    expect(screen.getByText('Paused')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Resume' }))
    const second = MockEventSource.last as MockEventSource
    expect(second).not.toBe(first)
    expect(second.url).toBe('/v1/events/stream?after=3')
  })

  it('shows reconnecting while the browser retries, then live again', async () => {
    await renderLive()
    const es = MockEventSource.last as MockEventSource
    act(() => es.open())
    act(() => es.fail(false))
    expect(screen.getByText('Connection lost, reconnecting…')).toBeInTheDocument()
    act(() => es.open())
    expect(screen.getByText('Live')).toBeInTheDocument()
  })

  it('offers a manual reconnect when the stream was refused', async () => {
    await renderLive()
    const es = MockEventSource.last as MockEventSource
    act(() => es.fail(true))
    expect(screen.getByText('Live updates stopped')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    expect(MockEventSource.instances).toHaveLength(2)
  })

  it('reports a browser without EventSource', async () => {
    vi.stubGlobal('EventSource', undefined)
    mockApi({ '/v1/me': me(), '/v1/events': eventPage([]) })
    renderApp('/ui/events')
    expect(await screen.findByText('Live updates are not supported by this browser')).toBeInTheDocument()
  })
})

describe('event detail', () => {
  it('shows the CloudEvent JSON in a pre block', async () => {
    const rec = eventRecord(7)
    mockApi({ '/v1/me': me(), [`/v1/events/${rec.event.id}`]: rec })
    renderApp(`/ui/events/${rec.event.id}`)
    const pre = await screen.findByLabelText('CloudEvent JSON')
    expect(pre.tagName).toBe('PRE')
    expect(JSON.parse(pre.textContent ?? '')).toEqual(rec.event)
    expect(screen.getByRole('link', { name: 'requests' })).toHaveAttribute('href', '/ui/watches/requests')
  })

  it('shows a not-found problem', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/events/NOPE': problem(404, 'Event not found', 'no such event'),
    })
    renderApp('/ui/events/NOPE')
    expect(await screen.findByRole('alert')).toHaveTextContent('no such event')
  })
})
