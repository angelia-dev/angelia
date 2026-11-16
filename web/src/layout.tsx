import { Link, Outlet } from '@tanstack/react-router'
import type { Me } from '@/api/me'
import { cn } from '@/lib/utils'

export const NAV_ITEMS = [
  { to: '/', label: 'Overview' },
  { to: '/watches', label: 'Watches' },
  { to: '/events', label: 'Events' },
  { to: '/deliveries', label: 'Deliveries' },
  { to: '/sinks', label: 'Sinks' },
  { to: '/subscriptions', label: 'Subscriptions' },
  { to: '/admin', label: 'Admin' },
] as const

function UserMenu({ me }: { me: Me }) {
  // Placeholder: the real menu (sign out, sessions) arrives with the sign-in story.
  return (
    <div className="flex items-center gap-2 text-sm text-muted-foreground" data-testid="user-menu">
      {me.user?.avatar_url && <img src={me.user.avatar_url} alt="" className="size-6 rounded-full" />}
      <span>{me.user?.login ?? me.name}</span>
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
            {NAV_ITEMS.map((item) => (
              <li key={item.to}>
                <Link
                  to={item.to}
                  activeOptions={{ exact: item.to === '/' }}
                  className={cn('block rounded-md px-3 py-2 text-sm hover:bg-muted')}
                  activeProps={{ className: 'bg-muted font-medium' }}
                >
                  {item.label}
                </Link>
              </li>
            ))}
          </ul>
        </nav>
        <main className="flex-1 p-4 md:p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
