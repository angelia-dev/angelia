import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { delivery, eventPage, eventRecord, me, page, watch } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

describe('overview', () => {
  it('computes counts from the list calls and lists latest events and failing watches', async () => {
    const old = new Date(Date.now() - 3 * 24 * 3600_000).toISOString()
    mockApi({
      '/v1/me': me(),
      '/v1/watches': page([watch('a'), watch('b', {}, { error_count: 2 }), watch('c', {}, { error_count: 1 })]),
      '/v1/events': eventPage([eventRecord(1, { time: old }), eventRecord(2), eventRecord(3)]),
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
      '/v1/events': eventPage([]),
      '/v1/deliveries': page([]),
    })
    renderApp()
    expect(await screen.findByText('3+', { selector: 'a' })).toBeInTheDocument()
    expect(api.count('/v1/watches')).toBe(3)
  })

  it('shows n/a instead of inexact numbers when the event walk is capped', async () => {
    let next = 0
    const api = mockApi({
      '/v1/me': me(),
      '/v1/watches': page([]),
      '/v1/events': () => eventPage([eventRecord(++next)], true),
      '/v1/deliveries': page([]),
    })
    renderApp()
    const counts = await screen.findByRole('region', { name: 'Counts' })
    const card = within(counts).getByText('Events in the last 24 hours').closest('div')?.parentElement
    await vi.waitFor(() => expect(card).toHaveTextContent('n/a'))
    expect(card).toHaveTextContent('More than 1000 events retained; see Events.')
    expect(screen.queryByRole('table', { name: 'Latest events' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open Events' })).toHaveAttribute('href', '/ui/events')
    expect(api.count('/v1/events')).toBe(5)
  })

  it('walks the events at most once a minute', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const api = mockApi({
        '/v1/me': me(),
        '/v1/watches': page([]),
        '/v1/events': eventPage([eventRecord(1)]),
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
        '/v1/events': eventPage([]),
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
      '/v1/events': eventPage([]),
      '/v1/deliveries': page([]),
    })
    renderApp()
    const alerts = await screen.findAllByRole('alert')
    expect(alerts[0]).toHaveTextContent('Service unavailable')
    expect(alerts[0]).toHaveTextContent('database is starting')
    expect(await screen.findByText('No events yet')).toBeInTheDocument()
  })
})
