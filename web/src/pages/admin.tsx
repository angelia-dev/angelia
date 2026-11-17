import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { hasRole, effectiveRole, useMe, type Me } from '@/api/me'
import { createToken, fetchAccess, fetchSessions, fetchTokens, revokeSession, revokeToken } from '@/api/queries'
import { useCursorList } from '@/api/queries'
import type { components } from '@/api/schema'
import { RelativeTime } from '@/components/badges'
import { Dialog } from '@/components/dialog'
import { EmptyState, ErrorState, Head, ListFrame, PageHeader, Skeleton } from '@/components/states'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableRow } from '@/components/ui/table'

type Token = components['schemas']['Token']
type Session = components['schemas']['Session']
type Scope = components['schemas']['TokenScope']

const TABS = [
  { id: 'tokens', label: 'API tokens' },
  { id: 'sessions', label: 'Sessions' },
  { id: 'access', label: 'Access' },
] as const
type TabId = (typeof TABS)[number]['id']

const control =
  'h-9 rounded-[2px] border border-border bg-card px-3 text-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring'

function Forbidden() {
  return (
    <p role="alert" className="text-sm">
      This screen is for administrators.
    </p>
  )
}

/** The admin screen: API tokens, UI sessions and the (read-only) access list. */
export function AdminPage() {
  const me = useMe()
  const [tab, setTab] = useState<TabId>('tokens')
  if (me.isPending) return <Skeleton className="h-40 w-full" />
  if (!me.data || !hasRole(me.data, 'admin')) return <Forbidden />
  return (
    <>
      <PageHeader title="Admin" />
      <div role="tablist" aria-label="Admin sections" className="mb-4 flex gap-1 border-b border-border">
        {TABS.map((t) => (
          <button
            key={t.id}
            role="tab"
            id={`tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls={`panel-${t.id}`}
            className={`-mb-px border-b-2 px-3 py-2 text-sm ${tab === t.id ? 'border-primary font-medium' : 'border-transparent hover:bg-muted'}`}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`}>
        {tab === 'tokens' && <TokensTab />}
        {tab === 'sessions' && <SessionsTab me={me.data} />}
        {tab === 'access' && <AccessTab me={me.data} />}
      </div>
    </>
  )
}

// --- API tokens -------------------------------------------------------------

function tokenState(t: Token): string {
  if (t.revoked_at) return 'revoked'
  if (t.expires_at && Date.parse(t.expires_at) <= Date.now()) return 'expired'
  return 'active'
}

function TokensTab() {
  const [includeRevoked, setIncludeRevoked] = useState(false)
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<Token>()
  const list = useCursorList(['tokens', includeRevoked], fetchTokens(includeRevoked))
  return (
    <section aria-label="API tokens">
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={includeRevoked} onChange={(e) => setIncludeRevoked(e.target.checked)} />
          Include revoked
        </label>
        <Button onClick={() => setCreating(true)}>Create token</Button>
      </div>
      {creating && <CreateTokenDialog onClose={() => setCreating(false)} />}
      {revoking && <RevokeTokenDialog token={revoking} onClose={() => setRevoking(undefined)} />}
      <ListFrame
        label="tokens"
        query={list}
        count={list.items.length}
        columns={8}
        empty={<EmptyState title="No API tokens">Create one to call the API from scripts and CI.</EmptyState>}
      >
        <Table aria-label="API tokens">
          <Head columns={['Name', 'Scope', 'State', 'Created', 'Created by', 'Expires', 'Last used', 'Actions']} />
          <TableBody>
            {list.items.map((t) => {
              const state = tokenState(t)
              return (
                <TableRow key={t.id}>
                  <TableCell className="font-medium">{t.name}</TableCell>
                  <TableCell>{t.scope}</TableCell>
                  <TableCell>
                    <Badge variant={state === 'active' ? 'default' : 'secondary'}>{state}</Badge>
                  </TableCell>
                  <TableCell>
                    <RelativeTime value={t.created_at} />
                  </TableCell>
                  <TableCell>{t.created_by}</TableCell>
                  <TableCell>{t.expires_at ? <RelativeTime value={t.expires_at} /> : 'never'}</TableCell>
                  <TableCell>
                    <RelativeTime value={t.last_used_at} />
                  </TableCell>
                  <TableCell>
                    {!t.revoked_at && (
                      <Button variant="outline" size="sm" aria-label={`Revoke token ${t.name}`} onClick={() => setRevoking(t)}>
                        Revoke
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </ListFrame>
    </section>
  )
}

/**
 * Creates a token and shows its value once. The value lives only in this component's state
 * (never in the query cache, a mutation result or storage) and is dropped when the dialog closes.
 */
function CreateTokenDialog({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [scope, setScope] = useState<Scope>('read')
  const [expires, setExpires] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [secret, setSecret] = useState<string>()
  const [copied, setCopied] = useState<'yes' | 'no'>()

  const expiryInPast = expires !== '' && !(new Date(expires).getTime() > Date.now())

  async function submit() {
    setBusy(true)
    setError(undefined)
    try {
      const created = await createToken({
        name: name.trim(),
        scope,
        expires_at: expires ? new Date(expires).toISOString() : undefined,
      })
      setSecret(created.token)
      void qc.invalidateQueries({ queryKey: ['tokens'] })
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret ?? '')
      setCopied('yes')
    } catch {
      setCopied('no')
    }
  }

  if (secret !== undefined) {
    return (
      // Closing drops the secret for good: only the explicit button does it, never Escape or the backdrop.
      <Dialog title="Token created" onClose={onClose} dismissible={false}>
        <p role="alert" className="mb-3 rounded-[2px] border border-destructive p-3 text-sm">
          Copy this token now. It is shown only once and cannot be retrieved later.
        </p>
        <code data-testid="token-value" className="block break-all rounded-[2px] bg-muted p-3 text-sm">
          {secret}
        </code>
        <div className="mt-2 flex items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => void copy()}>
            Copy
          </Button>
          {copied === 'yes' && <span role="status" className="text-sm">Copied.</span>}
          {copied === 'no' && <span role="status" className="text-sm">Could not copy; select the token and copy it by hand.</span>}
        </div>
        <div className="mt-4 flex justify-end">
          <Button onClick={onClose}>I have copied it</Button>
        </div>
      </Dialog>
    )
  }
  return (
    <Dialog title="Create API token" onClose={onClose} dismissible={!busy}>
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim() && !expiryInPast && !busy) void submit()
        }}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor="token-name" className="text-xs font-medium text-muted-foreground">
            Name
          </label>
          <input id="token-name" className={control} value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="token-scope" className="text-xs font-medium text-muted-foreground">
            Scope
          </label>
          <select id="token-scope" className={control} value={scope} onChange={(e) => setScope(e.target.value as Scope)}>
            <option value="read">read</option>
            <option value="write">write</option>
            <option value="admin">admin</option>
          </select>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="token-expires" className="text-xs font-medium text-muted-foreground">
            Expires (optional)
          </label>
          <input id="token-expires" type="datetime-local" className={control} value={expires} onChange={(e) => setExpires(e.target.value)} />
        </div>
        {expiryInPast && (
          <p role="alert" className="text-sm text-destructive">
            The expiry must be in the future.
          </p>
        )}
        {error !== undefined && <ErrorState error={error} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={!name.trim() || expiryInPast || busy}>
            {busy ? 'Creating…' : 'Create'}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

function RevokeTokenDialog({ token, onClose }: { token: Token; onClose: () => void }) {
  const qc = useQueryClient()
  const m = useMutation({
    mutationFn: () => revokeToken(token.id),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['tokens'] })
      onClose()
    },
  })
  return (
    <Dialog title="Revoke token" onClose={onClose} dismissible={!m.isPending}>
      <p className="text-sm">
        Revoke token <strong>{token.name}</strong> ({token.scope})? Anything using it stops working immediately. This cannot be undone.
      </p>
      {m.isError && (
        <div className="mt-3">
          <ErrorState error={m.error} />
        </div>
      )}
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="destructive" disabled={m.isPending} onClick={() => m.mutate()}>
          {m.isPending ? 'Revoking…' : 'Revoke'}
        </Button>
      </div>
    </Dialog>
  )
}

// --- Sessions -----------------------------------------------------------------

/** This browser's session: `Me.session_id` names it (null for a token). */
export function isCurrentSession(me: Me, s: Session): boolean {
  return me.kind === 'session' && me.session_id != null && me.session_id === s.id
}

/** Another session of the signed-in person's own account. */
export function isOwnSession(me: Me, s: Session): boolean {
  return me.kind === 'session' && me.principal_id === `${s.user.provider}:${s.user.subject}`
}

function SessionsTab({ me }: { me: Me }) {
  const list = useCursorList(['sessions'], fetchSessions())
  const [revoking, setRevoking] = useState<Session>()
  return (
    <section aria-label="Sessions">
      <p className="mb-4 text-sm text-muted-foreground">
        Browser sessions of the admin UI. The one you are using is marked “this session”; other sessions of your account are
        marked “you”.
      </p>
      {revoking && (
        <RevokeSessionDialog
          session={revoking}
          current={isCurrentSession(me, revoking)}
          own={isOwnSession(me, revoking)}
          onClose={() => setRevoking(undefined)}
        />
      )}
      <ListFrame
        label="sessions"
        query={list}
        count={list.items.length}
        columns={6}
        empty={<EmptyState title="No active sessions" />}
      >
        <Table aria-label="Sessions">
          <Head columns={['User', 'Created', 'Last seen', 'User agent', 'Expires', 'Actions']} />
          <TableBody>
            {list.items.map((s) => (
              <TableRow key={s.id}>
                <TableCell>
                  <span className="font-medium">{s.user.login}</span>{' '}
                  {isCurrentSession(me, s) ? (
                    <Badge>this session</Badge>
                  ) : (
                    isOwnSession(me, s) && <Badge variant="secondary">you</Badge>
                  )}
                </TableCell>
                <TableCell>
                  <RelativeTime value={s.created_at} />
                </TableCell>
                <TableCell>
                  <RelativeTime value={s.last_seen_at} />
                </TableCell>
                <TableCell className="max-w-64 truncate" title={s.user_agent ?? undefined}>
                  {s.user_agent ?? '—'}
                </TableCell>
                <TableCell>
                  <RelativeTime value={s.expires_at} />
                </TableCell>
                <TableCell>
                  <Button variant="outline" size="sm" aria-label={`Revoke session of ${s.user.login} (${s.id})`} onClick={() => setRevoking(s)}>
                    Revoke
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ListFrame>
    </section>
  )
}

function RevokeSessionDialog({
  session,
  current,
  own,
  onClose,
}: {
  session: Session
  current: boolean
  own: boolean
  onClose: () => void
}) {
  const qc = useQueryClient()
  const m = useMutation({
    mutationFn: () => revokeSession(session.id),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['sessions'] })
      // If it was this browser's session, /v1/me now answers 401 and the app shows the sign-in page;
      // otherwise nothing else changes, so no other cache is touched.
      if (own || current) await qc.invalidateQueries({ queryKey: ['me'] })
      onClose()
    },
  })
  return (
    <Dialog title="Revoke session" onClose={onClose} dismissible={!m.isPending}>
      <p className="text-sm">
        Revoke the session of <strong>{session.user.login}</strong> signed in {new Date(session.created_at).toLocaleString()}? The
        browser is signed out at its next request.
      </p>
      {current ? (
        <p role="note" className="mt-2 text-sm font-medium">
          This is the session you are using now: you will be signed out.
        </p>
      ) : (
        own && (
          <p role="note" className="mt-2 text-sm font-medium">
            This is another session of your own account. You stay signed in here.
          </p>
        )
      )}
      {m.isError && (
        <div className="mt-3">
          <ErrorState error={m.error} />
        </div>
      )}
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="destructive" disabled={m.isPending} onClick={() => m.mutate()}>
          {m.isPending ? 'Revoking…' : 'Revoke'}
        </Button>
      </div>
    </Dialog>
  )
}

// --- Access -------------------------------------------------------------------

type AccessEntry = components['schemas']['AccessEntry']

function AccessTab({ me }: { me: Me }) {
  // The tab is only rendered for admins (AdminPage), so non-admins never call GET /v1/access.
  const access = useQuery({ queryKey: ['access'], queryFn: fetchAccess })
  return (
    <section aria-label="Access" className="space-y-3 text-sm">
      <p>
        Who may sign in to this UI, and with which role, is set in the instance config file under <code>ui.access</code>. It is
        read when Angelia starts, so a change needs a restart. This page cannot edit it.
      </p>
      <p>
        You are signed in as <strong>{me.user?.login ?? me.name}</strong> with the role <strong>{effectiveRole(me)}</strong>.
      </p>
      {access.isPending ? (
        <Skeleton className="h-24 w-full" />
      ) : access.isError ? (
        <ErrorState error={access.error} onRetry={() => void access.refetch()} />
      ) : access.data.items.length === 0 ? (
        <EmptyState title="No access entries" />
      ) : (
        <Table aria-label="Access list">
          <Head columns={['Provider', 'Kind', 'Login', 'Subject', 'Role']} />
          <TableBody>
            {access.data.items.map((e, i) => (
              <AccessRow key={`${e.provider}-${e.login ?? ''}-${e.subject ?? ''}-${i}`} entry={e} />
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}

function AccessRow({ entry: e }: { entry: AccessEntry }) {
  return (
    <TableRow>
      <TableCell>{e.provider}</TableCell>
      <TableCell>{e.kind}</TableCell>
      <TableCell>{e.login ?? '—'}</TableCell>
      <TableCell>
        {e.subject ?? <span className="text-muted-foreground">not signed in yet</span>}
        {e.pinned && (
          <Badge variant="outline" className="ml-2">
            pinned
          </Badge>
        )}
      </TableCell>
      <TableCell>{e.role}</TableCell>
    </TableRow>
  )
}
