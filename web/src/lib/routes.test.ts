import { describe, expect, it } from 'vitest'
import { sink, subscription, watch } from '@/test/fixtures'
import { configToYaml, cords, filterMatchesWatch, globToRegExp, routeName, watchPackage } from './routes'

describe('watchPackage', () => {
  it('follows the server: repo for GitHub, then the fallback keys', () => {
    expect(watchPackage({ source: 'github-release', coordinates: { repo: 'home-assistant/core' } })).toBe('home-assistant/core')
    expect(watchPackage({ source: 'pypi', coordinates: { name: 'requests' } })).toBe('requests')
    expect(watchPackage({ source: 'custom', coordinates: { b: 'y', a: 'x' } })).toBe('x/y')
  })
})

describe('globToRegExp', () => {
  it('matches like Go path.Match', () => {
    expect(globToRegExp('home-assistant/core*')!.test('home-assistant/core')).toBe(true)
    expect(globToRegExp('re*')!.test('requests')).toBe(true)
    expect(globToRegExp('*')!.test('a/b')).toBe(false)
    expect(globToRegExp('?ode')!.test('node')).toBe(true)
    expect(globToRegExp('[a-c]at')!.test('bat')).toBe(true)
    expect(globToRegExp('[^a-c]at')!.test('bat')).toBe(false)
    expect(globToRegExp('a.b')!.test('axb')).toBe(false)
    expect(globToRegExp('bad[')).toBeNull()
  })
})

describe('filterMatchesWatch and cords', () => {
  const ha = watch('home-assistant', { source: 'github-release', coordinates: { repo: 'home-assistant/core' }, labels: {} })
  const req = watch('requests')
  it('matches on source, labels and package', () => {
    const f = { source: [], labels: {}, package: '' }
    expect(filterMatchesWatch(f, ha)).toBe(true)
    expect(filterMatchesWatch({ ...f, source: ['pypi'] }, ha)).toBe(false)
    expect(filterMatchesWatch({ ...f, labels: { team: 'platform' } }, req)).toBe(true)
    expect(filterMatchesWatch({ ...f, labels: { team: 'web' } }, req)).toBe(false)
    expect(filterMatchesWatch({ ...f, package: 'home-assistant/*' }, ha)).toBe(true)
    expect(filterMatchesWatch({ ...f, package: 'home-assistant/*' }, req)).toBe(false)
  })
  it('lists every cord', () => {
    const all = subscription('all', 'hook')
    const pkg = { ...subscription('ha-only', 'hook', 'S2'), filter: { ...all.filter, package: 'home-assistant/core' } }
    expect(cords([ha, req], [all, pkg])).toEqual([
      { watch: 'home-assistant', subscription: 'all' },
      { watch: 'requests', subscription: 'all' },
      { watch: 'home-assistant', subscription: 'ha-only' },
    ])
  })
})

describe('routeName', () => {
  it('builds a free, valid name', () => {
    expect(routeName('Home Assistant', 'ci', new Set())).toBe('home-assistant-to-ci')
    expect(routeName('a', 'b', new Set(['a-to-b', 'a-to-b-2']))).toBe('a-to-b-3')
  })
})

describe('configToYaml', () => {
  it('writes a watch file apply accepts, defaults left out', () => {
    const s = { ...subscription('ha-to-ci', 'ci'), filter: { source: [], labels: {}, bump: ['major' as const], package: 'x*', type: ['release.published' as const] } }
    const k = sink('ci', { kind: 'github-dispatch', config: { repo: 'o/r', token_ref: 'gh', event_type: 'ha-release' } })
    const w = watch('ha', { source: 'github-release', coordinates: { repo: 'home-assistant/core' }, labels: {} })
    expect(configToYaml({ sinks: [k], watches: [w], subscriptions: [s] })).toBe(`version: 1

sinks:
  - name: ci
    kind: github-dispatch
    config:
      repo: o/r
      token_ref: gh
      event_type: ha-release

watches:
  - name: ha
    source: github-release
    coordinates:
      repo: home-assistant/core
    interval: 1h
    scheme: pep440
    policy:
      prerelease: none
      cooldown: 0s
      baseline: true

subscriptions:
  - name: ha-to-ci
    sink: ci
    filter:
      bump: [major]
      package: "x*"
`)
  })
})
