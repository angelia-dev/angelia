import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { delivery, me, page, subscription } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

const dead = delivery('D0000001', { status: 'dead', attempts: 8, last_error: 'HTTP 500' })
const base = (role: 'viewer' | 'editor' | 'admin') => ({
  '/v1/me': me({ role, scope: role === 'viewer' ? 'read' : role === 'editor' ? 'write' : 'admin' }),
  '/v1/subscriptions': page([subscription('to-slack', 'slack-main')]),
  '/v1/deliveries': page([dead, delivery('D0000002', { status: 'pending' })]),
  '/v1/deliveries/D0000001': dead,
})

describe('replay one delivery', () => {
  it('is hidden from viewers', async () => {
    mockApi(base('viewer'))
    renderApp('/ui/deliveries')
    await screen.findByRole('table', { name: 'Deliveries' })
    expect(screen.queryByRole('button', { name: /Replay/ })).not.toBeInTheDocument()
  })

  it('confirms, sends the CSRF header and shows the queued delivery; only replayable rows have the button', async () => {
    const api = mockApi({
      ...base('editor'),
      'POST /v1/deliveries/D0000001/replay': { ...dead, status: 'pending', attempts: 8 },
    })
    renderApp('/ui/deliveries')
    await screen.findByRole('table', { name: 'Deliveries' })
    expect(screen.getAllByRole('button', { name: /^Replay delivery/ })).toHaveLength(1)
    await userEvent.click(screen.getByRole('button', { name: 'Replay delivery D0000001' }))
    const dialog = screen.getByRole('dialog', { name: 'Replay delivery' })
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Replay' }))
    expect(await within(dialog).findByText('The delivery is queued again.')).toBeInTheDocument()
    const post = api.calls.find((c) => c.method === 'POST')
    expect(post?.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(within(dialog).getByRole('link', { name: 'Delivery D0000001' })).toBeInTheDocument()
    await vi.waitFor(() => expect(api.count('/v1/deliveries')).toBeGreaterThan(1))
  })

  it('cancel sends nothing', async () => {
    const api = mockApi(base('admin'))
    renderApp('/ui/deliveries/D0000001')
    await userEvent.click(await screen.findByRole('button', { name: 'Replay' }))
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('shows the 409 problem detail', async () => {
    mockApi({
      ...base('editor'),
      'POST /v1/deliveries/D0000001/replay': problem(409, 'Not replayable', 'delivery D0000001 is in_flight'),
    })
    renderApp('/ui/deliveries/D0000001')
    await userEvent.click(await screen.findByRole('button', { name: 'Replay' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Replay' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('delivery D0000001 is in_flight')
  })
})

describe('replay range', () => {
  it('is hidden from viewers', async () => {
    mockApi(base('viewer'))
    renderApp('/ui/deliveries')
    await screen.findByRole('table', { name: 'Deliveries' })
    expect(screen.queryByRole('button', { name: 'Replay range' })).not.toBeInTheDocument()
  })

  it('counts with a dry run, names the filter, then replays', async () => {
    const bodies: Record<string, unknown>[] = []
    const api = mockApi({
      ...base('editor'),
      'POST /v1/replays': () => ({ replayed: 3, dry_run: bodies.length === 0, deliveries: [], truncated: false }),
    })
    const stubbed = globalThis.fetch
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const req = new Request(typeof input === 'string' && input.startsWith('/') ? location.origin + input : input, init)
      if (req.method === 'POST') bodies.push((await req.clone().json()) as Record<string, unknown>)
      return stubbed(req)
    })
    renderApp('/ui/deliveries')
    await userEvent.click(await screen.findByRole('button', { name: 'Replay range' }))
    const dialog = screen.getByRole('dialog', { name: 'Replay range' })
    expect(within(dialog).getByRole('button', { name: 'Count deliveries' })).toBeDisabled()
    await userEvent.type(within(dialog).getByRole('textbox', { name: 'Event id' }), 'EVT1')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Count deliveries' }))
    expect(await within(dialog).findByText('3 deliveries would be replayed.')).toBeInTheDocument()
    expect(dialog).toHaveTextContent('event EVT1, status dead')
    expect(bodies[0]).toMatchObject({ event: 'EVT1', status: ['dead'], dry_run: true })
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Replay 3' }))
    expect(await within(dialog).findByText('3 deliveries queued again.')).toBeInTheDocument()
    expect(bodies[1]).toMatchObject({ event: 'EVT1', dry_run: false })
  })

  it('shows a validation problem with its detail', async () => {
    mockApi({
      ...base('admin'),
      'POST /v1/replays': problem(422, 'Validation failed', 'since must be before until'),
    })
    renderApp('/ui/deliveries')
    await userEvent.click(await screen.findByRole('button', { name: 'Replay range' }))
    const dialog = screen.getByRole('dialog')
    await userEvent.click(within(dialog).getByLabelText('Time range'))
    await userEvent.type(within(dialog).getByLabelText('Subscription (name or id)'), 'to-slack')
    await userEvent.type(within(dialog).getByLabelText(/Event time from/), '2026-01-02T00:00')
    await userEvent.type(within(dialog).getByLabelText(/Event time until/), '2026-01-01T00:00')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Count deliveries' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('since must be before until')
  })
})
