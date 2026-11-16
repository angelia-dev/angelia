import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { delivery, me, page, sink, subscription } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

const base = {
  '/v1/me': me(),
  '/v1/subscriptions': page([subscription('to-slack', 'slack-main')]),
  '/v1/sinks': page([sink('slack-main'), sink('email')]),
}

describe('deliveries list', () => {
  it('shows status as text, the delivery\'s sink, attempts and the last error', async () => {
    mockApi({
      ...base,
      '/v1/deliveries': page([
        delivery('D0000001', { status: 'dead', attempts: 8, last_error: 'HTTP 500', next_attempt_at: null, sink: 'slack-main' }),
        delivery('D0000002', { status: 'delivered', attempts: 1 }),
        delivery('D0000003', { status: 'pending', subscription_name: null, sink: null, subscription_id: 'GONE' }),
      ]),
    })
    renderApp('/ui/deliveries')
    const table = await screen.findByRole('table', { name: 'Deliveries' })
    await within(table).findAllByText('slack-main')
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('dead letter')
    expect(rows[0]).toHaveTextContent('slack-main')
    expect(rows[0]).toHaveTextContent('to-slack')
    expect(rows[0]).toHaveTextContent('8')
    expect(rows[0]).toHaveTextContent('HTTP 500')
    expect(rows[1]).toHaveTextContent('delivered')
    expect(rows[2]).toHaveTextContent('(deleted)')
    expect(rows[2]).toHaveTextContent('in 1 minute')
  })

  it('filters by status, including dead letters', async () => {
    const api = mockApi({ ...base, '/v1/deliveries': page([delivery('D1')]) })
    renderApp('/ui/deliveries')
    await screen.findByRole('table', { name: 'Deliveries' })
    expect(api.calls.find((c) => c.url.pathname === '/v1/deliveries')?.url.searchParams.getAll('status')).toEqual(['dead'])
    await userEvent.selectOptions(screen.getByLabelText('Status'), 'pending')
    await vi.waitFor(() => expect(api.calls.at(-1)?.url.searchParams.getAll('status')).toEqual(['pending']))
  })

  it('shows the dead-letter empty state', async () => {
    mockApi({ ...base, '/v1/deliveries': () => page([]) })
    renderApp('/ui/deliveries')
    expect(await screen.findByText('No dead letters')).toBeInTheDocument()
  })

  it('pages with the cursor', async () => {
    const api = mockApi({
      ...base,
      '/v1/deliveries': ({ url }) => (url.searchParams.get('cursor') ? page([delivery('D0000002')]) : page([delivery('D0000001')], 'C2')),
    })
    renderApp('/ui/deliveries')
    await screen.findByText('D0000001'.slice(-8))
    await userEvent.click(screen.getByRole('button', { name: 'Load more' }))
    expect(await screen.findByText('D0000002'.slice(-8))).toBeInTheDocument()
    expect(api.calls.at(-1)?.url.searchParams.get('cursor')).toBe('C2')
  })

  it('shows the problem', async () => {
    mockApi({ ...base, '/v1/deliveries': problem(500, 'Internal error', 'store unavailable') })
    renderApp('/ui/deliveries')
    expect(await screen.findByRole('alert')).toHaveTextContent('store unavailable')
  })
})

describe('delivery detail', () => {
  it('shows the delivery and links to its event', async () => {
    mockApi({
      ...base,
      '/v1/deliveries/D1': delivery('D1', { status: 'dead', attempts: 5, last_error: 'timeout', sink: 'slack-main' }),
    })
    renderApp('/ui/deliveries/D1')
    const table = await screen.findByRole('table', { name: 'Delivery details' })
    expect(table).toHaveTextContent('dead letter')
    expect(table).toHaveTextContent('timeout')
    expect(within(table).getByRole('link', { name: 'EVT00000000000000000000001' })).toHaveAttribute(
      'href',
      '/ui/events/EVT00000000000000000000001',
    )
    expect(await within(table).findByText('slack-main')).toBeInTheDocument()
  })

  it('shows a not-found problem', async () => {
    mockApi({ ...base, '/v1/deliveries/X': problem(404, 'Delivery not found', 'no delivery X') })
    renderApp('/ui/deliveries/X')
    expect(await screen.findByRole('alert')).toHaveTextContent('no delivery X')
  })
})

describe('server-side filters', () => {
  it('sends the sink filter to the API and keeps Load more', async () => {
    const api = mockApi({ ...base, '/v1/deliveries': page([delivery('D0000001', { sink: 'email' })], 'C2') })
    renderApp('/ui/deliveries')
    await screen.findByText('D0000001'.slice(-8))
    await userEvent.selectOptions(screen.getByLabelText('Sink'), 'email')
    await vi.waitFor(() => expect(api.calls.at(-1)?.url.searchParams.get('sink')).toBe('email'))
    expect(api.calls.at(-1)?.url.searchParams.getAll('status')).toEqual(['dead'])
    expect(screen.getByRole('button', { name: 'Load more' })).toBeInTheDocument()
  })

  it('sends a time range as RFC 3339 since/until and refuses an end before the start', async () => {
    const api = mockApi({ ...base, '/v1/deliveries': page([delivery('D0000001')]) })
    renderApp('/ui/deliveries')
    await screen.findByText('D0000001'.slice(-8))
    const from = screen.getByLabelText('Event recorded from')
    const to = screen.getByLabelText('Event recorded before')
    expect(from).toHaveAttribute('type', 'datetime-local')
    await userEvent.type(from, '2026-11-01T08:00')
    await userEvent.type(to, '2026-11-02T08:00')
    await vi.waitFor(() => {
      const q = api.calls.filter((c) => c.url.pathname === '/v1/deliveries').at(-1)?.url.searchParams
      expect(q?.get('since')).toBe(new Date('2026-11-01T08:00').toISOString())
      expect(q?.get('until')).toBe(new Date('2026-11-02T08:00').toISOString())
    })
    await userEvent.clear(to)
    await userEvent.type(to, '2026-10-01T08:00')
    expect(await screen.findByText('Must be after the start.')).toBeInTheDocument()
    expect(to).toHaveAttribute('aria-invalid', 'true')
    await vi.waitFor(() => {
      const q = api.calls.filter((c) => c.url.pathname === '/v1/deliveries').at(-1)?.url.searchParams
      expect(q?.has('until')).toBe(false)
    })
  })
})
