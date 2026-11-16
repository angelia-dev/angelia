import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

export function PlaceholderPage({ title }: { title: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>This screen is not built yet.</CardDescription>
      </CardHeader>
      <CardContent className="text-sm text-muted-foreground">Coming in a later release of the admin UI.</CardContent>
    </Card>
  )
}
