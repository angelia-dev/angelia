import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { eventPage, me, page } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

const providers = [{ name: 'github', label: 'GitHub' }]

const empty = {
  '/v1/watches': page([]),
  '/v1/events': eventPage([]),
  '/v1/deliveries': page([]),
}

describe('app shell', () => {
  it('shows the sign-in page on 401', async () => {
    mockApi({ '/v1/me': problem(401, 'Unauthorized', 'sign in first'), '/ui/auth/providers': providers })
    renderApp()
    const link = await screen.findByRole('link', { name: 'Sign in with GitHub' })
    expect(link).toHaveAttribute('href', '/ui/auth/login/github')
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument()
  })

  it('shows one sign-in button per provider, in the server order', async () => {
    const api = mockApi({
      '/v1/me': problem(401, 'Unauthorized'),
      '/ui/auth/providers': [
        { name: 'github', label: 'GitHub' },
        { name: 'corp-sso', label: 'Company SSO' },
        { name: 'Bad Name', label: '<b>x</b>' }, // not a provider name: dropped
        { name: 'google' }, // no label: the name
      ],
    })
    renderApp()
    await screen.findByRole('link', { name: 'Sign in with GitHub' })
    const links = screen.getAllByRole('link')
    expect(links.map((l) => [l.textContent, l.getAttribute('href')])).toEqual([
      ['Sign in with GitHub', '/ui/auth/login/github'],
      ['Sign in with Company SSO', '/ui/auth/login/corp-sso'],
      ['Sign in with google', '/ui/auth/login/google'],
    ])
    expect(api.calls.find((c) => c.url.pathname === '/ui/auth/providers')?.method).toBe('GET')
  })

  it('offers a retry when the providers cannot be loaded', async () => {
    let ok = false
    mockApi({
      '/v1/me': problem(401, 'Unauthorized'),
      '/ui/auth/providers': () => (ok ? providers : new Response('nope', { status: 500 })),
    })
    renderApp()
    const retry = await screen.findByRole('button', { name: /retry/i }, { timeout: 3000 })
    expect(screen.queryByRole('link', { name: /Sign in/ })).not.toBeInTheDocument()
    ok = true
    await userEvent.click(retry)
    expect(await screen.findByRole('link', { name: 'Sign in with GitHub' })).toBeInTheDocument()
  })

  it('shows avatar, login, role and the full navigation to an admin', async () => {
    mockApi({ '/v1/me': me(), ...empty })
    renderApp('/ui/watches')
    expect(await screen.findByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    for (const label of ['Overview', 'Watches', 'Events', 'Deliveries', 'Sinks', 'Subscriptions', 'Consumers', 'Plugins', 'Admin']) {
      expect(screen.getByRole('link', { name: label })).toBeInTheDocument()
    }
    const menu = screen.getByTestId('user-menu')
    expect(menu).toHaveTextContent('octocat')
    expect(screen.getByLabelText('Role: admin')).toBeInTheDocument()
    expect(menu.querySelector('img')).toHaveAttribute('src', 'https://avatars.example/1.png')
  })

  it('hides the admin entry from a viewer and refuses the admin route', async () => {
    mockApi({ '/v1/me': me({ role: 'viewer', scope: 'read' }), ...empty })
    renderApp('/ui/admin')
    expect(await screen.findByRole('alert')).toHaveTextContent('administrators')
    expect(screen.queryByRole('link', { name: 'Admin' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('Role: viewer')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Watches' })).toBeInTheDocument()
  })

  it('signs out with a CSRF-marked POST and returns to the sign-in page', async () => {
    let signedOut = false
    const api = mockApi({
      '/v1/me': () => (signedOut ? problem(401, 'Unauthorized') : me()),
      '/ui/auth/providers': providers,
      'POST /ui/auth/logout': () => {
        signedOut = true
        return new Response(null, { status: 204 })
      },
      ...empty,
    })
    renderApp('/ui/watches')
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('link', { name: 'Sign in with GitHub' })).toBeInTheDocument()
    const logout = api.calls.find((c) => c.method === 'POST')
    expect(logout?.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(logout?.url.pathname).toBe('/ui/auth/logout')
  })

  it('keeps the shell and shows the error when sign out fails', async () => {
    mockApi({ '/v1/me': me(), 'POST /ui/auth/logout': new Response(null, { status: 500 }), ...empty })
    renderApp('/ui/watches')
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }))
    await waitFor(() => expect(screen.getAllByRole('alert')[0]).toHaveTextContent('sign out failed'))
  })

  it('reports an API error that is not a 401', async () => {
    mockApi({ '/v1/me': problem(500, 'Internal error', 'boom') })
    renderApp()
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not reach the Angelia API: boom')
  })
})
