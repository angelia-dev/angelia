import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { components } from '@/api/schema'
import { me, page } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

type Token = components['schemas']['Token']
type Session = components['schemas']['Session']

const token = (id: string, name: string, over: Partial<Token> = {}): Token => ({
  id,
  name,
  scope: 'read',
  created_at: new Date(Date.now() - 3_600_000).toISOString(),
  created_by: 'github:1',
  expires_at: null,
  last_used_at: null,
  revoked_at: null,
  revoked_by: null,
  ...over,
})

const session = (id: string, login: string, subject: string): Session => ({
  id,
  user: { provider: 'github', subject, login, name: null, avatar_url: null },
  user_agent: `Browser of ${login}`,
  created_at: new Date(Date.now() - 7_200_000).toISOString(),
  last_seen_at: new Date(Date.now() - 60_000).toISOString(),
  expires_at: new Date(Date.now() + 86_400_000).toISOString(),
  revoked_at: null,
  revoked_by: null,
})

const SECRET = 'ang_01JBX2A7C8D9E0F1G2H3J4K5M6_secretsecretsecret'

describe('admin visibility', () => {
  it('hides the Admin nav item from editors and shows forbidden on the URL', async () => {
    mockApi({ '/v1/me': me({ role: 'editor', scope: 'write' }) })
    renderApp('/ui/admin')
    expect(await screen.findByRole('alert')).toHaveTextContent('This screen is for administrators.')
    expect(screen.queryByRole('link', { name: 'Admin' })).not.toBeInTheDocument()
  })

  it('shows the Admin nav item to admins', async () => {
    mockApi({ '/v1/me': me(), '/v1/tokens': page([]) })
    renderApp('/ui/admin')
    expect(await screen.findByRole('link', { name: 'Admin' })).toBeInTheDocument()
    expect(await screen.findByRole('tab', { name: 'API tokens', selected: true })).toBeInTheDocument()
  })
})

describe('tokens', () => {
  it('lists tokens and the include-revoked toggle changes the query', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/tokens': ({ url }) =>
        page(
          url.searchParams.get('include_revoked') === 'true'
            ? [token('T1', 'ci'), token('T2', 'old', { revoked_at: new Date().toISOString() })]
            : [token('T1', 'ci', { scope: 'write' })],
        ),
    })
    renderApp('/ui/admin')
    const table = await screen.findByRole('table', { name: 'API tokens' })
    expect(table).toHaveTextContent('ci')
    expect(table).toHaveTextContent('write')
    expect(within(table).queryByText('old')).not.toBeInTheDocument()
    await userEvent.click(screen.getByLabelText('Include revoked'))
    expect(await screen.findByText('old')).toBeInTheDocument()
    expect(api.calls.at(-1)?.url.searchParams.get('include_revoked')).toBe('true')
    expect(screen.getAllByRole('button', { name: /^Revoke token/ })).toHaveLength(1)
  })

  it('creates a token, shows it once and drops it', async () => {
    let created = false
    const api = mockApi({
      '/v1/me': me(),
      '/v1/tokens': () => page(created ? [token('T9', 'deploy')] : []),
      'POST /v1/tokens': () => {
        created = true
        return new Response(JSON.stringify({ ...token('T9', 'deploy', { scope: 'write' }), token: SECRET }), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        })
      },
    })
    const writeText = vi.fn(() => Promise.resolve())
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    renderApp('/ui/admin')
    await screen.findByText('No API tokens')
    await userEvent.click(screen.getByRole('button', { name: 'Create token' }))
    const dialog = screen.getByRole('dialog', { name: 'Create API token' })
    await userEvent.type(within(dialog).getByLabelText('Name'), 'deploy')
    await userEvent.selectOptions(within(dialog).getByLabelText('Scope'), 'write')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create' }))

    const shown = await screen.findByRole('dialog', { name: 'Token created' })
    expect(within(shown).getByTestId('token-value')).toHaveTextContent(SECRET)
    expect(shown).toHaveTextContent('shown only once')
    const post = api.calls.find((c) => c.method === 'POST')
    expect(post?.headers.get('X-Angelia-CSRF')).toBe('1')
    await userEvent.click(within(shown).getByRole('button', { name: 'Copy' }))
    expect(writeText).toHaveBeenCalledWith(SECRET)
    expect(await within(shown).findByText('Copied.')).toBeInTheDocument()

    await userEvent.click(within(shown).getByRole('button', { name: 'Done' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body).not.toHaveTextContent(SECRET)
    expect(await screen.findByText('deploy')).toBeInTheDocument()
    // the list never carried the secret, and nothing was stored in the browser
    expect(document.body).not.toHaveTextContent(SECRET)
    expect(JSON.stringify({ ...localStorage })).not.toContain('ang_')
    expect(JSON.stringify({ ...sessionStorage })).not.toContain('ang_')
    // reopening the dialog starts empty
    await userEvent.click(screen.getByRole('button', { name: 'Create token' }))
    expect(screen.queryByTestId('token-value')).not.toBeInTheDocument()
  })

  it('shows the problem when create fails', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/tokens': page([]),
      'POST /v1/tokens': problem(422, 'Validation failed', 'name "ci" is already used by an active token'),
    })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('button', { name: 'Create token' }))
    const dialog = screen.getByRole('dialog')
    await userEvent.type(within(dialog).getByLabelText('Name'), 'ci')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('already used by an active token')
  })

  it('revokes after confirmation and refreshes the list', async () => {
    let revoked = false
    const api = mockApi({
      '/v1/me': me(),
      '/v1/tokens': () => page(revoked ? [] : [token('T1', 'ci')]),
      'DELETE /v1/tokens/T1': () => {
        revoked = true
        return new Response(null, { status: 204 })
      },
    })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke token ci' }))
    const dialog = screen.getByRole('dialog', { name: 'Revoke token' })
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }))
    expect(await screen.findByText('No API tokens')).toBeInTheDocument()
    expect(api.calls.find((c) => c.method === 'DELETE')?.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('cancelling the revoke sends nothing', async () => {
    const api = mockApi({ '/v1/me': me(), '/v1/tokens': page([token('T1', 'ci')]) })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke token ci' }))
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false)
  })
})

