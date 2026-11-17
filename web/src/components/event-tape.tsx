import { useEffect, useMemo, useRef, type KeyboardEvent } from 'react'
import { Link } from '@tanstack/react-router'
import type { EventRecord } from '@/api/types'
import { RelativeTime } from '@/components/badges'
import { EmptyState } from '@/components/states'
import { Stamp } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { bumpTone } from '@/lib/release'

export type TapeRange = '24h' | '7d' | '30d'

export const RANGES: { value: TapeRange; label: string; ms: number }[] = [
  { value: '24h', label: '24 hours', ms: 24 * 3600_000 },
  { value: '7d', label: '7 days', ms: 7 * 24 * 3600_000 },
  { value: '30d', label: '30 days', ms: 30 * 24 * 3600_000 },
]

/** Holes closer than this share of the tape are moved to other lanes so each stays clickable. */
const CROWD = 0.025
const LANES = ['50%', '26%', '74%']

type HoleKind = 'published' | 'prerelease' | 'yanked'

function holeKind(type: string): HoleKind {
  if (type.includes('yanked')) return 'yanked'
  if (type.includes('prerelease')) return 'prerelease'
  return 'published'
}

const HOLE_CLASS: Record<HoleKind, string> = {
  published: 'bg-[#14123a]',
  prerelease: 'bg-transparent shadow-[inset_0_0_0_3px_#14123a]',
  yanked: 'bg-[#c2361d]',
}

function utc(iso: string) {
  return new Date(iso).toISOString().slice(0, 16).replace('T', ' ') + ' UTC'
}

/** The tick labels under the tape: hours for a day, days for longer ranges. */
function ticks(start: number, end: number, range: TapeRange): { at: number; label: string }[] {
  const out: { at: number; label: string }[] = []
  if (range === '24h') {
    const first = Math.ceil(start / (6 * 3600_000)) * 6 * 3600_000
    for (let t = first; t <= end; t += 6 * 3600_000) out.push({ at: t, label: new Date(t).toISOString().slice(11, 16) })
  } else {
    const step = range === '7d' ? 1 : 5
    const day = 24 * 3600_000
    const first = Math.ceil(start / day) * day
    for (let t = first, i = 0; t <= end; t += day, i++) {
      if (i % step) continue
      const d = new Date(t)
      const day = d.getUTCDate()
      const label =
        range === '7d'
          ? `${d.toLocaleDateString('en', { weekday: 'short', timeZone: 'UTC' })} ${day}`
          : `${day} ${d.toLocaleDateString('en', { month: 'short', timeZone: 'UTC' })}`
      out.push({ at: t, label })
    }
  }
  return out
}

/**
 * The punched tape: one hole per release in the range, placed by time. A solid hole is a
 * release, a ring a prerelease, a red hole a yank. Arrow keys move along the tape.
 */
