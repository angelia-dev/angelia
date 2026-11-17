import { act, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { eventPageDesc, eventRecord, me } from '@/test/fixtures'
import { MockEventSource, mockApi, problem, renderApp } from '@/test/mock-api'

beforeEach(() => {
  MockEventSource.reset()
  vi.stubGlobal('EventSource', MockEventSource)
})
afterEach(() => vi.unstubAllGlobals())

describe('events timeline', () => {
  it('reads the newest events first with order=desc and opens the stream at the head', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': eventPageDesc([eventRecord(9), eventRecord(8), eventRecord(7)]),
    })
    renderApp('/ui/events')
    const table = await screen.findByRole('table', { name: 'Events, newest first' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((r) => within(r).getAllByRole('cell')[0]?.textContent)).toEqual(['9', '8', '7'])
    const q = api.calls.find((c) => c.url.pathname === '/v1/events')?.url.searchParams
    expect(q?.get('order')).toBe('desc')
    expect(q?.has('before')).toBe(false)
    expect(q?.has('after')).toBe(false)
    await vi.waitFor(() => expect(MockEventSource.instances).toHaveLength(1))
    expect(MockEventSource.last?.url).toBe('/v1/events/stream?after=9')
    expect(api.count('/v1/events')).toBe(1)
    expect(screen.queryByRole('button', { name: 'Show older events' })).not.toBeInTheDocument()
  })

  it('loads older events with `before` when the user asks', async () => {
    const full = Array.from({ length: 200 }, (_, i) => eventRecord(400 - i))
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': ({ url }) =>
        url.searchParams.get('before') === '201' ? eventPageDesc([eventRecord(200), eventRecord(199)]) : eventPageDesc(full, true),
    })
    renderApp('/ui/events')
    const table = await screen.findByRole('table', { name: 'Events, newest first' })
    expect(within(table).getAllByRole('row')).toHaveLength(201)
    expect(api.count('/v1/events')).toBe(1) // a full page: nothing more is fetched by itself
    await userEvent.click(screen.getByRole('button', { name: 'Show older events' }))
    await vi.waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(203))
    const second = api.calls.filter((c) => c.url.pathname === '/v1/events')[1]?.url.searchParams
    expect(second?.get('order')).toBe('desc')
    expect(second?.get('before')).toBe('201')
    expect(screen.queryByRole('button', { name: 'Show older events' })).not.toBeInTheDocument()
    expect(MockEventSource.last?.url).toBe('/v1/events/stream?after=400')
  })

  it('fetches a few older pages by itself while the filters match too little, then waits', async () => {
    let before = 1000
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': () => {
        before -= 100
        return eventPageDesc([], true, { head: 1000, next_before: before })
      },
    })
    renderApp('/ui/events')
    const older = await screen.findByRole('button', { name: 'Show older events' })
    await vi.waitFor(() => expect(api.count('/v1/events')).toBe(5))
    expect(screen.queryByText('No events yet')).not.toBeInTheDocument()
    expect(screen.getByText(/No event matched among the newest events/)).toBeInTheDocument()
    const befores = api.calls.filter((c) => c.url.pathname === '/v1/events').map((c) => c.url.searchParams.get('before'))
    expect(befores).toEqual([null, '900', '800', '700', '600'])
    await vi.waitFor(() => expect(MockEventSource.last?.url).toBe('/v1/events/stream?after=1000'))
    await userEvent.click(older)
    await vi.waitFor(() => expect(api.count('/v1/events')).toBe(6))
  })

  it('does not send an invalid watch or source filter', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/events': eventPageDesc([eventRecord(1)]),
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
      '/v1/events': eventPageDesc([eventRecord(1)]),
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
    mockApi({ '/v1/me': me(), '/v1/events': eventPageDesc([]) })
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
      '/v1/events': eventPageDesc([eventRecord(2), eventRecord(1)]),
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
    mockApi({ '/v1/me': me(), '/v1/events': eventPageDesc([]) })
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

  const ago = (min: number) => new Date(Date.now() - min * 60_000).toISOString()

  it('punches one hole per release on the tape and shows the selected one as a bulletin', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/events': eventPageDesc([
        eventRecord(3, { time: ago(1) }),
        eventRecord(2, { time: ago(120) }),
        eventRecord(1, { time: ago(300), type: 'dev.angelia.release.prerelease.v1' }),
      ]),
    })
    renderApp('/ui/events')
    const tape = await screen.findByRole('listbox', { name: /Releases of the last 7 days/ })
    const holes = within(tape).getAllByRole('option')
    expect(holes).toHaveLength(3)
    expect(holes[0]).toHaveAccessibleName(/requests 1\.1\.0, prerelease/)
    expect(holes[2]).toHaveAttribute('aria-selected', 'true') // the newest
    const bulletin = screen.getByRole('region', { name: 'Selected release' })
    expect(bulletin).toHaveTextContent('1.3.0')
    expect(within(bulletin).getByLabelText('CloudEvent JSON')).toHaveTextContent('pypi:requests@1.3.0')

    holes[2]!.focus()
    await userEvent.keyboard('{ArrowLeft}')
    expect(within(tape).getAllByRole('option')[1]).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('region', { name: 'Selected release' })).toHaveTextContent('1.2.0')

    await userEvent.click(screen.getByRole('button', { name: 'Show event 3 on the tape' }))
    expect(screen.getByRole('region', { name: 'Selected release' })).toHaveTextContent('1.3.0')
  })

  it('says when the range is quiet and names the newest release', async () => {
    const old = new Date(Date.now() - 40 * 24 * 3600_000).toISOString()
    mockApi({ '/v1/me': me(), '/v1/events': eventPageDesc([eventRecord(1, { time: old })]) })
    renderApp('/ui/events')
    expect(await screen.findByText('No releases in the last 7 days')).toBeInTheDocument()
    expect(screen.getByText(/The newest one, requests 1\.1\.0/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '30 days' }))
    expect(screen.getByText('No releases in the last 30 days')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Selected release' })).not.toBeInTheDocument()
  })
})
