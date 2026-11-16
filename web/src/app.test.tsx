import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory } from '@tanstack/react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createAppRouter } from './router'

const me = {
  principal_id: 'github:1',
  name: 'octocat',
  scope: 'admin',
  kind: 'session',
  role: 'admin',
  user: { provider: 'github', subject: '1', login: 'octocat' },
}

function mockMe(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }))),
  )
}

function renderApp(path = '/ui/') {
  const router = createAppRouter(createMemoryHistory({ initialEntries: [path] }))
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('app shell', () => {
  it('shows the sign-in page on 401', async () => {
    mockMe(401, { type: 'unauthorized' })
    renderApp()
    const link = await screen.findByRole('link', { name: 'Sign in with GitHub' })
    expect(link).toHaveAttribute('href', '/ui/auth/login/github')
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument()
  })

  it('shows the layout and navigation when signed in', async () => {
    mockMe(200, me)
    renderApp('/ui/watches')
    expect(await screen.findByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    for (const label of ['Overview', 'Watches', 'Events', 'Deliveries', 'Sinks', 'Subscriptions', 'Admin']) {
      expect(screen.getByRole('link', { name: label })).toBeInTheDocument()
    }
    expect(screen.getByText('Angelia')).toBeInTheDocument()
    expect(screen.getByTestId('user-menu')).toHaveTextContent('octocat')
    expect(screen.getByText('This screen is not built yet.')).toBeInTheDocument()
  })

  it('reports an API error that is not a 401', async () => {
    mockMe(500, {})
    renderApp()
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not reach the Angelia API')
  })
})
