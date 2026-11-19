import { useEffect, useRef, useState, type PointerEvent as ReactPointerEvent } from 'react'
import type { Sink, Subscription, Watch } from '@/api/types'
import { cords as computeCords } from '@/lib/routes'
import { cn } from '@/lib/utils'

/** What the board shows as selected, and what a click selects. */
export type BoardItem = { kind: 'watch' | 'sink' | 'subscription'; name: string }

// Geometry: x in percent of the board's width, y in pixels. Columns: watches 0-30, routes 40-60,
// sinks 70-100; the cords run in the two gaps.
const ROW = 92
const GAP = 14
const COL = { watch: [0, 30], route: [40, 60], sink: [70, 100] } as const
const rowY = (i: number) => i * (ROW + GAP)
const midY = (i: number) => rowY(i) + ROW / 2

function curve(x1: number, y1: number, x2: number, y2: number) {
  const mx = (x1 + x2) / 2
  return `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`
}

function destination(s: Sink): string {
  const c = s.config
  for (const k of ['repo', 'url', 'url_env', 'service']) if (typeof c[k] === 'string') return c[k]
  return s.kind
}

function filterSummary(s: Subscription): string {
  const f = s.filter
  const parts: string[] = []
  if (f.package) parts.push(`package ${f.package}`)
  if (f.source.length) parts.push(f.source.join(', '))
  const labels = Object.entries(f.labels)
  if (labels.length) parts.push(labels.map(([k, v]) => `${k}=${v}`).join(' '))
  if (f.bump.length) parts.push(f.bump.join('+'))
  return parts.length ? parts.join(' · ') : 'every release'
}

function OwnerTag({ value }: { value: 'api' | 'file' }) {
  return (
    <span
      className={cn(
        'rounded-[2px] border-[1.5px] border-current px-1 font-mono text-[0.62rem] leading-[1.4] font-bold tracking-[0.1em] uppercase',
        value === 'file' ? 'text-canary' : 'text-[#6fd49a]',
      )}
      title={value === 'file' ? 'Managed by the watch file: read-only here until moved to the UI' : 'Managed through the API and the UI'}
    >
      {value === 'file' ? 'file' : 'ui'}
    </span>
  )
}

interface Drag {
  watch: string
  fromY: number
  x: number
  y: number
}

/**
 * The patch bay: watches on the left, routes (subscriptions) in the middle, sinks on the right,
 * joined by cords. A cord from a watch to a route means the route's filter matches the watch
 * today. Editors drag from a watch's jack to a sink to start a new route; the jack is also a
 * button, so the keyboard can start one too.
 */
