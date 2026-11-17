import { useState } from 'react'
import { Link, Outlet } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { effectiveRole, hasRole, type Me, type UiRole } from '@/api/me'
import { Glyph, Tape } from '@/components/brand'

export const NAV_ITEMS: { to: string; label: string; min: UiRole }[] = [
  { to: '/', label: 'Overview', min: 'viewer' },
  { to: '/watches', label: 'Watches', min: 'viewer' },
  { to: '/events', label: 'Events', min: 'viewer' },
  { to: '/deliveries', label: 'Deliveries', min: 'viewer' },
  { to: '/sinks', label: 'Sinks', min: 'viewer' },
  { to: '/subscriptions', label: 'Subscriptions', min: 'viewer' },
  { to: '/secrets', label: 'Secrets', min: 'admin' },
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
    <div className="flex items-center gap-2.5 text-sm text-chrome-muted" data-testid="user-menu">
      {me.user?.avatar_url && <img src={me.user.avatar_url} alt="" className="size-7 rounded-full" />}
      <span className="font-semibold text-chrome-foreground max-sm:sr-only">{login}</span>
      <span
        aria-label={`Role: ${effectiveRole(me)}`}
        className="rounded-[2px] bg-canary px-1.5 py-px font-mono text-[0.72rem] font-bold tracking-[0.1em] text-chrome uppercase"
      >
        {effectiveRole(me)}
      </span>
      {me.kind === 'session' && (
        <button
          type="button"
          disabled={busy}
          onClick={() => void onSignOut()}
          className="h-8 rounded-[2px] px-3 font-semibold text-chrome-foreground shadow-[inset_0_0_0_1.5px_var(--chrome-muted)] transition-colors hover:text-canary hover:shadow-[inset_0_0_0_1.5px_var(--canary)] disabled:opacity-50"
        >
          Sign out
        </button>
      )}
      {error && (
        <span role="alert" className="text-canary">
          {error}
        </span>
      )}
    </div>
  )
}

export function Layout({ me }: { me: Me }) {
  return (
    <div className="flex min-h-screen flex-col">
      <header className="bg-chrome text-chrome-foreground">
        <div className="flex flex-wrap items-center gap-x-5 gap-y-1 px-4 pt-2.5 md:flex-nowrap md:px-6">
          <Link to="/" className="flex items-center gap-2.5 py-1 text-white no-underline" aria-label="Angelia, overview">
            <Glyph stroke="#fff" className="h-7 w-6" />
            <span className="wire-title text-[1.3rem] [font-stretch:72%]">Angelia</span>
          </Link>
          <div className="order-2 ml-auto md:order-3">
            <UserMenu me={me} />
          </div>
          <nav aria-label="Main" className="order-3 -mx-4 w-[calc(100%+2rem)] overflow-x-auto px-2 [scrollbar-width:none] md:order-2 md:mx-0 md:w-auto md:flex-1 md:px-0">
            <ul className="flex gap-0.5">
              {NAV_ITEMS.filter((item) => hasRole(me, item.min)).map((item) => (
                <li key={item.to}>
                  <Link
                    to={item.to}
                    activeOptions={{ exact: item.to === '/' }}
                    className="block px-2.5 pt-2 pb-2.5 text-[0.94rem] font-semibold whitespace-nowrap text-chrome-muted no-underline [font-stretch:82%] hover:text-canary"
                    activeProps={{ className: 'text-canary! shadow-[inset_0_-3px_0_var(--primary)]', 'aria-current': 'page' }}
                  >
                    {item.label}
                  </Link>
                </li>
              ))}
            </ul>
          </nav>
        </div>
      </header>
      <Tape />
      <main className="mx-auto w-full max-w-[90rem] min-w-0 flex-1 px-4 py-6 md:px-6">
        <Outlet />
      </main>
    </div>
  )
}