export function EventTape({
  events,
  range,
  now,
  selected,
  onSelect,
}: {
  events: EventRecord[]
  range: TapeRange
  now: number
  selected?: number
  onSelect: (seq: number) => void
}) {
  const span = RANGES.find((r) => r.value === range)!.ms
  const start = now - span
  const holes = useMemo(() => {
    const inRange = events.filter((r) => Date.parse(r.event.time) >= start).sort((a, b) => Date.parse(a.event.time) - Date.parse(b.event.time))
    let lane = 0
    let prev = -1
    return inRange.map((r) => {
      const x = Math.min(1, Math.max(0, (Date.parse(r.event.time) - start) / span))
      lane = prev >= 0 && x - prev < CROWD ? (lane + 1) % LANES.length : 0
      prev = x
      return { record: r, x, lane }
    })
  }, [events, start, span])
  const ref = useRef<HTMLDivElement>(null)
  const index = holes.findIndex((h) => h.record.seq === selected)

  // On a narrow screen the tape scrolls sideways: keep the selected hole in view.
  useEffect(() => {
    const hole = ref.current?.querySelector<HTMLElement>('[aria-selected=true]')
    hole?.scrollIntoView?.({ block: 'nearest', inline: 'center' })
  }, [selected, range])

  function onKey(e: KeyboardEvent<HTMLDivElement>) {
    if (!holes.length) return
    const moves: Record<string, number> = { ArrowRight: 1, ArrowLeft: -1, Home: -Infinity, End: Infinity }
    if (!(e.key in moves)) return
    e.preventDefault()
    const step = moves[e.key] ?? 0
    const from = index < 0 ? holes.length - 1 : index
    const next = step === Infinity ? holes.length - 1 : step === -Infinity ? 0 : Math.min(holes.length - 1, Math.max(0, from + step))
    const hole = holes[next]
    if (!hole) return
    onSelect(hole.record.seq)
    ref.current?.querySelectorAll<HTMLElement>('[role=option]')[next]?.focus()
  }

  return (
    <div className="overflow-x-auto pb-1">
      <div className="min-w-[40rem] px-3">
        <div
          ref={ref}
          role="listbox"
          aria-label={`Releases of the last ${RANGES.find((r) => r.value === range)!.label}, oldest to newest`}
          aria-orientation="horizontal"
          onKeyDown={onKey}
          className="relative h-16 bg-primary shadow-[inset_0_0_0_1px_rgb(20_18_58/0.15)]"
        >
          <div
            aria-hidden="true"
            className="absolute inset-x-0 top-[29px] h-1.5 bg-[radial-gradient(circle,rgb(20_18_58/0.55)_1.3px,transparent_1.6px)] bg-[length:12px_6px]"
          />
          {holes.map((h, i) => {
            const e = h.record.event
            const isSel = h.record.seq === selected
            const kind = holeKind(e.type)
            return (
              <button
                key={h.record.seq}
                type="button"
                role="option"
                aria-selected={isSel}
                tabIndex={isSel || (index < 0 && i === holes.length - 1) ? 0 : -1}
                aria-label={`${e.data.package} ${e.data.version}, ${kind}, ${utc(e.time)}`}
                title={`${e.data.package} ${e.data.version}`}
                onClick={() => onSelect(h.record.seq)}
                className={cn(
                  'absolute -mt-[11px] -ml-[11px] size-[22px] rounded-full transition-transform duration-150 hover:scale-115 focus-visible:outline-offset-4',
                  HOLE_CLASS[kind],
                  isSel && 'scale-110 ring-[3px] ring-[#14123a] ring-offset-[3px] ring-offset-primary',
                )}
                style={{ left: `${h.x * 100}%`, top: LANES[h.lane] }}
              />
            )
          })}
        </div>
        <div aria-hidden="true" className="relative h-6 font-mono text-[0.75rem] text-muted-foreground">
          {ticks(start, now, range).map((t) => (
            <span
              key={t.at}
              className="absolute top-1.5 -translate-x-1/2 whitespace-nowrap before:absolute before:-top-1.5 before:left-1/2 before:h-1 before:border-l before:border-border"
              style={{ left: `${((t.at - start) / span) * 100}%` }}
            >
              {t.label}
            </span>
          ))}
        </div>
      </div>
    </div>
  )
}

export function TapeLegend() {
  const item = 'inline-flex items-center gap-1.5'
  return (
    <div className="flex flex-wrap gap-4 text-sm text-muted-foreground">
      <span className={item}>
        <span aria-hidden="true" className="size-3 rounded-full bg-[#14123a] ring-1 ring-border" />
        release
      </span>
      <span className={item}>
        <span aria-hidden="true" className="size-3 rounded-full shadow-[inset_0_0_0_2px_var(--foreground)]" />
        prerelease
      </span>
      <span className={item}>
        <span aria-hidden="true" className="size-3 rounded-full bg-[#c2361d]" />
        yanked
      </span>
    </div>
  )
}

