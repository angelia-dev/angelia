import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

/** Where the GitHub sign-in starts: a browser redirect owned by the server (#262), not part of /v1. */
export const GITHUB_LOGIN_PATH = '/ui/auth/login/github'

export function SignInPage() {
  return (
    <main className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>Angelia</CardTitle>
          <CardDescription>Sign in to manage watches, sinks and deliveries.</CardDescription>
        </CardHeader>
        <CardContent>
          <Button asChild className="w-full">
            <a href={GITHUB_LOGIN_PATH}>Sign in with GitHub</a>
          </Button>
        </CardContent>
      </Card>
    </main>
  )
}
