import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { delivery, eventPageDesc, eventRecord, me, page, watch } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

describe('overview', () => {
  it('computes counts from the list calls and lists latest events and failing watches', async () => {
    const old = new Date(Date.now() - 3 * 24 * 3600_000).toISOString()
    mockApi({
      '/v1/me': me(),
      '/v1/watches': page([watch('a'), watch('b', {}, { error_count: 2 }), watch('c', {}, { error_count: 1 })]),
      '/v1/events': eventPageDesc([eventRecord(3), eventRecord(2), eventRecord(1, { time: old })], true),
      '/v1/deliveries': ({ url }) =>
        url.searchParams.getAll('status').includes('dead')
          ? page([delivery('D1', { status: 'dead' })])
          : page([delivery('P1'), delivery('P2')]),
    })
    renderApp()
    const counts = await screen.findByRole('region', { name: 'Counts' })
    await within(counts).findByText('3', { selector: 'a' }) // watches
    expect(within(counts).getByText('Failing watches').closest('div')?.parentElement).toHaveTextContent('2')
    expect(within(counts).getByText('Events in the last 24 hours').closest('div')?.parentElement).toHaveTextContent('2')
    expect(within(counts).getByText('Pending deliveries').closest('div')?.parentElement).toHaveTextContent('2')
    expect(within(counts).getByText('Dead letters').closest('div')?.parentElement).toHaveTextContent('1')

    const latest = await screen.findByRole('table', { name: 'Latest events' })
    const rows = within(latest).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(3)
    expect(rows[0]).toHaveTextContent('requests@1.3.0') // newest first

    const failing = screen.getByRole('table', { name: 'Failing watches' })
    expect(within(failing).getByRole('link', { name: 'b' })).toBeInTheDocument()
    expect(within(failing).queryByRole('link', { name: 'a' })).not.toBeInTheDocument()
  })

  it('labels a capped count with a plus sign and stops paging', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/watches': page([watch('a')], 'more'),
      '/v1/events': eventPageDesc([]),
      '/v1/deliveries': page([]),
    })
    renderApp()
    expect(await screen.findByText('3+', { selector: 'a' })).toBeInTheDocument()
    expect(api.count('/v1/watches')).toBe(3)
  })

  it('counts the last 24 hours exactly by reading newest first until an older event', async () => {
    const hour = (h: number) => new Date(Date.now() - h * 3600_000).toISOString()
    const api = mockApi({
      '/v1/me': me(),
      '/v1/watches': page([]),
      '/v1/events': ({ url }) => {
        const before = url.searchParams.get('before')
        if (before === null) return eventPageDesc([eventRecord(30, { time: hour(1) }), eventRecord(29, { time: hour(2) })], true)
        if (before === '29') return eventPageDesc([eventRecord(28, { time: hour(20) }), eventRecord(27, { time: hour(23) })], true)
        return eventPageDesc([eventRecord(26, { time: hour(23.5) }), eventRecord(25, { time: hour(30) })], true)
      },
      '/v1/deliveries': page([]),
    })
    renderApp()
    const counts = await screen.findByRole('region', { name: 'Counts' })
    const card = within(counts).getByText('Events in the last 24 hours').closest('div')?.parentElement
    await vi.waitFor(() => expect(card).toHaveTextContent(/^Events in the last 24 hours5$/))
    const calls = api.calls.filter((c) => c.url.pathname === '/v1/events').map((c) => c.url.searchParams)
    expect(calls.map((q) => q.get('order'))).toEqual(['desc', 'desc', 'desc'])
    expect(calls.map((q) => q.get('before'))).toEqual([null, '29', '27'])
    expect(calls.every((q) => !q.has('after'))).toBe(true)
    const latest = await screen.findByRole('table', { name: 'Latest events' })
    expect(within(latest).getAllByRole('row').slice(1)).toHaveLength(2) // the first page's newest
  })

  it('walks the events at most once a minute', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const api = mockApi({
        '/v1/me': me(),
        '/v1/watches': page([]),
        '/v1/events': eventPageDesc([eventRecord(1)]),
        '/v1/deliveries': page([]),
      })
      renderApp()
      await screen.findByRole('table', { name: 'Latest events' })
      expect(api.count('/v1/events')).toBe(1)
      await vi.advanceTimersByTimeAsync(35_000)
      expect(api.count('/v1/events')).toBe(1)
      await vi.advanceTimersByTimeAsync(30_000)
      expect(api.count('/v1/events')).toBe(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('refetches every 30 seconds', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const api = mockApi({
        '/v1/me': me(),
        '/v1/watches': page([]),
        '/v1/events': eventPageDesc([]),
        '/v1/deliveries': page([]),
      })
      renderApp()
      await screen.findByText('No failing watches')
      expect(api.count('/v1/watches')).toBe(1)
      await vi.advanceTimersByTimeAsync(30_500)
      expect(api.count('/v1/watches')).toBe(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('shows the problem detail when a call fails, without hiding the rest', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/watches': problem(503, 'Service unavailable', 'database is starting'),
      '/v1/events': eventPageDesc([]),
      '/v1/deliveries': page([]),
    })
    renderApp()
    const alerts = await screen.findAllByRole('alert')
    expect(alerts[0]).toHaveTextContent('Service unavailable')
    expect(alerts[0]).toHaveTextContent('database is starting')
    expect(await screen.findByText('No events yet')).toBeInTheDocument()
  })

  it('puts each watch in a cubby with its latest release, and switches to the compact rack', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/watches': page([watch('requests'), watch('other', {}, { error_count: 1 })]),
      '/v1/events': eventPageDesc([eventRecord(5), eventRecord(4)]),
      '/v1/deliveries': ({ url }) =>
        url.searchParams.getAll('status').includes('dead') ? page([delivery('D1', { status: 'dead', event_id: 'EVT00000000000000000000005' })]) : page([]),
    })
    renderApp()
    const cubby = await screen.findByRole('article', { name: 'Watch requests' })
    await within(cubby).findByText('1.5.0')
    expect(within(cubby).getByText('dead letter')).toBeInTheDocument()
    expect(within(cubby).getByRole('meter', { name: 'Time to the next check of requests' })).toBeInTheDocument()
    const other = screen.getByRole('article', { name: 'Watch other' })
    expect(other).toHaveTextContent('No new release since the baseline.')
    expect(other).toHaveTextContent('1 error')
    expect(screen.getByRole('status')).toHaveTextContent('1 dead letter.')

    await userEvent.click(screen.getByRole('button', { name: 'compact' }))
    const group = screen.getByRole('region', { name: 'pypi watches' })
    expect(within(group).getAllByRole('listitem')).toHaveLength(2)
    expect(within(group).getByLabelText('failing')).toBeInTheDocument()
    expect(screen.queryByRole('article')).not.toBeInTheDocument()
  })
})
