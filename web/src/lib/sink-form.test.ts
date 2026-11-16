import { describe, expect, it } from 'vitest'
import { schemaFields } from '@/api/plugins'
import { newPair } from '@/components/form'
import { sink } from '@/test/fixtures'
import { emptySinkForm, formToSinkWrite, sinkToForm, validateSinkForm } from './sink-form'

const webhook = schemaFields({
  type: 'object',
  required: ['url'],
  properties: {
    url: { type: 'string', pattern: '^https://' },
    secret_ref: { type: 'string' },
    signing_key: { type: 'string', 'x-angelia-secret': true },
    timeout: { type: 'string' },
    retries: { type: 'integer' },
    insecure: { type: 'boolean' },
    inputs: { type: 'object' },
  },
} as never)

describe('sink form mapping', () => {
  it('builds the config from typed fields and drops empty and secret-marked ones', () => {
    const f = {
      ...emptySinkForm(),
      kind: 'webhook',
      values: { url: ' https://ci.example/hook ', secret_ref: 'ci-signing', timeout: '', retries: '3', insecure: 'false', signing_key: 'x' },
      objects: { inputs: [newPair('a', 'b'), newPair('', '')] },
    }
    expect(formToSinkWrite(f, webhook)).toEqual({
      kind: 'webhook',
      config: { url: 'https://ci.example/hook', secret_ref: 'ci-signing', retries: 3, insecure: false, inputs: { a: 'b' } },
    })
  })

  it('loads a sink without its masked or redacted values and keeps unknown keys', () => {
    const s = sink('ci', {
      managed_by: 'api',
      masked_fields: ['signing_key'],
      config: { url: 'https://ci.example/hook', signing_key: '[REDACTED]', secret_ref: 'ci-signing', extra_flag: ['x'], inputs: { a: 'b' } },
    })
    const f = sinkToForm(s, webhook)
    expect(f.values).toEqual({ url: 'https://ci.example/hook', secret_ref: 'ci-signing' })
    expect(f.extra).toEqual({ extra_flag: ['x'] })
    expect(formToSinkWrite(f, webhook).config).toEqual({
      url: 'https://ci.example/hook',
      secret_ref: 'ci-signing',
      inputs: { a: 'b' },
      extra_flag: ['x'],
    })
  })

  it('edits the whole config as JSON when the kind has no schema', () => {
    const f = { ...emptySinkForm(), kind: 'odd', raw: '{"a_env":"X"}' }
    expect(formToSinkWrite(f, []).config).toEqual({ a_env: 'X' })
    expect(validateSinkForm({ ...f, raw: '[1]' }, [], { creating: false })['/config']).toBeDefined()
    expect(validateSinkForm({ ...f, raw: '{' }, [], { creating: false })['/config']).toBeDefined()
  })
})

describe('sink form validation', () => {
  it('mirrors the schema', () => {
    const f = { ...emptySinkForm(), name: 'Bad', values: { retries: '1.5', url: 'http://x' } }
    const e = validateSinkForm(f, webhook, { creating: true })
    expect(Object.keys(e).sort()).toEqual(['/config/retries', '/config/url', '/kind', '/name'])
  })

  it('requires required fields and accepts a good form', () => {
    const f = { ...emptySinkForm(), name: 'ci', kind: 'webhook' }
    expect(validateSinkForm(f, webhook, { creating: true })['/config/url']).toEqual(['Required.'])
    expect(validateSinkForm({ ...f, values: { url: 'https://ci.example' } }, webhook, { creating: true })).toEqual({})
  })
})