export function PatchBay({
  watches,
  sinks,
  subscriptions,
  selected,
  onSelect,
  onRoute,
  canRoute,
}: {
  watches: Watch[]
  sinks: Sink[]
  subscriptions: Subscription[]
  selected: BoardItem | null
  onSelect: (item: BoardItem) => void
  onRoute?: (watch: string, sink?: string) => void
  canRoute: boolean
}) {
  const boardRef = useRef<HTMLDivElement>(null)
  const [drag, setDrag] = useState<Drag | null>(null)
  const [hoverSink, setHoverSink] = useState<string | null>(null)

  const rows = Math.max(watches.length, sinks.length, subscriptions.length, 1)
  const height = rowY(rows) - GAP
  const wIndex = new Map(watches.map((w, i) => [w.name, i]))
  const sIndex = new Map(subscriptions.map((s, i) => [s.name, i]))
  const kIndex = new Map(sinks.map((k, i) => [k.name, i]))
  const links = computeCords(watches, subscriptions)

  const isSel = (kind: BoardItem['kind'], name: string) => selected?.kind === kind && selected.name === name
  const selWatch = selected?.kind === 'watch' ? selected.name : null
  const selSub = selected?.kind === 'subscription' ? selected.name : null
  const selSink = selected?.kind === 'sink' ? selected.name : null
  const lit = (watch: string | null, sub: string, sink: string | null) =>
    (selWatch !== null && watch === selWatch) || selSub === sub || (selSink !== null && sink === selSink)

  // While dragging, follow the pointer anywhere on the page and finish on release.
  useEffect(() => {
    if (!drag) return
    const sinkUnder = (x: number, y: number) =>
      (document.elementFromPoint?.(x, y)?.closest('[data-sink-name]') as HTMLElement | null)?.dataset.sinkName ?? null
    const move = (e: PointerEvent) => {
      const r = boardRef.current?.getBoundingClientRect()
      if (!r) return
      setDrag((d) => d && { ...d, x: ((e.clientX - r.left) / r.width) * 100, y: e.clientY - r.top })
      setHoverSink(sinkUnder(e.clientX, e.clientY))
    }
    const up = (e: PointerEvent) => {
      const target = sinkUnder(e.clientX, e.clientY)
      setDrag(null)
      setHoverSink(null)
      if (target) onRoute?.(drag.watch, target)
    }
    const cancel = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setDrag(null)
        setHoverSink(null)
      }
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
    window.addEventListener('keydown', cancel)
    return () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      window.removeEventListener('keydown', cancel)
    }
  }, [drag, onRoute])

  function startDrag(e: ReactPointerEvent, watch: string, i: number) {
    if (!canRoute || e.button !== 0) return
    e.preventDefault()
    setDrag({ watch, fromY: midY(i), x: COL.watch[1], y: midY(i) })
  }

  return (
    <div className="overflow-x-auto pb-2">
      <div className="min-w-[760px]">
        <div className="mb-2 grid grid-cols-[30%_10%_20%_10%_30%] border-t-[3px] border-chrome pt-2 dark:border-canary">
          <span className="wire-label text-muted-foreground">Watches · {watches.length}</span>
          <span />
          <span className="wire-label text-center text-muted-foreground">Routes · {subscriptions.length}</span>
          <span />
          <span className="wire-label text-right text-muted-foreground">Sinks · {sinks.length}</span>
        </div>
        <div ref={boardRef} className={cn('relative', drag && 'cursor-grabbing select-none')} style={{ height }} data-testid="patch-bay">
          <svg aria-hidden="true" className="pointer-events-none absolute inset-0 h-full w-full overflow-visible" viewBox={`0 0 100 ${height}`} preserveAspectRatio="none">
            {links.map((c) => {
              const wi = wIndex.get(c.watch)!
              const si = sIndex.get(c.subscription)!
              const on = lit(c.watch, c.subscription, null)
              return (
                <path
                  key={`${c.watch}>${c.subscription}`}
                  d={curve(COL.watch[1], midY(wi), COL.route[0], midY(si))}
                  className={on ? 'stroke-gold' : 'stroke-chrome dark:stroke-chrome-muted'}
                  strokeWidth={on ? 4 : 2.5}
                  fill="none"
                  vectorEffect="non-scaling-stroke"
                />
              )
            })}
            {subscriptions.map((s, si) => {
              const ki = kIndex.get(s.sink)
              if (ki === undefined) return null
              const on = lit(null, s.name, s.sink) || (selWatch !== null && links.some((c) => c.watch === selWatch && c.subscription === s.name))
              return (
                <path
                  key={`${s.name}>${s.sink}`}
                  d={curve(COL.route[1], midY(si), COL.sink[0], midY(ki))}
                  className={on ? 'stroke-gold' : 'stroke-chrome dark:stroke-chrome-muted'}
                  strokeWidth={on ? 4 : 2.5}
                  fill="none"
                  vectorEffect="non-scaling-stroke"
                />
              )
            })}
            {drag && (
              <path
                d={curve(COL.watch[1], drag.fromY, drag.x, drag.y)}
                className="stroke-gold-ink"
                strokeWidth={3}
                strokeDasharray="8 6"
                fill="none"
                vectorEffect="non-scaling-stroke"
              />
            )}
          </svg>

          {watches.map((w, i) => (
            <div key={w.id} className="absolute" style={{ left: `${COL.watch[0]}%`, width: `${COL.watch[1] - COL.watch[0]}%`, top: rowY(i), height: ROW }}>
              <button
                type="button"
                onClick={() => onSelect({ kind: 'watch', name: w.name })}
                aria-pressed={isSel('watch', w.name)}
                className={cn(
                  'flex h-full w-full flex-col overflow-hidden bg-[#1e1b4b] text-left text-canary transition-shadow',
                  isSel('watch', w.name) && 'shadow-[0_0_0_3px_var(--gold)]',
                  w.status.error_count > 0 && 'shadow-[0_0_0_2px_var(--destructive)]',
                )}
              >
                <span className="flex items-center gap-2 bg-chrome px-3 py-1.5">
                  <span className="wire-label truncate text-chrome-muted">{w.source}</span>
                  <span className="ml-auto">
                    <OwnerTag value={w.managed_by} />
                  </span>
                </span>
                <span className="flex min-w-0 flex-col px-3 py-1.5">
                  <span className="wire-subtitle truncate text-[1.05rem]">{w.name}</span>
                  <span className="truncate font-mono text-[0.72rem] text-chrome-foreground">{Object.values(w.coordinates).join(' · ')}</span>
                </span>
              </button>
              {canRoute && (
                <button
                  type="button"
                  aria-label={`Route ${w.name} to a sink`}
                  title="Drag to a sink to route this watch; or press to start a route"
                  onPointerDown={(e) => startDrag(e, w.name, i)}
                  onClick={() => onRoute?.(w.name)}
                  className="absolute top-1/2 -right-2.5 size-5 -translate-y-1/2 cursor-grab touch-none rounded-full border-[3px] border-primary bg-chrome transition-transform hover:scale-125"
                />
              )}
            </div>
          ))}

          {subscriptions.map((s, i) => (
            <button
              key={s.id}
              type="button"
              onClick={() => onSelect({ kind: 'subscription', name: s.name })}
              aria-pressed={isSel('subscription', s.name)}
              className={cn(
                'paper absolute flex flex-col justify-center gap-1 overflow-hidden px-3 text-left',
                isSel('subscription', s.name) ? 'outline-[3px] outline-gold outline-solid' : 'outline-1 outline-[#e9d995] outline-solid',
              )}
              style={{ left: `${COL.route[0]}%`, width: `${COL.route[1] - COL.route[0]}%`, top: rowY(i) + 10, height: ROW - 20 }}
            >
              <span className="flex items-center gap-1.5">
                <span className="truncate font-mono text-[0.78rem] font-bold">{s.name}</span>
                {s.managed_by === 'file' && <span className="font-mono text-[0.6rem] font-bold text-gold-ink uppercase">file</span>}
              </span>
              <span className="truncate font-mono text-[0.68rem] text-muted-foreground">{filterSummary(s)}</span>
            </button>
          ))}

          {sinks.map((k, i) => (
            <div
              key={k.id}
              data-sink-name={k.name}
              className="absolute"
              style={{ left: `${COL.sink[0]}%`, width: `${COL.sink[1] - COL.sink[0]}%`, top: rowY(i), height: ROW }}
            >
              <span aria-hidden="true" className="absolute top-1/2 -left-2 size-4 -translate-y-1/2 rounded-full border-[3px] border-primary bg-chrome" />
              <button
                type="button"
                onClick={() => onSelect({ kind: 'sink', name: k.name })}
                aria-pressed={isSel('sink', k.name)}
                className={cn(
                  'flex h-full w-full flex-col overflow-hidden bg-[#1e1b4b] text-left text-canary transition-shadow',
                  (isSel('sink', k.name) || hoverSink === k.name) && 'shadow-[0_0_0_3px_var(--gold)]',
                )}
              >
                <span className="flex items-center gap-2 bg-chrome px-3 py-1.5">
                  <span className="wire-label truncate text-chrome-muted">{k.kind}</span>
                  <span className="ml-auto">
                    <OwnerTag value={k.managed_by} />
                  </span>
                </span>
                <span className="flex min-w-0 flex-col px-3 py-1.5">
                  <span className="wire-subtitle truncate text-[1.05rem]">{k.name}</span>
                  <span className="truncate font-mono text-[0.72rem] text-chrome-foreground">{destination(k)}</span>
                </span>
              </button>
            </div>
          ))}
        </div>
        <p className="mt-4 border-t border-border pt-3 text-xs text-muted-foreground">
          A cord means a route's filter matches that watch today. A route is a filter, not a fixed link: a new watch that matches it
          joins it on its own.{canRoute && ' Drag from a watch’s gold jack to a sink to add a route.'}
        </p>
      </div>
    </div>
  )
}
