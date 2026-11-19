import { fireEvent, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { me, page, plugin, sink, subscription, watch } from '@/test/fixtures'
import { mockApi, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

const ha = watch('home-assistant', { managed_by: 'file', source: 'github-release', coordinates: { repo: 'home-assistant/core' }, labels: {} })
const csnet = sink('csnet-home-ci', { kind: 'github-dispatch', config: { repo: 'mmo/csnet', token_ref: 'gh' }, masked_fields: [], url_redacted: false })
const mel = sink('mel-garbage-ci', { kind: 'github-dispatch', config: { repo: 'mmo/mel', token_ref: 'gh' }, masked_fields: [], url_redacted: false })
const toCsnet = subscription('ha-to-csnet-home', 'csnet-home-ci', 'S1')
const toMel = { ...subscription('ha-to-mel-garbage', 'mel-garbage-ci', 'S2'), filter: { ...toCsnet.filter, package: 'home-assistant/core*' } }
const plugins = ({ url }: { url: URL }) =>
  url.searchParams.get('interface') === 'sink' ? page([plugin('github-dispatch', 'sink')]) : page([plugin('github-release', 'source')])

const base = (who = me()) => ({
  '/v1/me': who,
  '/v1/plugins': plugins,
  '/v1/secrets': page([]),
  '/v1/watches': page([ha]),
  '/v1/sinks': page([csnet, mel]),
  '/v1/subscriptions': page([toCsnet, toMel]),
})

describe('configure', () => {
  it('draws the patch bay with a cord per matching route', async () => {
    mockApi(base())
    renderApp('/ui/configure')
    const bay = await screen.findByTestId('patch-bay')
    for (const name of ['home-assistant', 'ha-to-csnet-home', 'ha-to-mel-garbage', 'csnet-home-ci', 'mel-garbage-ci']) {
      expect(within(bay).getByText(name)).toBeInTheDocument()
    }
    // Two watch→route cords and two route→sink cords.
    expect(bay.querySelectorAll('svg path')).toHaveLength(4)
    expect(within(bay).getByText('package home-assistant/core*')).toBeInTheDocument()
  })

  it('shows a file object as read-only with its YAML', async () => {
    mockApi(base())
    renderApp('/ui/configure')
    await userEvent.click(await screen.findByRole('button', { name: /^github-release.*home-assistant/ }))
    const drawer = screen.getByRole('complementary', { name: 'Editor: home-assistant' })
    expect(drawer).toHaveTextContent('comes from the watch file')
    await userEvent.click(within(drawer).getByRole('tab', { name: 'YAML' }))
    expect(drawer).toHaveTextContent('repo: home-assistant/core')
  })

  it('moves the watch-file objects to the UI in one adoption', async () => {
    const api = mockApi({
      ...base(),
      'POST /v1/adoptions': { dry_run: false, adopted: [] },
    })
    URL.createObjectURL = vi.fn(() => 'blob:x')
    URL.revokeObjectURL = vi.fn()
    renderApp('/ui/configure')
    expect(await screen.findByText(/5 objects come from the watch file/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Move them to the UI…' }))
    const dialog = screen.getByRole('dialog', { name: 'Manage these objects from the UI' })
    await userEvent.click(within(dialog).getByRole('button', { name: 'Move 5 objects' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'POST')).toBe(true))
    const body = JSON.parse(api.calls.find((c) => c.method === 'POST')!.body)
    expect(body.objects).toEqual([
      { kind: 'sink', name: 'csnet-home-ci' },
      { kind: 'sink', name: 'mel-garbage-ci' },
      { kind: 'watch', name: 'home-assistant' },
      { kind: 'subscription', name: 'ha-to-csnet-home' },
      { kind: 'subscription', name: 'ha-to-mel-garbage' },
    ])
    expect(URL.createObjectURL).toHaveBeenCalled()
    expect(await screen.findByRole('dialog', { name: 'Moved to the UI' })).toHaveTextContent('Before the next restart')
  })

  it('an editor cannot move file objects', async () => {
    mockApi(base(me({ role: 'editor', scope: 'write' })))
    renderApp('/ui/configure')
    expect(await screen.findByText('An admin can move them to the UI.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Move them to the UI…' })).not.toBeInTheDocument()
  })

  it('a cord from a watch starts a route prefilled from the watch', async () => {
    mockApi({ ...base(), '/v1/watches': page([{ ...ha, managed_by: 'api' }]), '/v1/sinks': page([{ ...csnet, managed_by: 'api' }]) })
    renderApp('/ui/configure')
    const jack = await screen.findByRole('button', { name: 'Route home-assistant to a sink' })
    const target = screen.getByText('csnet-home-ci').closest('[data-sink-name]')!
    document.elementFromPoint = vi.fn(() => target)
    fireEvent.pointerDown(jack, { button: 0 })
    fireEvent.pointerUp(window, { clientX: 10, clientY: 10 })
    const form = await screen.findByRole('form', { name: 'New subscription' })
    expect(within(form).getByLabelText(/^Name/)).toHaveValue('home-assistant-to-csnet-home-ci')
    expect(within(form).getByLabelText(/^Package/)).toHaveValue('home-assistant/core')
  })

  it('a viewer reads the board without edit controls', async () => {
    mockApi(base(me({ role: 'viewer', scope: 'read' })))
    renderApp('/ui/configure')
    await screen.findByTestId('patch-bay')
    expect(screen.queryByRole('button', { name: '+ Watch' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Route .* to a sink/ })).not.toBeInTheDocument()
  })
})
