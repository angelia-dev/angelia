import { useRef, useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { hasRole, useMe } from '@/api/me'
import { deleteSecret, invalidateSecrets, putSecret, secretExists, type SecretWrite } from '@/api/mutations'
import { fetchSecrets } from '@/api/secrets'
import { useCursorList } from '@/api/queries'
import type { components } from '@/api/schema'
import { RelativeTime } from '@/components/badges'
import {
  Checkbox,
  ConfirmDelete,
  Field,
  inputClass,
  ProblemSummary,
  TextArea,
  TextInput,
  errorsFor,
  mergeErrors,
  problemFieldErrors,
  useFocusFirstError,
  type FieldErrors,
} from '@/components/form'
import { EmptyState, Head, ListFrame, PageHeader } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableRow } from '@/components/ui/table'
import { NAME_RE } from '@/lib/watch-form'

type Secret = components['schemas']['Secret']

const MAX_VALUE_BYTES = 64 * 1024
const BASE64_RE = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/

export const parseHosts = (text: string) =>
  text
    .split(/[\n,]/)
    .map((h) => h.trim())
    .filter(Boolean)

/** Client-side checks of a secret write; the value is checked for size and shape, never echoed. */
export function validateSecret(f: { name: string; value: string; base64: boolean; hosts: string }, creating: boolean): FieldErrors {
  const e: FieldErrors = {}
  const add = (p: string, m: string) => (e[p] ??= []).push(m)
  if (creating && !NAME_RE.test(f.name)) add('/name', 'Use 1 to 63 characters: lower-case letters, digits, "." "_" "-", starting with a letter or digit.')
  const valuePointer = f.base64 ? '/value_base64' : '/value'
  if (f.value === '') add(valuePointer, 'Enter the secret value.')
  else if (f.base64) {
    if (f.value.length < 4 || !BASE64_RE.test(f.value)) add(valuePointer, 'Not valid padded base64.')
    else if ((f.value.length / 4) * 3 - (f.value.match(/=+$/)?.[0].length ?? 0) > MAX_VALUE_BYTES) add(valuePointer, 'At most 64 KiB once decoded.')
  } else if (new TextEncoder().encode(f.value).length > MAX_VALUE_BYTES) add(valuePointer, 'At most 64 KiB.')
  const hosts = parseHosts(f.hosts)
  if (hosts.length > 32) add('/hosts', 'At most 32 hosts.')
  if (new Set(hosts).size !== hosts.length) add('/hosts', 'A host is listed twice.')
  for (const h of hosts) {
    if (h.length > 261) add('/hosts', `"${h}" is too long.`)
    else if (h.includes('*')) add('/hosts', `"${h}": wildcards are not allowed.`)
    else if (/[/\s@?#]/.test(h)) add('/hosts', `"${h}": give a host or host:port, not a URL.`)
    else if (h !== h.toLowerCase()) add('/hosts', `"${h}": use lower case.`)
  }
  return e
}

const SECRET_POINTERS = ['/name', '/value', '/value_base64', '/hosts']

/**
 * Create or replace a secret. The value lives only in the password input, which is uncontrolled:
 * it is never React state (so no `value` attribute in the DOM, nothing in dev tools) and is
 * cleared the moment the form is submitted. The write is a plain call, not a mutation or a
 * query, so TanStack Query never holds the request body. Nothing returned by the API contains a value.
 */
function SecretForm({ initial, onDone, onCancel }: { initial?: Secret; onDone: () => void; onCancel: () => void }) {
  const creating = !initial
  const client = useQueryClient()
  const [name, setName] = useState(initial?.name ?? '')
  const valueRef = useRef<HTMLInputElement>(null)
  const formRef = useRef<HTMLFormElement>(null)
  const [base64, setBase64] = useState(false)
  const [hosts, setHosts] = useState(initial?.hosts.join('\n') ?? '')
  const [clientErrors, setClientErrors] = useState<FieldErrors>({})
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const focusError = useFocusFirstError(formRef, error !== undefined && !busy)

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    const input = valueRef.current
    const value = input?.value ?? ''
    const found = validateSecret({ name, value, base64, hosts }, creating)
    setClientErrors(found)
    if (Object.keys(found).length > 0) {
      focusError()
      return
    }
    const body: SecretWrite = { hosts: parseHosts(hosts), ...(base64 ? { value_base64: value } : { value }) }
    if (input) input.value = '' // cleared on submit, whatever the answer: retype it to try again
    setError(undefined)
    setBusy(true)
    try {
      if (creating && (await secretExists(name))) {
        setError(new NameTaken())
        return
      }
      await putSecret(initial?.name ?? name, body)
      await invalidateSecrets(client)
      onDone()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  const taken: FieldErrors = error instanceof NameTaken ? { '/name': ['A secret with this name already exists. Use Replace in the list.'] } : {}
  const errors = mergeErrors(clientErrors, problemFieldErrors(error), taken)
  const title = creating ? 'New secret' : `Replace ${initial.name}`
  const valuePointer = base64 ? '/value_base64' : '/value'

  return (
    <Card className="mb-6">
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <form ref={formRef} aria-label={title} noValidate onSubmit={(e) => void onSubmit(e)} className="flex flex-col gap-4" autoComplete="off">
          <p className="text-sm text-muted-foreground">
            The value is encrypted at rest and write-only: it is never shown again, by this page or by the API. The field is cleared
            when you submit.
          </p>
          <TextInput label="Name" required value={name} disabled={!creating} onChange={setName} errors={errorsFor(errors, '/name')} />
          <Field
            label={creating ? 'Value' : 'New value'}
            required
            errors={[...errorsFor(errors, valuePointer), ...errorsFor(errors, base64 ? '/value' : '/value_base64')]}
            hint="Up to 64 KiB. Nothing is trimmed."
          >
            {(p) => (
              <input
                {...p}
                ref={valueRef}
                type="password"
                className={inputClass}
                defaultValue=""
                autoComplete="new-password"
                spellCheck={false}
                autoCapitalize="off"
                autoCorrect="off"
              />
            )}
          </Field>
          <Checkbox label="The value is base64-encoded bytes" checked={base64} onChange={setBase64} />
          <TextArea
            label="Hosts"
            value={hosts}
            onChange={setHosts}
            errors={errorsFor(errors, '/hosts')}
            hint="Optional host binding, one per line: host or host:port, lower case, no wildcards. A bound secret is only sent to these hosts. A secret used for a source or registry credential must be bound."
          />
          <ProblemSummary error={error instanceof NameTaken ? undefined : error} claimed={SECRET_POINTERS} />
          <div className="flex gap-2">
            <Button type="submit" disabled={busy}>
              {busy ? 'Saving…' : creating ? 'Store secret' : 'Replace secret'}
            </Button>
            <Button type="button" variant="outline" disabled={busy} onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}

class NameTaken extends Error {}

function Referrers({ secret }: { secret: Secret }) {
  if (secret.referenced_by.length === 0) return <span aria-label="not referenced">—</span>
  return (
    <ul className="text-xs">
      {secret.referenced_by.map((r) => (
        <li key={`${r.resource}/${r.name}${r.pointer}`}>
          {r.resource} <strong>{r.name}</strong> <code>{r.pointer}</code>
        </li>
      ))}
    </ul>
  )
}

function DeleteSecret({ secret }: { secret: Secret }) {
  const client = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteSecret(secret.name),
    onSuccess: () => invalidateSecrets(client),
  })
  return (
    <ConfirmDelete
      what={`secret ${secret.name}`}
      consequence={secret.referenced_by.length ? 'It is still referenced, so the API will refuse until the references are removed.' : 'Anything that named it will fail until it is stored again.'}
      busy={remove.isPending}
      error={remove.error}
      onConfirm={() => remove.mutate()}
    />
  )
}

export function SecretsPage() {
  const me = useMe()
  const isAdmin = me.data ? hasRole(me.data, 'admin') : false
  const list = useCursorList(['secrets', 'list'], fetchSecrets())
  const [form, setForm] = useState<'new' | Secret | null>(null)

  if (me.data && !isAdmin) {
    return (
      <p role="alert" className="text-sm">
        This screen is for administrators.
      </p>
    )
  }
  if (!me.data) return null

  return (
    <>
      <PageHeader title="Secrets">{!form && <Button onClick={() => setForm('new')}>New secret</Button>}</PageHeader>
      {form && (
        <SecretForm
          key={form === 'new' ? 'new' : form.name}
          initial={form === 'new' ? undefined : form}
          onDone={() => setForm(null)}
          onCancel={() => setForm(null)}
        />
      )}
      <ListFrame
        label="secrets"
        query={list}
        count={list.items.length}
        columns={6}
        empty={<EmptyState title="No secrets stored">Store a secret to name it from a sink with a *_ref key.</EmptyState>}
      >
        <Table aria-label="Secrets">
          <Head columns={['Name', 'Hosts', 'Key id', 'Updated', 'Referenced by', 'Actions']} />
          <TableBody>
            {list.items.map((s) => (
              <TableRow key={s.name}>
                <TableHead scope="row" className="font-medium">
                  {s.name}
                </TableHead>
                <TableCell>{s.hosts.length ? s.hosts.join(', ') : 'unbound'}</TableCell>
                <TableCell>
                  <code>{s.key_id}</code>
                </TableCell>
                <TableCell>
                  <RelativeTime value={s.updated_at} />
                </TableCell>
                <TableCell>
                  <Referrers secret={s} />
                </TableCell>
                <TableCell>
                  <div className="flex flex-col gap-2">
                    <Button variant="outline" size="sm" onClick={() => setForm(s)}>
                      Replace
                      <span className="sr-only"> secret {s.name}</span>
                    </Button>
                    <DeleteSecret secret={s} />
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </>
  )
}
