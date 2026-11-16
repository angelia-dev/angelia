import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, mockApi, problemWith, renderApp } from '@/test/mock-api'
import { me, page, plugin, watch } from '@/test/fixtures'

afterEach(() => vi.unstubAllGlobals())

const pypiSchema = {
  type: 'object',
  required: ['package'],
  properties: { package: { type: 'string', description: 'The PyPI project name.' } },
}
const plugins = page([plugin('pypi', 'source', pypiSchema), plugin('npm', 'source')])

const editor = () => me({ role: 'editor', scope: 'write' })
const viewer = () => me({ role: 'viewer', scope: 'read' })

/** Opens the "New watch" form and fills name, source and the pypi package. */
async function fillNewWatch() {
  await userEvent.click(await screen.findByRole('button', { name: 'New watch' }))
  const form = await screen.findByRole('form', { name: 'New watch' })
  await userEvent.type(within(form).getByLabelText(/^Name/), 'flask')
  await userEvent.selectOptions(await within(form).findByRole('combobox', { name: /^Source/ }), 'pypi')
  await userEvent.type(await within(form).findByLabelText(/^package/), 'Flask')
  return form
}

describe('creating a watch', () => {
  it('shows the form to editors, maps it to a PUT with the CSRF header, and refetches the list', async () => {
    const api = mockApi({
      '/v1/me': editor(),
      '/v1/plugins': plugins,
      '/v1/watches': page([]),
      'PUT /v1/watches/flask': jsonResponse(201, watch('flask')),
    })
    renderApp('/ui/watches')
    const form = await fillNewWatch()
    await userEvent.clear(within(form).getByLabelText('Interval'))
    await userEvent.type(within(form).getByLabelText('Interval'), '6h')
    await userEvent.selectOptions(within(form).getByLabelText('Pre-releases'), 'include')
    await userEvent.click(within(form).getByRole('checkbox', { name: 'minor' }))
    await userEvent.click(within(form).getByRole('button', { name: 'Add label' }))
    await userEvent.type(within(form).getByLabelText('Label key'), 'team')
    await userEvent.type(within(form).getByLabelText('Label value'), 'web')
    const before = api.count('/v1/watches')
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))

    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    const put = api.calls.find((c) => c.method === 'PUT')!
    expect(put.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(JSON.parse(put.body)).toEqual({
      source: 'pypi',
      coordinates: { package: 'Flask' },
      interval: '6h',
      labels: { team: 'web' },
      policy: { prerelease: 'include', include: [], exclude: [], constraint: '', bump: ['minor'], cooldown: '0', baseline: true },
    })
    await vi.waitFor(() => expect(api.count('/v1/watches')).toBeGreaterThan(before))
    await vi.waitFor(() => expect(screen.queryByRole('form', { name: 'New watch' })).not.toBeInTheDocument())
  })

  it('checks the fields before sending anything', async () => {
    const api = mockApi({ '/v1/me': editor(), '/v1/plugins': plugins, '/v1/watches': page([]) })
    renderApp('/ui/watches')
    await userEvent.click(await screen.findByRole('button', { name: 'New watch' }))
    const form = await screen.findByRole('form', { name: 'New watch' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'Bad Name')
    await userEvent.clear(within(form).getByLabelText('Interval'))
    await userEvent.type(within(form).getByLabelText('Interval'), '1m')
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))
    expect(await within(form).findByText(/lower-case letters/)).toBeInTheDocument()
    expect(within(form).getByText('Choose a source.')).toBeInTheDocument()
    expect(within(form).getByText('Must be between 5m and 720h.')).toBeInTheDocument()
    expect(within(form).getByLabelText(/^Name/)).toHaveAttribute('aria-invalid', 'true')
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false)
  })

  it('refuses a name that already exists instead of overwriting that watch', async () => {
    const api = mockApi({
      '/v1/me': editor(),
      '/v1/plugins': plugins,
      '/v1/watches': page([]),
      '/v1/watches/flask': watch('flask'),
    })
    renderApp('/ui/watches')
    const form = await fillNewWatch()
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))
    expect(await within(form).findByText(/already exists/)).toBeInTheDocument()
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false)
  })

  it('shows 422 field errors next to the fields and the rest in a summary', async () => {
    mockApi({
      '/v1/me': editor(),
      '/v1/plugins': plugins,
      '/v1/watches': page([]),
      'PUT /v1/watches/flask': problemWith(422, 'Validation failed', {
        errors: [
          { pointer: '/policy/constraint', detail: 'bad range ">>"' },
          { pointer: '/coordinates/package', detail: 'no such package' },
          { pointer: '/policy/include/1', detail: 'invalid regex' },
          { pointer: '/somewhere/else', detail: 'unexpected' },
        ],
      }),
    })
    renderApp('/ui/watches')
    const form = await fillNewWatch()
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))

    const constraint = within(form).getByLabelText('Constraint')
    await vi.waitFor(() => expect(constraint).toHaveAttribute('aria-invalid', 'true'))
    expect(constraint).toHaveAccessibleDescription(/bad range/)
    expect(within(form).getByLabelText(/^package/)).toHaveAccessibleDescription(/no such package/)
    expect(within(form).getByLabelText('Include')).toHaveAccessibleDescription(/invalid regex/)
    // only the error no field claims is listed in the summary
    const summary = within(form).getAllByRole('alert').find((a) => a.textContent?.includes('Validation failed'))!
    expect(summary).toHaveTextContent('/somewhere/else')
    expect(summary).not.toHaveTextContent('bad range')
  })

  it('shows a 409 problem', async () => {
    mockApi({
      '/v1/me': editor(),
      '/v1/plugins': plugins,
      '/v1/watches': page([]),
      'PUT /v1/watches/flask': problemWith(409, 'Managed by file', { detail: 'watch flask is managed by the watch file; change the file instead' }),
    })
    renderApp('/ui/watches')
    const form = await fillNewWatch()
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))
    expect(await within(form).findByText(/managed by the watch file; change the file/)).toBeInTheDocument()
  })

  it('shows a 403 in plain words', async () => {
    mockApi({
      '/v1/me': editor(),
      '/v1/plugins': plugins,
      '/v1/watches': page([]),
      'PUT /v1/watches/flask': problemWith(403, 'Forbidden', { required_scope: 'admin' }),
    })
    renderApp('/ui/watches')
    const form = await fillNewWatch()
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))
    expect(await within(form).findByText(/needs the admin role/)).toBeInTheDocument()
  })

  it('offers no "New watch" to viewers', async () => {
    mockApi({ '/v1/me': viewer(), '/v1/watches': page([watch('flask')]) })
    renderApp('/ui/watches')
    await screen.findByRole('table', { name: 'Watches' })
    expect(screen.queryByRole('button', { name: 'New watch' })).not.toBeInTheDocument()
  })
})

