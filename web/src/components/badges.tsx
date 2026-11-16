import { Badge } from '@/components/ui/badge'
import type { DeliveryStatus } from '@/api/types'
import { formatRelative } from '@/lib/time'

/** Relative time with the exact timestamp in the tooltip and `dateTime`. */
export function RelativeTime({ value }: { value: string | null | undefined }) {
  if (!value) return <span aria-label="none">—</span>
  return (
    <time dateTime={value} title={value}>
      {formatRelative(value)}
    </time>
  )
}

const STATUS: Record<DeliveryStatus, { label: string; symbol: string; variant: 'default' | 'secondary' | 'outline' | 'destructive' }> = {
  pending: { label: 'pending', symbol: '◔', variant: 'secondary' },
  in_flight: { label: 'in flight', symbol: '↻', variant: 'outline' },
  delivered: { label: 'delivered', symbol: '✓', variant: 'default' },
  dead: { label: 'dead letter', symbol: '✕', variant: 'destructive' },
}

/** Status as text plus a symbol: colour is never the only signal. */
export function DeliveryStatusBadge({ status }: { status: DeliveryStatus }) {
  const s = STATUS[status]
  return (
    <Badge variant={s.variant}>
      <span aria-hidden="true" className="mr-1">
        {s.symbol}
      </span>
      {s.label}
    </Badge>
  )
}

export const WATCH_FILE_DOCS = 'https://github.com/angelia-dev/angelia/blob/main/docs/watch-file.md'

/** `file` rows are read-only in the UI; their badge links to the watch-file docs. */
export function ManagedByBadge({ value }: { value: 'api' | 'file' }) {
  if (value === 'file') {
    return (
      <Badge variant="outline" title="Managed by the config file">
        <a href={WATCH_FILE_DOCS} target="_blank" rel="noreferrer" aria-label="file: managed by the watch file (opens the docs)">
          file
        </a>
      </Badge>
    )
  }
  return (
    <Badge variant="outline" title="Managed through the API">
      {value}
    </Badge>
  )
}

export function ErrorCount({ count }: { count: number }) {
  if (count === 0) return <span>0</span>
  return (
    <Badge variant="destructive">
      <span aria-hidden="true" className="mr-1">
        ⚠
      </span>
      {count} {count === 1 ? 'error' : 'errors'}
    </Badge>
  )
}

export function LabelList({ labels }: { labels: Record<string, string> }) {
  const entries = Object.entries(labels)
  if (entries.length === 0) return <span aria-label="no labels">—</span>
  return (
    <ul className="flex flex-wrap gap-1">
      {entries.map(([k, v]) => (
        <li key={k}>
          <Badge variant="secondary">
            {k}={v}
          </Badge>
        </li>
      ))}
    </ul>
  )
}
