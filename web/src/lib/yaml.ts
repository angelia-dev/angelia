import type { Watch } from '@/api/types'

// `@` is a reserved indicator in YAML: it may appear inside a plain scalar but not start one.
const PLAIN = /^[A-Za-z0-9_./][A-Za-z0-9_./@+-]*$/
const RESERVED = /^(true|false|yes|no|on|off|null|y|n|~)$/i
const NUMBERISH = /^[-+]?(\d[\d_]*)?\.?\d*([eE][-+]?\d+)?$|^0[xob]|^[-+]?\.(inf|nan)$/i
// YAML 1.1 timestamps and sexagesimal-looking dates (`2024-01-02`) decode as non-strings in some parsers.
const DATEISH = /^\d{4}-\d\d?-\d\d?/
// Characters YAML does not allow raw in a stream or reads as line breaks; JSON.stringify leaves them as they are.
const UNSAFE = /[\u007F-\u009F\u2028\u2029\uFEFF\uFFFE\uFFFF]/g

/** A YAML scalar: plain when that is unambiguous, else a double-quoted string with every unsafe character escaped. */
export function scalar(v: string): string {
  if (PLAIN.test(v) && !RESERVED.test(v) && !NUMBERISH.test(v) && !DATEISH.test(v)) return v
  return JSON.stringify(v).replace(UNSAFE, (c) => '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0'))
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
