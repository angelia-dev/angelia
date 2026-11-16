import { describe, expect, it } from 'vitest'
import { formatRelative } from './time'

const now = Date.parse('2026-11-16T12:00:00Z')

describe('formatRelative', () => {
  it.each([
    ['2026-11-16T11:57:00Z', '3 minutes ago'],
    ['2026-11-16T12:30:00Z', 'in 30 minutes'],
    ['2026-11-15T12:00:00Z', 'yesterday'],
    ['2026-11-16T09:00:00Z', '3 hours ago'],
    ['2026-11-16T12:00:00Z', 'now'],
  ])('%s -> %s', (iso, text) => {
    expect(formatRelative(iso, now)).toBe(text)
  })

  it('shows a dash for no timestamp and the raw text for an invalid one', () => {
    expect(formatRelative(null, now)).toBe('—')
    expect(formatRelative('garbage', now)).toBe('garbage')
  })
})
