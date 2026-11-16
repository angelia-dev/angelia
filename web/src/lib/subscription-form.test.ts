import { describe, expect, it } from 'vitest'
import { newPair } from '@/components/form'
import { subscription } from '@/test/fixtures'
import { emptySubscriptionForm, formToSubscriptionWrite, subscriptionToForm, validateSubscriptionForm } from './subscription-form'

describe('subscription form', () => {
  it('maps a subscription to the form and back to the same write body', () => {
    const s = subscription('to-slack', 'slack')
    s.filter = { source: ['pypi'], labels: { team: 'web' }, bump: ['major'], package: 'req*', type: ['release.published', 'release.yanked'] }
    s.policy = { prerelease: 'none', cooldown: '24h' }
    s.template = '{{ .data.package }}'
    const write = { sink: s.sink, filter: s.filter, policy: s.policy, template: s.template }
    expect(formToSubscriptionWrite(subscriptionToForm(s))).toEqual(write)
  })

  it('sends a null pre-release mode when it is inherited', () => {
    const f = { ...emptySubscriptionForm(), sink: 'slack' }
    expect(formToSubscriptionWrite(f).policy).toEqual({ prerelease: null, cooldown: '0' })
  })

  it('validates name, sink, types, labels, cooldown and template size', () => {
    const e = validateSubscriptionForm(
      { ...emptySubscriptionForm(), name: 'X', types: [], labels: [newPair('a', '1'), newPair('a', '2')], cooldown: 'x', template: 'a'.repeat(32769) },
      { creating: true },
    )
    expect(Object.keys(e).sort()).toEqual(['/filter/labels', '/filter/type', '/name', '/policy/cooldown', '/sink', '/template'])
    expect(validateSubscriptionForm({ ...emptySubscriptionForm(), name: 'ok', sink: 's' }, { creating: true })).toEqual({})
  })

  it('refuses a negative cooldown and control characters in label values', () => {
    const e = validateSubscriptionForm(
      { ...emptySubscriptionForm(), name: 'ok', sink: 's', cooldown: '-1h', labels: [newPair('team', 'a\nb')] },
      { creating: true },
    )
    expect(Object.keys(e).sort()).toEqual(['/filter/labels', '/policy/cooldown'])
  })
})
