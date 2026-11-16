import { QueryClient } from '@tanstack/react-query'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, mockApi, problemWith, renderApp } from '@/test/mock-api'
import { me, page, secret } from '@/test/fixtures'

afterEach(() => vi.unstubAllGlobals())

const list = page([
  secret('ci-signing', { hosts: ['ci.example'], referenced_by: [{ resource: 'sink', name: 'ci', pointer: '/config/secret_ref' }] }),
  secret('spare'),
])
const VALUE = 'sk-super-secret-value-123'

const routes = (extra: Record<string, unknown> = {}) => ({ '/v1/me': me(), '/v1/secrets': list, ...extra })

describe('secrets page', () => {
  it('lists names and metadata, never a value', async () => {
    mockApi(routes())
    renderApp('/ui/secrets')
    const table = await screen.findByRole('table', { name: 'Secrets' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('ci-signing')
    expect(rows[0]).toHaveTextContent('ci.example')
    expect(rows[0]).toHaveTextContent('0123456789abcdef')
    expect(rows[0]).toHaveTextContent('sink ci /config/secret_ref')
    expect(rows[1]).toHaveTextContent('unbound')
  })

  it('is for admins only: hidden in the navigation and refused to an editor', async () => {
    mockApi(routes({ '/v1/me': me({ role: 'editor', scope: 'write' }) }))
    renderApp('/ui/secrets')
    expect(await screen.findByRole('alert')).toHaveTextContent('administrators')
    expect(screen.queryByRole('link', { name: 'Secrets' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'New secret' })).not.toBeInTheDocument()
  })

  it('stores a secret with a write-only field, cleared on submit, and keeps no value in the cache', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    let release: (r: Response) => void = () => {}
    const api = mockApi(
      routes({
        'PUT /v1/secrets/new-one': () => new Promise<Response>((resolve) => (release = resolve)),
      }),
    )
    renderApp('/ui/secrets', client)
    await userEvent.click(await screen.findByRole('button', { name: 'New secret' }))
    const form = await screen.findByRole('form', { name: 'New secret' })
    const input = within(form).getByLabelText(/^Value/)
    expect(input).toHaveAttribute('type', 'password')
    await userEvent.type(within(form).getByLabelText(/^Name/), 'new-one')
    await userEvent.type(input, VALUE)
    await userEvent.type(within(form).getByLabelText('Hosts'), 'ci.example\napi.example:8443')
    await userEvent.click(within(form).getByRole('button', { name: 'Store secret' }))

    // cleared at submit, before the server has answered
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(within(form).getByLabelText(/^Value/)).toHaveValue('')
    const put = api.calls.find((c) => c.method === 'PUT')!
    expect(put.url.pathname).toBe('/v1/secrets/new-one')
    expect(put.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(JSON.parse(put.body)).toEqual({ hosts: ['ci.example', 'api.example:8443'], value: VALUE })
    release(jsonResponse(201, secret('new-one')))
    await vi.waitFor(() => expect(screen.queryByRole('form', { name: 'New secret' })).not.toBeInTheDocument())

    const cached = JSON.stringify(client.getQueryCache().getAll().map((q) => [q.queryKey, q.state.data, q.state.error]))
    expect(cached).not.toContain(VALUE)
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => [m.state.variables, m.state.data]))).not.toContain(VALUE)
    expect(document.body.innerHTML).not.toContain(VALUE)
  })

  it('also clears the field when the server refuses, and shows the problem', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    mockApi(
      routes({
        'PUT /v1/secrets/new-one': problemWith(422, 'Secrets key missing', { detail: 'the instance has no secrets key configured' }),
      }),
    )
    renderApp('/ui/secrets', client)
    await userEvent.click(await screen.findByRole('button', { name: 'New secret' }))
    const form = await screen.findByRole('form', { name: 'New secret' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'new-one')
    await userEvent.type(within(form).getByLabelText(/^Value/), VALUE)
    await userEvent.click(within(form).getByRole('button', { name: 'Store secret' }))
    expect(await within(form).findByText('the instance has no secrets key configured')).toBeInTheDocument()
    expect(within(form).getByLabelText(/^Value/)).toHaveValue('')
    expect(JSON.stringify(client.getQueryCache().getAll().map((q) => q.state.error))).not.toContain(VALUE)
    expect(document.body.innerHTML).not.toContain(VALUE)
  })

  it('maps 422 field errors for the hosts', async () => {
    mockApi(
      routes({
        'PUT /v1/secrets/new-one': problemWith(422, 'Validation failed', { errors: [{ pointer: '/hosts/0', detail: 'not a host' }] }),
      }),
    )
    renderApp('/ui/secrets')
    await userEvent.click(await screen.findByRole('button', { name: 'New secret' }))
    const form = await screen.findByRole('form', { name: 'New secret' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'new-one')
    await userEvent.type(within(form).getByLabelText(/^Value/), VALUE)
    await userEvent.type(within(form).getByLabelText('Hosts'), 'bad_host')
    await userEvent.click(within(form).getByRole('button', { name: 'Store secret' }))
    await vi.waitFor(() => expect(within(form).getByLabelText('Hosts')).toHaveAccessibleDescription(/not a host/))
  })

  it('checks the form before sending: name, value, hosts', async () => {
    const api = mockApi(routes())
    renderApp('/ui/secrets')
    await userEvent.click(await screen.findByRole('button', { name: 'New secret' }))
    const form = await screen.findByRole('form', { name: 'New secret' })
    await userEvent.type(within(form).getByLabelText('Hosts'), 'Upper.example, *.example')
    await userEvent.click(within(form).getByRole('button', { name: 'Store secret' }))
    expect(await within(form).findByText('Enter the secret value.')).toBeInTheDocument()
    expect(within(form).getByText(/use lower case/)).toBeInTheDocument()
    expect(within(form).getByText(/wildcards are not allowed/)).toBeInTheDocument()
    expect(within(form).getByLabelText(/^Name/)).toHaveAttribute('aria-invalid', 'true')
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false)
  })

  it('sends value_base64 when the value is base64', async () => {
    const api = mockApi(routes({ 'PUT /v1/secrets/bin': jsonResponse(201, secret('bin')) }))
    renderApp('/ui/secrets')
    await userEvent.click(await screen.findByRole('button', { name: 'New secret' }))
    const form = await screen.findByRole('form', { name: 'New secret' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'bin')
    await userEvent.click(within(form).getByRole('checkbox', { name: /base64/ }))
    await userEvent.type(within(form).getByLabelText(/^Value/), 'aGVsbG8=')
    await userEvent.click(within(form).getByRole('button', { name: 'Store secret' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(JSON.parse(api.calls.find((c) => c.method === 'PUT')!.body)).toEqual({ hosts: [], value_base64: 'aGVsbG8=' })
  })

  it('refuses a name that already exists on create, and replaces from the list', async () => {
    const api = mockApi(routes({ '/v1/secrets/spare': secret('spare'), 'PUT /v1/secrets/spare': secret('spare') }))
    renderApp('/ui/secrets')
    await userEvent.click(await screen.findByRole('button', { name: 'New secret' }))
    let form = await screen.findByRole('form', { name: 'New secret' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'spare')
    await userEvent.type(within(form).getByLabelText(/^Value/), VALUE)
    await userEvent.click(within(form).getByRole('button', { name: 'Store secret' }))
    expect(await within(form).findByText(/already exists/)).toBeInTheDocument()
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false)
    await userEvent.click(within(form).getByRole('button', { name: 'Cancel' }))

    const table = screen.getByRole('table', { name: 'Secrets' })
    await userEvent.click(within(within(table).getByRole('row', { name: /spare/ })).getByRole('button', { name: /Replace/ }))
    form = await screen.findByRole('form', { name: 'Replace spare' })
    expect(within(form).getByLabelText(/^Name/)).toBeDisabled()
    await userEvent.type(within(form).getByLabelText(/^New value/), VALUE)
    await userEvent.click(within(form).getByRole('button', { name: 'Replace secret' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(JSON.parse(api.calls.find((c) => c.method === 'PUT')!.body).value).toBe(VALUE)
  })

  it('shows the secret-in-use reason when a delete is refused', async () => {
    mockApi(
      routes({
        'DELETE /v1/secrets/ci-signing': problemWith(409, 'Secret in use', {
          detail: 'secret ci-signing is referenced by sink ci (/config/secret_ref)',
        }),
      }),
    )
    renderApp('/ui/secrets')
    const table = await screen.findByRole('table', { name: 'Secrets' })
    await userEvent.click(within(within(table).getByRole('row', { name: /ci-signing/ })).getByRole('button', { name: /^Delete/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    expect(await screen.findByText('secret ci-signing is referenced by sink ci (/config/secret_ref)')).toBeInTheDocument()
  })

  it('deletes an unreferenced secret after confirmation', async () => {
    const api = mockApi(routes({ 'DELETE /v1/secrets/spare': new Response(null, { status: 204 }) }))
    renderApp('/ui/secrets')
    const table = await screen.findByRole('table', { name: 'Secrets' })
    await userEvent.click(within(within(table).getByRole('row', { name: /spare/ })).getByRole('button', { name: /^Delete/ }))
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false)
    const before = api.count('/v1/secrets')
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'DELETE')).toBe(true))
    await vi.waitFor(() => expect(api.count('/v1/secrets')).toBeGreaterThan(before))
  })
})
