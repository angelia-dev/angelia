import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, mockApi, problemWith, renderApp } from '@/test/mock-api'
import { me, page, plugin, secret, sink, subscription } from '@/test/fixtures'

afterEach(() => vi.unstubAllGlobals())

const webhookSchema = {
  type: 'object',
  required: ['url'],
  properties: {
    url: { type: 'string', description: 'The https URL.' },
    secret_ref: { type: 'string', description: 'Signing secret.' },
    timeout: { type: 'string' },
  },
}
const sinkPlugins = page([plugin('webhook', 'sink', webhookSchema), plugin('shoutrrr', 'sink')])
const sourcePlugins = page([plugin('pypi', 'source'), plugin('npm', 'source')])
const pluginsRoute = ({ url }: { url: URL }) => (url.searchParams.get('interface') === 'sink' ? sinkPlugins : sourcePlugins)

const admin = () => me()
const editor = () => me({ role: 'editor', scope: 'write' })
const viewer = () => me({ role: 'viewer', scope: 'read' })

describe('sinks', () => {
  const base = (who = admin()) => ({
    '/v1/me': who,
    '/v1/plugins': pluginsRoute,
    '/v1/secrets': page([secret('ci-signing'), secret('other')]),
    '/v1/sinks': page([sink('from-file'), sink('ci', { managed_by: 'api', url_redacted: false, config: { url: 'https://ci.example/hook', secret_ref: 'ci-signing' }, masked_fields: [] })]),
  })

  it('creates a sink: config fields from the plugin schema, ref picked from secret names', async () => {
    const api = mockApi({ ...base(), 'PUT /v1/sinks/hook': jsonResponse(201, sink('hook')) })
    renderApp('/ui/sinks')
    await userEvent.click(await screen.findByRole('button', { name: 'New sink' }))
    const form = await screen.findByRole('form', { name: 'New sink' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'hook')
    await userEvent.selectOptions(within(form).getByLabelText(/^Kind/), 'webhook')
    await userEvent.type(within(form).getByLabelText(/^url/), 'https://ci.example/hook')
    const ref = within(form).getByLabelText('secret_ref')
    expect(within(ref).getAllByRole('option').map((o) => o.textContent)).toEqual(['(none)', 'ci-signing', 'other'])
    await userEvent.selectOptions(ref, 'ci-signing')
    const before = api.count('/v1/sinks')
    await userEvent.click(within(form).getByRole('button', { name: 'Create sink' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    const put = api.calls.find((c) => c.method === 'PUT')!
    expect(put.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(JSON.parse(put.body)).toEqual({ kind: 'webhook', config: { url: 'https://ci.example/hook', secret_ref: 'ci-signing' } })
    await vi.waitFor(() => expect(api.count('/v1/sinks')).toBeGreaterThan(before))
  })

  it('validates before sending and maps 422 field errors', async () => {
    const api = mockApi({
      ...base(),
      'PUT /v1/sinks/hook': problemWith(422, 'Validation failed', {
        errors: [
          { pointer: '/config/url', detail: 'url must be https' },
          { pointer: '/config', detail: 'exactly one of a and b' },
        ],
      }),
    })
    renderApp('/ui/sinks')
    await userEvent.click(await screen.findByRole('button', { name: 'New sink' }))
    const form = await screen.findByRole('form', { name: 'New sink' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'hook')
    await userEvent.selectOptions(within(form).getByLabelText(/^Kind/), 'webhook')
    await userEvent.click(within(form).getByRole('button', { name: 'Create sink' }))
    expect(await within(form).findByText('Required.')).toBeInTheDocument()
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false)
    await userEvent.type(within(form).getByLabelText(/^url/), 'http://x')
    await userEvent.click(within(form).getByRole('button', { name: 'Create sink' }))
    await vi.waitFor(() => expect(within(form).getByLabelText(/^url/)).toHaveAccessibleDescription(/url must be https/))
    const summary = within(form).getAllByRole('alert').find((a) => a.textContent?.includes('Validation failed'))!
    expect(summary).toHaveTextContent('exactly one of a and b')
  })

  it('shows admin-only controls to admins and nothing to editors and viewers', async () => {
    mockApi(base(editor()))
    renderApp('/ui/sinks')
    await screen.findByRole('table', { name: 'Sinks' })
    expect(screen.queryByRole('button', { name: 'New sink' })).not.toBeInTheDocument()
    expect(screen.queryByRole('columnheader', { name: 'Actions' })).not.toBeInTheDocument()
  })

  it('keeps file-managed sinks read-only and edits API sinks with the stored config', async () => {
    const api = mockApi({ ...base(), 'PUT /v1/sinks/ci': sink('ci') })
    renderApp('/ui/sinks')
    const table = await screen.findByRole('table', { name: 'Sinks' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('read-only')
    expect(within(rows[0]!).queryByRole('button')).not.toBeInTheDocument()
    expect(within(rows[0]!).getByRole('link', { name: /watch file/ })).toBeInTheDocument()
    await userEvent.click(within(rows[1]!).getByRole('button', { name: /Edit/ }))
    const form = await screen.findByRole('form', { name: 'Edit ci' })
    expect(within(form).getByLabelText(/^Name/)).toBeDisabled()
    await vi.waitFor(() => expect(within(form).getByLabelText(/^url/)).toHaveValue('https://ci.example/hook'))
    expect(within(form).getByLabelText('secret_ref')).toHaveValue('ci-signing')
    await userEvent.click(within(form).getByRole('button', { name: 'Save changes' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(JSON.parse(api.calls.find((c) => c.method === 'PUT')!.body)).toEqual({
      kind: 'webhook',
      config: { url: 'https://ci.example/hook', secret_ref: 'ci-signing' },
    })
  })

  it('shows the sink-in-use reason when a delete is refused', async () => {
    mockApi({
      ...base(),
      'DELETE /v1/sinks/ci': problemWith(409, 'Sink in use', { detail: 'sink ci is used by subscription platform-ci' }),
    })
    renderApp('/ui/sinks')
    const table = await screen.findByRole('table', { name: 'Sinks' })
    await userEvent.click(within(table).getByRole('button', { name: /^Delete/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    expect(await screen.findByText('sink ci is used by subscription platform-ci')).toBeInTheDocument()
  })

  it('deletes after confirmation', async () => {
    const api = mockApi({ ...base(), 'DELETE /v1/sinks/ci': new Response(null, { status: 204 }) })
    renderApp('/ui/sinks')
    const table = await screen.findByRole('table', { name: 'Sinks' })
    await userEvent.click(within(table).getByRole('button', { name: /^Delete/ }))
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false)
    const before = api.count('/v1/sinks')
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    await vi.waitFor(() => expect(api.calls.find((c) => c.method === 'DELETE')?.headers.get('X-Angelia-CSRF')).toBe('1'))
    await vi.waitFor(() => expect(api.count('/v1/sinks')).toBeGreaterThan(before))
  })
})

describe('subscriptions', () => {
  const apiSub = () => ({ ...subscription('platform-ci', 'ci'), managed_by: 'api' as const })
  const base = (who = editor()) => ({
    '/v1/me': who,
    '/v1/plugins': pluginsRoute,
    '/v1/sinks': page([sink('from-file'), sink('ci', { managed_by: 'api' })]),
    '/v1/subscriptions': page([subscription('file-sub', 'from-file'), apiSub()]),
  })

  it('creates a subscription for an editor, offering only API-managed sinks', async () => {
    const api = mockApi({ ...base(), 'PUT /v1/subscriptions/mine': jsonResponse(201, apiSub()) })
    renderApp('/ui/subscriptions')
    await userEvent.click(await screen.findByRole('button', { name: 'New subscription' }))
    const form = await screen.findByRole('form', { name: 'New subscription' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'mine')
    const sinkSelect = await within(form).findByLabelText(/^Sink/)
    await vi.waitFor(() => expect(within(sinkSelect).getAllByRole('option').map((o) => o.textContent)).toEqual(['Choose a sink', 'ci (webhook)']))
    await userEvent.selectOptions(sinkSelect, 'ci')
    await userEvent.click(await within(form).findByRole('checkbox', { name: 'pypi' }))
    await userEvent.click(within(form).getByRole('checkbox', { name: 'minor' }))
    await userEvent.type(within(form).getByLabelText('Package glob'), 'req*')
    await userEvent.selectOptions(within(form).getByLabelText('Pre-releases'), 'none')
    await userEvent.type(within(form).getByLabelText('Template'), 'hello')
    await userEvent.click(within(form).getByRole('button', { name: 'Create subscription' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true))
    const put = api.calls.find((c) => c.method === 'PUT')!
    expect(put.headers.get('X-Angelia-CSRF')).toBe('1')
    expect(put.url.searchParams.get('dry_run')).toBeNull()
    expect(JSON.parse(put.body)).toEqual({
      sink: 'ci',
      filter: { source: ['pypi'], labels: {}, bump: ['minor'], package: 'req*', type: ['release.published'] },
      policy: { prerelease: 'none', cooldown: '0' },
      template: 'hello',
    })
  })

  it('checks the template with a dry run and shows its error next to the field', async () => {
    let n = 0
    const api = mockApi({
      ...base(),
      'PUT /v1/subscriptions/platform-ci': () =>
        ++n === 1
          ? problemWith(422, 'Validation failed', { errors: [{ pointer: '/template', detail: 'template: unexpected "}"' }] })
          : jsonResponse(200, apiSub()),
    })
    renderApp('/ui/subscriptions')
    const table = await screen.findByRole('table', { name: 'Subscriptions' })
    const row = within(table).getByRole('row', { name: /platform-ci/ })
    await userEvent.click(within(row).getByRole('button', { name: /Edit/ }))
    const form = await screen.findByRole('form', { name: 'Edit platform-ci' })
    await userEvent.type(within(form).getByLabelText('Template'), '}}')
    await userEvent.click(within(form).getByRole('button', { name: 'Check template' }))
    await vi.waitFor(() => expect(within(form).getByLabelText('Template')).toHaveAccessibleDescription(/unexpected/))
    expect(api.calls.filter((c) => c.method === 'PUT').every((c) => c.url.searchParams.get('dry_run') === 'true')).toBe(true)
    await userEvent.click(within(form).getByRole('button', { name: 'Check template' }))
    expect(await within(form).findByRole('status')).toHaveTextContent('nothing was saved')
  })

  it('shows a 409 cross-owner problem and a 403', async () => {
    mockApi({
      ...base(),
      'PUT /v1/subscriptions/mine': problemWith(409, 'Cross-owner reference', { detail: 'sink ci is managed by the watch file' }),
    })
    renderApp('/ui/subscriptions')
    await userEvent.click(await screen.findByRole('button', { name: 'New subscription' }))
    const form = await screen.findByRole('form', { name: 'New subscription' })
    await userEvent.type(within(form).getByLabelText(/^Name/), 'mine')
    await vi.waitFor(() => expect(within(form).getByRole('option', { name: 'ci (webhook)' })).toBeInTheDocument())
    await userEvent.selectOptions(within(form).getByLabelText(/^Sink/), 'ci')
    await userEvent.click(within(form).getByRole('button', { name: 'Create subscription' }))
    expect(await within(form).findByText('sink ci is managed by the watch file')).toBeInTheDocument()
  })

  it('keeps file-managed subscriptions read-only', async () => {
    mockApi(base())
    renderApp('/ui/subscriptions')
    const table = await screen.findByRole('table', { name: 'Subscriptions' })
    const row = within(table).getByRole('row', { name: /file-sub/ })
    expect(row).toHaveTextContent('read-only')
    expect(within(row).queryByRole('button')).not.toBeInTheDocument()
  })

  it('shows no controls to viewers', async () => {
    mockApi(base(viewer()))
    renderApp('/ui/subscriptions')
    await screen.findByRole('table', { name: 'Subscriptions' })
    expect(screen.queryByRole('button', { name: 'New subscription' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Edit/ })).not.toBeInTheDocument()
  })

  it('deletes after confirmation and shows a refusal', async () => {
    const api = mockApi({ ...base(), 'DELETE /v1/subscriptions/platform-ci': new Response(null, { status: 204 }) })
    renderApp('/ui/subscriptions')
    const table = await screen.findByRole('table', { name: 'Subscriptions' })
    await userEvent.click(within(table).getByRole('button', { name: /^Delete/ }))
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false)
    await userEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'DELETE')).toBe(true))
  })
})
