import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { me, page, watch } from '@/test/fixtures'
import { mockApi, problem, renderApp } from '@/test/mock-api'

afterEach(() => vi.unstubAllGlobals())

describe('watches list', () => {
  it('shows an accessible table with badges, labels and relative times', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/watches': page([
        watch('requests', { managed_by: 'file' }),
        watch('flask', { source: 'npm', managed_by: 'api' }, { error_count: 3, last_checked_at: null }),
      ]),
    })
    renderApp('/ui/watches')
    const table = await screen.findByRole('table', { name: 'Watches' })
    expect(within(table).getAllByRole('columnheader').every((th) => th.getAttribute('scope') === 'col')).toBe(true)
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('requests')
    expect(rows[0]).toHaveTextContent('package: requests')
    expect(rows[0]).toHaveTextContent('team=platform')
    expect(rows[0]).toHaveTextContent('file')
    expect(rows[0]).toHaveTextContent('3 minutes ago')
    expect(rows[0]).toHaveTextContent('in 57 minutes')
    expect(rows[1]).toHaveTextContent('3 errors') // text, not only colour
    expect(rows[1]).toHaveTextContent('api')
  })

  it('sends the source and label filters to the API', async () => {
    const api = mockApi({ '/v1/me': me(), '/v1/watches': page([watch('requests')]) })
    renderApp('/ui/watches')
    await screen.findByRole('table', { name: 'Watches' })
    await userEvent.type(screen.getByLabelText('Source'), 'pypi')
    await userEvent.type(screen.getByLabelText('Label (key=value)'), 'team=platform')
    await vi.waitFor(() => {
      const last = api.calls.filter((c) => c.url.pathname === '/v1/watches').at(-1)
      expect(last?.url.searchParams.getAll('source')).toEqual(['pypi'])
      expect(last?.url.searchParams.getAll('label')).toEqual(['team=platform'])
    })
  })

  it('pages with the next cursor on "Load more"', async () => {
    const api = mockApi({
      '/v1/me': me(),
      '/v1/watches': ({ url }) => (url.searchParams.get('cursor') ? page([watch('second')]) : page([watch('first')], 'CUR1')),
    })
    renderApp('/ui/watches')
    await screen.findByText('first')
    await userEvent.click(screen.getByRole('button', { name: 'Load more' }))
    expect(await screen.findByText('second')).toBeInTheDocument()
    expect(api.calls.at(-1)?.url.searchParams.get('cursor')).toBe('CUR1')
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument()
  })

  it('shows empty states, plain and filtered', async () => {
    mockApi({ '/v1/me': me(), '/v1/watches': page([]) })
    renderApp('/ui/watches')
    expect(await screen.findByText('No watches yet')).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Source'), 'x')
    expect(await screen.findByText('No watch matches these filters')).toBeInTheDocument()
  })

  it('shows the problem title and detail with a retry button', async () => {
    mockApi({ '/v1/me': me(), '/v1/watches': problem(400, 'Invalid label filter', 'label must be key=value') })
    renderApp('/ui/watches')
    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid label filter')
    expect(screen.getByRole('alert')).toHaveTextContent('label must be key=value')
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })
})

describe('watch detail', () => {
  it('shows status, policy and versions', async () => {
    const w = watch(
      'requests',
      { policy: { prerelease: 'include', include: ['^2\\.'], exclude: [], constraint: '>=2', bump: ['minor'], cooldown: '24h', baseline: false } },
      { error_count: 1 },
    )
    const seen = new Date().toISOString()
    mockApi({
      '/v1/me': me(),
      '/v1/watches/requests': w,
      '/v1/watches/requests/versions': page([
        { version: '2.33.0', normalized_version: '2.33.0', prerelease: false, yanked: false, published_at: null, first_seen_at: seen, removed_at: null },
        { version: '2.34.0rc1', normalized_version: '2.34.0rc1', prerelease: true, yanked: true, published_at: null, first_seen_at: seen, removed_at: null },
      ]),
    })
    renderApp('/ui/watches/requests')
    expect(await screen.findByRole('heading', { name: 'requests' })).toBeInTheDocument()
    const policy = screen.getByRole('table', { name: 'Watch policy' })
    expect(policy).toHaveTextContent('^2\\.')
    expect(policy).toHaveTextContent('>=2')
    expect(policy).toHaveTextContent('24h')
    expect(screen.getByRole('table', { name: 'Watch status' })).toHaveTextContent('1 error')
    const versions = await screen.findByRole('table', { name: 'Versions' })
    expect(versions).toHaveTextContent('2.33.0')
    expect(versions).toHaveTextContent('pre-release, yanked')
  })

  it('shows a not-found problem', async () => {
    mockApi({
      '/v1/me': me(),
      '/v1/watches/nope': problem(404, 'Watch not found', 'no watch named nope'),
      '/v1/watches/nope/versions': page([]),
    })
    renderApp('/ui/watches/nope')
    expect(await screen.findByRole('alert')).toHaveTextContent('no watch named nope')
  })
})
