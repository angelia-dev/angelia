// Build the npm snapshot for benchmark 6 (version ordering).
//
// Usage (from the repository root; see bench/ordering/README.md):
//
//   node bench/ordering/snapshot/npm.mjs \
//       bench/ordering/packages/npm.txt bench/ordering/testdata/npm.jsonl.gz
//
// For each package in the list it fetches the abbreviated packument from
// registry.npmjs.org, splits the version keys into the ones node-semver
// accepts in strict mode and the ones it rejects, and sorts the accepted ones
// with semver.compare: the precedence order npm itself uses when it picks a
// version (npm-pick-manifest calls semver.rcompare). Build metadata does not
// count for precedence, so "1.0.0+a" and "1.0.0+b" form one group.
//
// node-semver is the copy bundled with npm unless SEMVER_PATH points to
// another one (for example `npm install --no-save semver@7.7.4` and
// SEMVER_PATH=node_modules/semver).
//
// Output: gzip'd JSON lines, same format as pypi.py. Network: registry.npmjs.org only.

import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { join, resolve } from 'node:path'
import { gzipSync } from 'node:zlib'

const REGISTRY = 'https://registry.npmjs.org/'
const ACCEPT = 'application/vnd.npm.install-v1+json'
const USER_AGENT = 'angelia-bench-ordering/1 (+https://github.com/angelia-dev/angelia)'
const WORKERS = 8
const RETRIES = 3

function semverPath () {
  if (process.env.SEMVER_PATH) return resolve(process.env.SEMVER_PATH)
  const root = execFileSync('npm', ['root', '-g'], { encoding: 'utf8' }).trim()
  return join(root, 'npm', 'node_modules', 'semver')
}

const semverDir = semverPath()
const require = createRequire(join(semverDir, 'package.json'))
const semver = require('./index.js')
const semverVersion = JSON.parse(readFileSync(join(semverDir, 'package.json'), 'utf8')).version
if (!process.env.SEMVER_PATH) {
  console.error(`warning: SEMVER_PATH is not set; using node-semver ${semverVersion} bundled with the global npm (${semverDir}). ` +
    'Set SEMVER_PATH to pin the reference implementation.')
}

function readList (path) {
  const names = readFileSync(path, 'utf8').split('\n')
    .map(l => l.split('#')[0].trim())
    .filter(Boolean)
  return [...new Set(names)].sort()
}

async function fetchVersions (name) {
  const url = REGISTRY + name.replace('/', '%2F')
  for (let attempt = 0; attempt < RETRIES; attempt++) {
    try {
      const res = await fetch(url, { headers: { Accept: ACCEPT, 'User-Agent': USER_AGENT } })
      if (res.status === 404) return null
      if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`)
      const doc = await res.json()
      return Object.keys(doc.versions ?? {})
    } catch (err) {
      if (attempt === RETRIES - 1) throw err
      await new Promise(r => setTimeout(r, 1000 * 2 ** attempt))
    }
  }
  return null
}

function byteCompare (a, b) {
  return a < b ? -1 : a > b ? 1 : 0
}

function order (versions) {
  const valid = []
  const invalid = []
  for (const raw of versions) {
    if (semver.valid(raw, { loose: false }) !== null) valid.push(raw)
    else invalid.push(raw)
  }
  valid.sort((a, b) => semver.compare(a, b) || byteCompare(a, b))
  const groups = []
  for (const raw of valid) {
    const last = groups.at(-1)
    if (last && semver.compare(last[0], raw) === 0) last.push(raw)
    else groups.push([raw])
  }
  return { order: groups, invalid: invalid.sort(byteCompare) }
}

async function main (listPath, outPath) {
  const names = readList(listPath)
  const results = new Map()
  let next = 0
  await Promise.all(Array.from({ length: WORKERS }, async () => {
    while (next < names.length) {
      const name = names[next++]
      results.set(name, await fetchVersions(name))
    }
  }))
  const missing = names.filter(n => results.get(n) === null)
  const records = names.filter(n => results.get(n) !== null)
    .map(name => ({ name, ...order(results.get(name)) }))
  const meta = {
    registry: 'npm',
    source: 'https://registry.npmjs.org/ (abbreviated packument, version keys)',
    oracle: `node-semver ${semverVersion} (semver.compare, strict)`,
    generated: new Date().toISOString().slice(0, 10),
    packages: records.length,
    versions: records.reduce((n, r) => n + r.order.flat().length + r.invalid.length, 0),
    missing
  }
  const lines = [{ meta }, ...records].map(o => JSON.stringify(o) + '\n').join('')
  writeFileSync(outPath, gzipSync(lines, { level: 9 }))
  console.error(`npm: ${meta.packages} packages, ${meta.versions} versions, missing ${JSON.stringify(missing)}`)
}

if (process.argv.length !== 4) {
  console.error('usage: npm.mjs PACKAGE_LIST OUT.jsonl.gz')
  process.exit(2)
}
await main(process.argv[2], process.argv[3])
