import { describe, expect, it } from 'vitest'
import { watch } from '@/test/fixtures'
import { scalar, watchToYaml } from './yaml'

describe('watchToYaml', () => {
  it('renders a watch-file entry', () => {
    const w = watch('requests')
    w.policy.include = ['^2\\.']
    w.policy.bump = ['minor', 'patch']
    expect(watchToYaml(w)).toBe(`version: 1
watches:
  - name: requests
    source: pypi
    coordinates:
      package: requests
    interval: 1h
    scheme: pep440
    labels:
      team: platform
    policy:
      prerelease: none
      include: ["^2\\\\."]
      exclude: []
      constraint: ""
      bump: [minor, patch]
      cooldown: 0s
      baseline: true
`)
  })

  it('omits labels when there are none', () => {
    expect(watchToYaml(watch('x', { labels: {} }))).not.toContain('labels:')
  })

  it('quotes scalars that YAML would read as something else', () => {
    expect(scalar('true')).toBe('"true"')
    expect(scalar('1.10')).toBe('"1.10"')
    expect(scalar('')).toBe('""')
    expect(scalar('>=2 <3')).toBe('">=2 <3"')
    expect(scalar('@types/node')).toBe('"@types/node"')
    expect(scalar('scope/@x')).toBe('scope/@x')
    expect(scalar('.inf')).toBe('".inf"')
    expect(scalar('-.NaN')).toBe('"-.NaN"')
    expect(scalar('2024-01-02')).toBe('"2024-01-02"')
    expect(scalar('a: b')).toBe('"a: b"')
    expect(scalar('#x')).toBe('"#x"')
  })

  it('cannot be broken out of with newlines, quotes or control characters', () => {
    expect(scalar('a\nb: c')).toBe('"a\\nb: c"')
    expect(scalar('x"\n- name: evil')).toBe('"x\\"\\n- name: evil"')
    expect(scalar('a\u2028b\u007fc\u0085')).toBe('"a\\u2028b\\u007fc\\u0085"')
    expect(scalar('tab\there')).toBe('"tab\\there"')
  })

  it('quotes keys and values of maps so a key cannot inject structure', () => {
    const y = watchToYaml(watch('x', { labels: { 'k': 'v\n  evil: 1' }, coordinates: { 'a: b': '@scope/pkg' } }))
    expect(y).toContain('      "a: b": "@scope/pkg"')
    expect(y).toContain('      k: "v\\n  evil: 1"')
    expect(y).not.toContain('\n  evil')
  })
})
