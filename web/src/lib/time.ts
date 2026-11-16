const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 31_557_600],
  ['month', 2_629_800],
  ['day', 86_400],
  ['hour', 3_600],
  ['minute', 60],
  ['second', 1],
]

/** "3 hours ago", "in 2 minutes", "now"; an em dash for a missing timestamp. No date library. */
export function formatRelative(iso: string | null | undefined, now: number = Date.now(), locale = 'en'): string {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return iso
  const diff = Math.round((t - now) / 1000)
  const rtf = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' })
  for (const [unit, secs] of UNITS) {
    if (Math.abs(diff) >= secs) return rtf.format(Math.round(diff / secs), unit)
  }
  return rtf.format(0, 'second')
}
