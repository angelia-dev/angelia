import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import type { Versioned } from '@/api/problem'
import { Button } from '@/components/ui/button'
import { ErrorState, Skeleton } from '@/components/states'

/**
 * Reads the current version of a resource (its body and `ETag`) before an edit form opens, so
 * the save can send `If-Match` and never overwrite a change made since. `children` is rendered
 * with that version; `reload` reads it again and opens a fresh form (the input is dropped).
 */
export function VersionedEdit<T>({
  kind,
  name,
  fetch,
  children,
}: {
  kind: string
  name: string
  fetch: (name: string) => Promise<Versioned<T>>
  children: (v: Versioned<T>, reload: () => void) => ReactNode
}) {
  const query = useQuery({
    queryKey: ['versioned', kind, name],
    queryFn: () => fetch(name),
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  if (query.isPending) return <Skeleton className="mb-6 h-40 w-full" />
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  // Keyed by the tag, so a reload that finds a new version starts a new form.
  return <div key={query.data.etag ?? 'none'}>{children(query.data, () => void query.refetch())}</div>
}

/** The 412 of an edit: someone else saved first. The user's input stays until they reload. */
export function ChangedElsewhere({ kind, onReload }: { kind: string; onReload: () => void }) {
  return (
    <div role="alert" tabIndex={-1} className="rounded-lg border border-destructive p-3 text-sm">
      <p className="font-semibold text-destructive">Changed by someone else</p>
      <p className="mt-1">
        This {kind} was changed by someone else since you opened the form, so nothing was saved. Reload to edit the current
        version; your changes here are kept until you do.
      </p>
      <Button type="button" variant="outline" size="sm" className="mt-2" onClick={onReload}>
        Reload {kind}
      </Button>
    </div>
  )
}
