import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { me, page, sink, subscription } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

describe('read-only lists', () => {
  it('lists sinks with the redacted URL and masked fields', async () => {
    mockApi({ '/v1/me': me({ role: 'viewer', scope: 'read' }), '/v1/sinks': page([sink('slack-main')]) })
    renderApp('/ui/sinks')
    const table = await screen.findByRole('table', { name: 'Sinks' })
    expect(table).toHaveTextContent('slack-main')
    expect(table).toHaveTextContent('https://hooks.example/***')
    expect(table).toHaveTextContent('URL redacted')
    expect(table).toHaveTextContent('secret_ref')
    expect(screen.queryByRole('button', { name: /edit|delete|add/i })).not.toBeInTheDocument()
  })

  it('lists subscriptions', async () => {
    mockApi({ '/v1/me': me(), '/v1/subscriptions': page([subscription('to-slack', 'slack-main')]) })
    renderApp('/ui/subscriptions')
    const table = await screen.findByRole('table', { name: 'Subscriptions' })
    expect(within(table).getByText('to-slack')).toBeInTheDocument()
    expect(table).toHaveTextContent('slack-main')
    expect(table).toHaveTextContent('release.published')
  })

  it('lists consumers', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/consumers': page([
        {
          name: 'release-bot',
          id: 'C1',
          filter: { watch: [], type: [], source: ['pypi'] },
          acked_seq: 42,
          acked_at: null,
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString(),
        },
      ]),
    })
    renderApp('/ui/consumers')
    const table = await screen.findByRole('table', { name: 'Consumers' })
    expect(table).toHaveTextContent('release-bot')
    expect(table).toHaveTextContent('42')
    expect(table).toHaveTextContent('pypi')
  })

  it('lists plugins', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/plugins': page([
        {
          kind: 'internal-registry',
          interface: 'source',
          runtime: 'wasm',
          version: '1.2.0',
          abi: 'angelia-plugin-v1',
          config_schema: null,
          allowed_hosts: [{ host: 'registry.example', origin: 'manifest', credential_ports: [] }],
        },
      ]),
    })
    renderApp('/ui/plugins')
    const table = await screen.findByRole('table', { name: 'Plugins' })
    expect(table).toHaveTextContent('internal-registry')
    expect(table).toHaveTextContent('registry.example')
  })

  it.each([
    ['/ui/sinks', 'No sinks'],
    ['/ui/subscriptions', 'No subscriptions'],
    ['/ui/consumers', 'No consumers'],
    ['/ui/plugins', 'No plugins installed'],
  ])('%s shows an empty state', async (path, text) => {
    mockApi({
      '/v1/me': me(),
      '/v1/sinks': page([]),
      '/v1/subscriptions': page([]),
      '/v1/consumers': page([]),
      '/v1/plugins': page([]),
    })
    renderApp(path)
    expect(await screen.findByText(text)).toBeInTheDocument()
  })

  it('shows a forbidden problem with its detail', async () => {
    mockApi({ '/v1/me': me(), '/v1/sinks': problem(403, 'Forbidden', 'scope read is not enough') })
    renderApp('/ui/sinks')
    expect(await screen.findByRole('alert')).toHaveTextContent('scope read is not enough')
  })
})
