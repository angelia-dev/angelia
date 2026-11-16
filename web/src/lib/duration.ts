const UNIT: Record<string, number> = { ns: 1e-9, us: 1e-6, 'µs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 }

/** Seconds of a Go duration (`30m`, `1h30m`, `0`), or null when it is not one. */
export function parseDuration(text: string): number | null {
  const s = text.trim()
  if (s === '0' || s === '+0' || s === '-0') return 0
  const m = /^([-+]?)((?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+)$/.exec(s)
  if (!m) return null
  let total = 0
  for (const part of (m[2] as string).matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)) {
    total += Number(part[1]) * (UNIT[part[2] as string] as number)
  }
  return m[1] === '-' ? -total : total
}
