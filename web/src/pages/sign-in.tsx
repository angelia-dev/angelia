import { useProviders, loginPath } from '@/api/providers'
import { ErrorState, Skeleton } from '@/components/states'
import { Glyph, Tape } from '@/components/brand'
import { Button } from '@/components/ui/button'

/** One button per configured identity provider (GitHub, OIDC), from GET /ui/auth/providers. */
export function SignInPage() {
  const providers = useProviders()
  return (
    <main className="flex min-h-screen flex-col bg-chrome text-chrome-foreground">
      <Tape />
      <div className="flex flex-1 items-center justify-center p-4">
        <div className="w-full max-w-sm">
          <div className="mb-6 flex items-center gap-3">
            <Glyph stroke="#fff" className="h-14 w-12" />
            <div>
              <h1 className="wire-title text-[2.6rem] text-white [font-stretch:66%]">Angelia</h1>
              <p className="text-chrome-muted">The self-hosted release event bus.</p>
            </div>
          </div>
          <div className="paper p-5">
            <p className="wire-label mb-3 text-gold-ink">Sign in</p>
            <p className="mb-4 text-sm">Sign in to manage watches, sinks and deliveries.</p>
            <div className="flex flex-col gap-2">
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
            </div>
          </div>
        </div>
      </div>
    </main>
  )
}