describe('editing and deleting a watch', () => {
  const routes = (who: ReturnType<typeof me>, w = watch('flask')) => ({
    '/v1/me': who,
    '/v1/plugins': plugins,
    '/v1/watches': page([w]),
    '/v1/watches/flask': w,
    '/v1/watches/flask/versions': page([]),
  })

  it('prefills the form, keeps the name fixed and sends the whole watch', async () => {
    const api = mockApi({ ...routes(editor()), 'PUT /v1/watches/flask': watch('flask') })
    renderApp('/ui/watches/flask')
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    const form = await screen.findByRole('form', { name: 'Edit flask' })
    expect(within(form).getByLabelText(/^Name/)).toBeDisabled()
    expect(within(form).getByLabelText(/^Name/)).toHaveValue('flask')
    expect(within(form).getByText(/last write wins/)).toBeInTheDocument()
    await userEvent.clear(within(form).getByLabelText('Interval'))
    await userEvent.type(within(form).getByLabelText('Interval'), '2h')
    await userEvent.click(within(form).getByRole('button', { name: 'Save changes' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    const body = JSON.parse(api.calls.find((c) => c.method === 'PUT')!.body)
    expect(body.interval).toBe('2h')
    expect(body.source).toBe('pypi')
    expect(body.scheme).toBe('pep440')
    expect(body.labels).toEqual({ team: 'platform' })
  })

  it('asks before deleting, then deletes with the CSRF header and goes back to the list', async () => {
    const api = mockApi({ ...routes(editor()), 'DELETE /v1/watches/flask': new Response(null, { status: 204 }) })
    renderApp('/ui/watches/flask')
    await userEvent.click(await screen.findByRole('button', { name: /^Delete/ }))
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false)
    const dialog = screen.getByRole('alertdialog')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /^Delete/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'DELETE')).toBe(true))
    expect(api.calls.find((c) => c.method === 'DELETE')!.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(await screen.findByRole('heading', { name: 'Watches' })).toBeInTheDocument()
  })

  it('shows a refused delete', async () => {
    mockApi({ ...routes(editor()), 'DELETE /v1/watches/flask': problemWith(409, 'Conflict', { detail: 'cannot delete right now' }) })
    renderApp('/ui/watches/flask')
    await userEvent.click(await screen.findByRole('button', { name: /^Delete/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    expect(await screen.findByText('cannot delete right now')).toBeInTheDocument()
  })

  it('shows the watch as a watch-file entry', async () => {
    mockApi(routes(viewer()))
    renderApp('/ui/watches/flask')
    await userEvent.click(await screen.findByRole('button', { name: 'Show as YAML' }))
    const yaml = screen.getByLabelText('Watch file YAML')
    expect(yaml).toHaveTextContent('version: 1')
    expect(yaml).toHaveTextContent('- name: flask')
    expect(yaml).toHaveTextContent('package: flask')
    await userEvent.click(screen.getByRole('button', { name: 'Hide YAML' }))
    expect(screen.queryByLabelText('Watch file YAML')).not.toBeInTheDocument()
  })

  it('hides Edit and Delete from viewers', async () => {
    mockApi(routes(viewer()))
    renderApp('/ui/watches/flask')
    await screen.findByRole('button', { name: 'Show as YAML' })
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Delete/ })).not.toBeInTheDocument()
  })

  it('keeps file-managed watches read-only and links the badge to the docs', async () => {
    mockApi(routes(me(), watch('flask', { managed_by: 'file' })))
    renderApp('/ui/watches/flask')
    const link = await screen.findByRole('link', { name: /managed by the watch file/ })
    expect(link).toHaveAttribute('href', expect.stringContaining('docs/watch-file.md'))
    expect(screen.getByText(/read-only here/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Delete/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Show as YAML' })).not.toBeInTheDocument()
  })
})

describe('watch form details', () => {
  const optSchema = {
    type: 'object',
    required: ['package'],
    properties: { package: { type: 'string' }, index: { type: 'string', description: 'Optional index URL.' } },
  }

  it('drops an optional coordinate that was typed and cleared again', async () => {
    const api = mockApi({
      '/v1/me': editor(),
      '/v1/plugins': page([plugin('pypi', 'source', optSchema)]),
      '/v1/watches': page([]),
      'PUT /v1/watches/flask': jsonResponse(201, watch('flask')),
    })
    renderApp('/ui/watches')
    const form = await fillNewWatch()
    await userEvent.type(within(form).getByLabelText(/^index/), 'x')
    await userEvent.clear(within(form).getByLabelText(/^index/))
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(JSON.parse(api.calls.find((c) => c.method === 'PUT')!.body).coordinates).toEqual({ package: 'Flask' })
  })

  it('moves focus to the first invalid field', async () => {
    mockApi({ '/v1/me': editor(), '/v1/plugins': plugins, '/v1/watches': page([]) })
    renderApp('/ui/watches')
    await userEvent.click(await screen.findByRole('button', { name: 'New watch' }))
    const form = await screen.findByRole('form', { name: 'New watch' })
    await userEvent.click(within(form).getByRole('button', { name: 'Create watch' }))
    await vi.waitFor(() => expect(within(form).getByLabelText(/^Name/)).toHaveFocus())
  })

  it('focuses Cancel in the delete confirmation and returns focus on Escape', async () => {
    const w = watch('flask')
    mockApi({ '/v1/me': editor(), '/v1/plugins': plugins, '/v1/watches': page([w]), '/v1/watches/flask': w, '/v1/watches/flask/versions': page([]) })
    renderApp('/ui/watches/flask')
    await userEvent.click(await screen.findByRole('button', { name: /^Delete/ }))
    const dialog = screen.getByRole('alertdialog')
    expect(dialog).toHaveAccessibleDescription(/Delete watch flask/)
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus()
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^Delete/ })).toHaveFocus()
  })
})