describe('sessions', () => {
  const sessions = page([session('S1', 'octocat', '1'), session('S2', 'someone', '2')])

  it('lists sessions and marks those of the signed-in user', async () => {
    mockApi({ '/v1/me': me(), '/v1/sessions': sessions })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('tab', { name: 'Sessions' }))
    const table = await screen.findByRole('table', { name: 'Sessions' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('octocat')
    expect(rows[0]).toHaveTextContent('you')
    expect(rows[0]).toHaveTextContent('Browser of octocat')
    expect(rows[1]).toHaveTextContent('someone')
    expect(rows[1]).not.toHaveTextContent('you')
  })

  it('revokes another user\'s session and stays signed in', async () => {
    let gone = false
    const api = mockApi({
      '/v1/me': me(),
      '/v1/sessions': () => page(gone ? [session('S1', 'octocat', '1')] : [session('S1', 'octocat', '1'), session('S2', 'someone', '2')]),
      'DELETE /v1/sessions/S2': () => {
        gone = true
        return new Response(null, { status: 204 })
      },
    })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('tab', { name: 'Sessions' }))
    await userEvent.click(await screen.findByRole('button', { name: /Revoke session of someone/ }))
    const dialog = screen.getByRole('dialog', { name: 'Revoke session' })
    expect(dialog).not.toHaveTextContent('signed out.')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }))
    await vi.waitFor(() => expect(screen.queryByText('someone')).not.toBeInTheDocument())
    expect(api.calls.find((c) => c.method === 'DELETE')?.url.pathname).toBe('/v1/sessions/S2')
    expect(screen.queryByRole('button', { name: 'Sign in with GitHub' })).not.toBeInTheDocument()
  })

  it('revoking your own session signs you out', async () => {
    let revoked = false
    mockApi({
      '/v1/me': () => (revoked ? problem(401, 'Unauthorized', 'session revoked') : me()),
      '/v1/sessions': page([session('S1', 'octocat', '1')]),
      'DELETE /v1/sessions/S1': () => {
        revoked = true
        return new Response(null, { status: 204 })
      },
    })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('tab', { name: 'Sessions' }))
    await userEvent.click(await screen.findByRole('button', { name: /Revoke session of octocat/ }))
    expect(screen.getByRole('dialog')).toHaveTextContent('you will be signed out')
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Revoke' }))
    expect(await screen.findByRole('link', { name: /sign in/i })).toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: 'Sessions' })).not.toBeInTheDocument()
  })

  it('shows the problem when revoke fails', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/sessions': page([session('S2', 'someone', '2')]),
      'DELETE /v1/sessions/S2': problem(404, 'Not found', 'no session S2'),
    })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('tab', { name: 'Sessions' }))
    await userEvent.click(await screen.findByRole('button', { name: /Revoke session of someone/ }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Revoke' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('no session S2')
  })
})

describe('access', () => {
  it('explains where ui.access lives and shows the signed-in role', async () => {
    mockApi({ '/v1/me': me() })
    renderApp('/ui/admin')
    await userEvent.click(await screen.findByRole('tab', { name: 'Access' }))
    const panel = screen.getByRole('region', { name: 'Access' })
    expect(panel).toHaveTextContent('ui.access')
    expect(panel).toHaveTextContent('octocat')
    expect(panel).toHaveTextContent('admin')
    expect(within(panel).getByRole('link', { name: 'docs/admin-ui.md' })).toHaveAttribute('href', expect.stringContaining('docs/admin-ui.md'))
  })
})
