import {
  createRootRoute,
  createRoute,
  createRouter,
  type RouterHistory,
} from '@tanstack/react-router'
import { useMe } from '@/api/me'
import { Layout } from '@/layout'
import { PlaceholderPage } from '@/pages/placeholder'
import { SignInPage } from '@/pages/sign-in'

/** The root asks who is signed in: the sign-in page on 401, else the app shell. */
function Root() {
  const me = useMe()
  if (me.isPending) {
    return (
      <p role="status" className="p-6 text-sm text-muted-foreground">
        Loading…
      </p>
    )
  }
  if (me.isError) {
    if (me.error.name === 'NotSignedInError') return <SignInPage />
    return (
      <p role="alert" className="p-6 text-sm text-destructive">
        Could not reach the Angelia API: {me.error.message}
      </p>
    )
  }
  return <Layout me={me.data} />
}

const rootRoute = createRootRoute({ component: Root })

const pages = [
  ['/', 'Overview'],
  ['/watches', 'Watches'],
  ['/events', 'Events'],
  ['/deliveries', 'Deliveries'],
  ['/sinks', 'Sinks'],
  ['/subscriptions', 'Subscriptions'],
  ['/admin', 'Admin'],
] as const

const routeTree = rootRoute.addChildren(
  pages.map(([path, title]) =>
    createRoute({ getParentRoute: () => rootRoute, path, component: () => <PlaceholderPage title={title} /> }),
  ),
)

export function createAppRouter(history?: RouterHistory) {
  return createRouter({ routeTree, basepath: '/ui', history })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
