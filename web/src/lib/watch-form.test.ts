import { describe, expect, it } from 'vitest'
import { newPair } from '@/components/form'
import { watch } from '@/test/fixtures'
import { emptyWatchForm, formToWrite, validateWatchForm, watchToForm } from './watch-form'

describe('watch form mapping', () => {
  it('maps a watch to the form and back to the same write body', () => {
    const w = watch('requests')
    w.policy = { prerelease: 'include', include: ['^2'], exclude: ['rc'], constraint: '>=2 <3', bump: ['minor'], cooldown: '24h', baseline: false }
    expect(formToWrite(watchToForm(w))).toEqual({
      source: 'pypi',
      coordinates: { package: 'requests' },
      interval: '1h',
      scheme: 'pep440',
      labels: { team: 'platform' },
      policy: w.policy,
    })
  })

  it('omits the scheme when it is left to the default, and splits regex lines', () => {
    const f = { ...emptyWatchForm(), name: 'x', source: 'npm', include: ' a \n\n b ', coordinates: [newPair('name', 'left-pad'), newPair('', 'dropped')] }
    const body = formToWrite(f)
    expect(body.scheme).toBeUndefined()
    expect(body.policy?.include).toEqual(['a', 'b'])
    expect(body.coordinates).toEqual({ name: 'left-pad' })
  })
})

describe('watch form validation', () => {
  const ok = { ...emptyWatchForm(), name: 'flask', source: 'pypi', coordinates: [newPair('package', 'flask')] }

  it('accepts a valid form', () => {
    expect(validateWatchForm(ok, { creating: true })).toEqual({})
  })

  it('mirrors the schema: name, source, coordinates, interval, cooldown, labels, counts', () => {
    const e = validateWatchForm(
      {
        ...emptyWatchForm(),
        name: 'Bad Name',
        interval: '1m',
        cooldown: '-1h',
        labels: [newPair('bad key', 'v'), newPair('a', 'x'), newPair('a', 'y')],
        include: Array.from({ length: 17 }, (_, i) => `r${i}`).join('\n'),
      },
      { creating: true },
    )
    expect(Object.keys(e).sort()).toEqual(['/coordinates', '/interval', '/labels', '/name', '/policy/cooldown', '/policy/include', '/source'])
    expect(e['/labels']).toHaveLength(2)
  })

  it('checks the interval range and syntax', () => {
    expect(validateWatchForm({ ...ok, interval: '721h' }, { creating: false })['/interval']).toBeDefined()
    expect(validateWatchForm({ ...ok, interval: 'soon' }, { creating: false })['/interval']).toBeDefined()
    expect(validateWatchForm({ ...ok, interval: '1h30m' }, { creating: false })).toEqual({})
  })

  it('requires the coordinates the source schema requires', () => {
    const e = validateWatchForm({ ...ok, coordinates: [newPair('package', ' ')] }, { creating: false, requiredCoordinates: ['package'] })
    expect(e['/coordinates/package']).toBeDefined()
  })

  it('refuses a coordinate key given twice instead of dropping one', () => {
    const e = validateWatchForm({ ...ok, coordinates: [newPair('package', 'flask'), newPair('package', 'django')] }, { creating: false })
    expect(e['/coordinates']).toEqual(['Coordinate "package" is given twice.'])
  })

  it('does not check the name when editing', () => {
    expect(validateWatchForm({ ...ok, name: '' }, { creating: false })).toEqual({})
  })
})
