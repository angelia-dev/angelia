import { useProviders, loginPath } from '@/api/providers'
import { ErrorState, Skeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

/** One button per configured identity provider (GitHub, OIDC), from GET /ui/auth/providers. */
export function SignInPage() {
  const providers = useProviders()
  return (
    <main className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>Angelia</CardTitle>
          <CardDescription>Sign in to manage watches, sinks and deliveries.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-2">
          {providers.isPending ? (
            <Skeleton className="h-9 w-full" />
          ) : providers.isError ? (
            <ErrorState error={providers.error} onRetry={() => void providers.refetch()} />
          ) : providers.data.length === 0 ? (
            <p role="alert" className="text-sm">
              No sign-in provider is configured on this instance.
            </p>
          ) : (
            providers.data.map((p) => (
              <Button key={p.name} asChild className="w-full">
                <a href={loginPath(p.name)}>Sign in with {p.label}</a>
              </Button>
            ))
          )}
        </CardContent>
      </Card>
    </main>
  )
}
