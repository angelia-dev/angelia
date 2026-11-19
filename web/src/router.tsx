import {
  createRootRoute,
  createRoute,
  createRouter,
  type RouterHistory,
} from '@tanstack/react-router'
import { useMe } from '@/api/me'
import { Layout } from '@/layout'
import { DeliveriesPage, DeliveryDetailPage } from '@/pages/deliveries'
import { EventDetailPage, EventsPage } from '@/pages/events'
import { OverviewPage } from '@/pages/overview'
import { ConsumersPage, PluginsPage, SinksPage, SubscriptionsPage } from '@/pages/resources'
import { WatchDetailPage, WatchesPage } from '@/pages/watches'
import { SecretsPage } from '@/pages/secrets'
import { AdminPage } from '@/pages/admin'
import { ConfigurePage } from '@/pages/configure'
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

const route = <P extends string>(path: P, component: () => React.ReactNode) =>
  createRoute({ getParentRoute: () => rootRoute, path, component })

const routeTree = rootRoute.addChildren([
  route('/', OverviewPage),
  route('/watches', WatchesPage),
  route('/watches/$name', WatchDetailPage),
  route('/events', EventsPage),
  route('/events/$id', EventDetailPage),
  route('/deliveries', DeliveriesPage),
  route('/deliveries/$id', DeliveryDetailPage),
  route('/configure', ConfigurePage),
  route('/sinks', SinksPage),
  route('/subscriptions', SubscriptionsPage),
  route('/secrets', SecretsPage),
  route('/consumers', ConsumersPage),
  route('/plugins', PluginsPage),
  route('/admin', AdminPage),
])

export function createAppRouter(history?: RouterHistory) {
  return createRouter({ routeTree, basepath: '/ui', history })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
