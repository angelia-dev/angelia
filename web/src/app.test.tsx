import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { eventPage, me, page } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

const empty = {
  '/v1/watches': page([]),
  '/v1/events': eventPage([]),
  '/v1/deliveries': page([]),
}

describe('app shell', () => {
  it('shows the sign-in page on 401', async () => {
    mockApi({ '/v1/me': problem(401, 'Unauthorized', 'sign in first') })
    renderApp()
    const link = await screen.findByRole('link', { name: 'Sign in with GitHub' })
    expect(link).toHaveAttribute('href', '/ui/auth/login/github')
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument()
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
