import { useState } from 'react'
import { Link, Outlet } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { effectiveRole, hasRole, type Me, type UiRole } from '@/api/me'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

export const NAV_ITEMS: { to: string; label: string; min: UiRole }[] = [
  { to: '/', label: 'Overview', min: 'viewer' },
  { to: '/watches', label: 'Watches', min: 'viewer' },
  { to: '/events', label: 'Events', min: 'viewer' },
  { to: '/deliveries', label: 'Deliveries', min: 'viewer' },
  { to: '/sinks', label: 'Sinks', min: 'viewer' },
  { to: '/subscriptions', label: 'Subscriptions', min: 'viewer' },
  { to: '/consumers', label: 'Consumers', min: 'viewer' },
  { to: '/plugins', label: 'Plugins', min: 'viewer' },
  { to: '/admin', label: 'Admin', min: 'admin' },
]

/** Ends the session (a browser-only route, outside /v1) and goes back to the sign-in page. */
export async function signOut(queryClient: ReturnType<typeof useQueryClient>) {
  const res = await fetch('/ui/auth/logout', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'X-Angelia-CSRF': '1' },
  })
  if (!res.ok && res.status !== 401) throw new Error(`sign out failed (${res.status})`)
  queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== 'me' })
  await queryClient.invalidateQueries({ queryKey: ['me'] })
}

function UserMenu({ me }: { me: Me }) {
  const queryClient = useQueryClient()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const login = me.user?.login ?? me.name

  async function onSignOut() {
    setBusy(true)
    setError(undefined)
    try {
      await signOut(queryClient)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'sign out failed')
      setBusy(false)
    }
  }

  return (
    <div className="flex items-center gap-3 text-sm" data-testid="user-menu">
      {me.user?.avatar_url && <img src={me.user.avatar_url} alt="" className="size-7 rounded-full" />}
      <span className="font-medium">{login}</span>
      <Badge variant="secondary" aria-label={`Role: ${effectiveRole(me)}`}>
        {effectiveRole(me)}
      </Badge>
      {me.kind === 'session' && (
        <Button variant="outline" size="sm" disabled={busy} onClick={() => void onSignOut()}>
          Sign out
        </Button>
      )}
      {error && (
        <span role="alert" className="text-destructive">
          {error}
        </span>
      )}
    </div>
  )
}

export function Layout({ me }: { me: Me }) {
  return (
    <div className="flex min-h-screen flex-col">
      <header className="flex h-14 items-center justify-between border-b border-border bg-card px-4">
        <span className="text-lg font-semibold">Angelia</span>
        <UserMenu me={me} />
      </header>
      <div className="flex flex-1 flex-col md:flex-row">
        <nav aria-label="Main" className="border-b border-border p-2 md:w-52 md:border-r md:border-b-0">
          <ul className="flex gap-1 overflow-x-auto md:flex-col">
            {NAV_ITEMS.filter((item) => hasRole(me, item.min)).map((item) => (
              <li key={item.to}>
                <Link
                  to={item.to}
                  activeOptions={{ exact: item.to === '/' }}
                  className={cn('block rounded-md px-3 py-2 text-sm hover:bg-muted')}
                  activeProps={{ className: 'bg-muted font-medium', 'aria-current': 'page' }}
                >
                  {item.label}
                </Link>
              </li>
            ))}
          </ul>
        </nav>
        <main className="min-w-0 flex-1 p-4 md:p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
