import type { Watch } from '@/api/types'

const PLAIN = /^[A-Za-z0-9_./@][A-Za-z0-9_./@+-]*$/
const RESERVED = /^(true|false|yes|no|on|off|null|y|n|~)$/i
const NUMBERISH = /^[-+]?(\d[\d_]*)?\.?\d*([eE][-+]?\d+)?$|^0[xob]/i

/** A YAML scalar: plain when that is unambiguous, else a JSON string (valid double-quoted YAML). */
export function scalar(v: string): string {
  if (PLAIN.test(v) && !RESERVED.test(v) && !NUMBERISH.test(v)) return v
  return JSON.stringify(v)
}

function map(entries: [string, string][], indent: string): string[] {
  return entries.map(([k, v]) => `${indent}${scalar(k)}: ${scalar(v)}`)
}

function list(values: readonly string[]): string {
  return values.length === 0 ? '[]' : `[${values.map(scalar).join(', ')}]`
}

/**
 * The watch as a watch-file document (docs/watch-file.md): one `watches[]` entry
 * with the keys the file takes, so it can be moved to GitOps as it is.
 */
export function watchToYaml(
  w: Pick<Watch, 'name' | 'source' | 'coordinates' | 'interval' | 'scheme' | 'labels' | 'policy'>,
): string {
  const p = w.policy
  const lines = ['version: 1', 'watches:', `  - name: ${scalar(w.name)}`, `    source: ${scalar(w.source)}`, '    coordinates:']
  lines.push(...map(Object.entries(w.coordinates), '      '))
  lines.push(`    interval: ${scalar(w.interval)}`, `    scheme: ${scalar(w.scheme)}`)
  const labels = Object.entries(w.labels)
  if (labels.length) lines.push('    labels:', ...map(labels, '      '))
  lines.push(
    '    policy:',
    `      prerelease: ${p.prerelease ?? 'none'}`,
    `      include: ${list(p.include ?? [])}`,
    `      exclude: ${list(p.exclude ?? [])}`,
    `      constraint: ${scalar(p.constraint ?? '')}`,
    `      bump: ${list(p.bump ?? [])}`,
    `      cooldown: ${scalar(p.cooldown ?? '0')}`,
    `      baseline: ${p.baseline ?? true}`,
  )
  return lines.join('\n') + '\n'
}
