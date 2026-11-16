import type { ReactNode } from 'react'
import { ApiError } from '@/api/problem'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden="true" className={cn('animate-pulse rounded bg-muted', className)} />
}

/** A table-shaped placeholder while the first page loads. */
export function TableSkeleton({ label, columns = 5, rows = 5 }: { label: string; columns?: number; rows?: number }) {
  return (
    <div role="status" aria-label={`Loading ${label}`}>
      <Table aria-hidden="true">
        <TableBody>
          {Array.from({ length: rows }, (_, r) => (
            <TableRow key={r}>
              {Array.from({ length: columns }, (_, c) => (
                <TableCell key={c}>
                  <Skeleton className="h-4 w-full" />
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <span className="sr-only">Loading {label}…</span>
    </div>
  )
}

export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-border p-8 text-center">
      <p className="font-medium">{title}</p>
      {children && <p className="mt-1 text-sm text-muted-foreground">{children}</p>}
    </div>
  )
}

/** Shows an RFC 9457 problem (`title` and `detail`), or the plain message of any other error. */
export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const problem = error instanceof ApiError ? error.problem : undefined
  const title = problem?.title ?? 'Something went wrong'
  const detail = problem?.detail ?? (error instanceof Error ? error.message : String(error))
  return (
    <div role="alert" className="rounded-lg border border-destructive p-4">
      <p className="font-semibold text-destructive">{title}</p>
      <p className="mt-1 text-sm">{detail}</p>
      {error instanceof ApiError && problem?.status === undefined && <p className="mt-1 text-xs">HTTP {error.status}</p>}
      {problem?.errors?.length ? (
        <ul className="mt-2 list-disc pl-5 text-sm">
          {problem.errors.map((e) => (
            <li key={e.pointer}>
              <code>{e.pointer}</code>: {e.detail}
            </li>
          ))}
        </ul>
      ) : null}
      {onRetry && (
        <Button variant="outline" size="sm" className="mt-3" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  )
}

export function PageHeader({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
      <h1 className="text-2xl font-semibold">{title}</h1>
      {children}
    </div>
  )
}

/** Column headers with scope="col", for accessible tables. */
export function Head({ columns }: { columns: string[] }) {
  return (
    <TableHeader>
      <TableRow>
        {columns.map((c) => (
          <TableHead key={c} scope="col">
            {c}
          </TableHead>
        ))}
      </TableRow>
    </TableHeader>
  )
}

interface ListQuery {
  isPending: boolean
  isError: boolean
  error: unknown
  refetch: () => unknown
  hasNextPage: boolean
  isFetchingNextPage: boolean
  fetchNextPage: () => unknown
}

/** The loading / error / empty / table / load-more frame shared by every list page. */
export function ListFrame({
  label,
  query,
  count,
  columns,
  empty,
  children,
}: {
  label: string
  query: ListQuery
  count: number
  columns: number
  empty: ReactNode
  children: ReactNode
}) {
  if (query.isPending) return <TableSkeleton label={label} columns={columns} />
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  if (count === 0 && !query.hasNextPage) return <>{empty}</>
  return (
    <>
      {count === 0 ? empty : children}
      {query.hasNextPage && (
        <div className="mt-4">
          <Button variant="outline" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
            {query.isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        </div>
      )}
    </>
  )
}
