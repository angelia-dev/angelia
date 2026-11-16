// Fails when src/api/schema.d.ts is stale against the vendored OpenAPI spec:
// regenerates it into a temporary file and compares.
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const dir = mkdtempSync(join(tmpdir(), 'angelia-api-'))
const out = join(dir, 'schema.d.ts')
try {
  execFileSync('npx', ['--no-install', 'openapi-typescript', '../api/openapi/angelia.v1.yaml', '-o', out], {
    stdio: 'ignore',
  })
  if (readFileSync(out, 'utf8') !== readFileSync('src/api/schema.d.ts', 'utf8')) {
    console.error('src/api/schema.d.ts is stale: run `npm run gen:api` and commit the result')
    process.exit(1)
  }
  console.log('src/api/schema.d.ts is up to date')
} finally {
  rmSync(dir, { recursive: true, force: true })
}