/** The selected release as a bulletin slip on canary paper, next to its CloudEvent. */
export function Bulletin({ record }: { record: EventRecord }) {
  const e = record.event
  const d = e.data
  const p = e.type.includes('yanked') ? { tone: 'flash' as const, label: 'Yanked' } : bumpTone(d.bump)
  const labels = Object.entries(d.watch.labels ?? {})
  return (
    <section aria-label="Selected release" className="grid gap-4 lg:grid-cols-[minmax(0,5fr)_minmax(0,6fr)] lg:items-start">
      <div className="paper relative px-8 py-5 [background-image:radial-gradient(circle,rgb(42_38_20/0.28)_2.2px,transparent_2.6px),radial-gradient(circle,rgb(42_38_20/0.28)_2.2px,transparent_2.6px)] [background-position:3px_4px,calc(100%-3px)_4px] [background-repeat:repeat-y] [background-size:14px_18px]">
        <div className="flex items-start justify-between gap-3">
          <div className="font-mono text-[0.8rem] break-all text-muted-foreground">
            {d.source} · {d.package}
          </div>
          <Stamp tone={p.tone}>{p.label}</Stamp>
        </div>
        <div className="wire-title mt-2 mb-1 text-[3rem] break-all [font-stretch:62%]">{d.version}</div>
        <div className="font-mono text-sm">
          {d.prerelease ? 'prerelease · ' : ''}
          {d.bump ?? 'new'}
          {d.previous_version ? ` from ${d.previous_version}` : ''}
        </div>
        <dl className="mt-3 grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-sm">
          <dt className="wire-label pt-0.5 text-muted-foreground">Recorded</dt>
          <dd className="font-mono">
            {utc(e.time)} (<RelativeTime value={e.time} />)
          </dd>
          <dt className="wire-label pt-0.5 text-muted-foreground">Watch</dt>
          <dd className="font-mono">
            {d.watch.name ? (
              <Link to="/watches/$name" params={{ name: d.watch.name }} className="text-link underline">
                {d.watch.name}
              </Link>
            ) : (
              d.watch.id
            )}
          </dd>
          {labels.length > 0 && (
            <>
              <dt className="wire-label pt-0.5 text-muted-foreground">Labels</dt>
              <dd className="font-mono break-all">{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</dd>
            </>
          )}
          <dt className="wire-label pt-0.5 text-muted-foreground">Seq</dt>
          <dd className="font-mono">{record.seq}</dd>
        </dl>
        <div className="mt-4 border-t border-dashed pt-3">
          <Link to="/events/$id" params={{ id: e.id }} className="font-semibold text-link underline">
            Open the event and its deliveries
          </Link>
        </div>
      </div>
      <div className="min-w-0">
        <div className="flex justify-between bg-chrome-2 px-3 py-1.5 font-mono text-[0.75rem] tracking-[0.08em] text-canary uppercase">
          <span>CloudEvent</span>
          <span>seq {record.seq}</span>
        </div>
        <pre
          tabIndex={0}
          aria-label="CloudEvent JSON"
          className="max-h-[28rem] overflow-auto border-t-[3px] border-primary bg-code p-4 font-mono text-xs leading-relaxed text-code-foreground"
        >
          {JSON.stringify(e, null, 2)}
        </pre>
      </div>
    </section>
  )
}

/** The quiet state: an empty range still shows the tape, and says when the newest release was. */
export function QuietTape({ newest, rangeLabel }: { newest?: EventRecord; rangeLabel: string }) {
  return (
    <EmptyState title={`No releases in the last ${rangeLabel}`}>
      {newest ? (
        <>
          The newest one, {newest.event.data.package} {newest.event.data.version}, came <RelativeTime value={newest.event.time} />. Pick a longer
          range to see it on the tape.
        </>
      ) : (
        'Releases appear here as watches detect them.'
      )}
    </EmptyState>
  )
}
