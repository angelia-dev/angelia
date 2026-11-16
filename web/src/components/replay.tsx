import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createReplay, fetchCapped, fetchSubscriptions, replayDelivery, type ReplaySelector } from '@/api/queries'
import type { Delivery } from '@/api/types'
import { DeliveryStatusBadge } from '@/components/badges'
import { Dialog } from '@/components/dialog'
import { TextFilter } from '@/components/filters'
import { ErrorState } from '@/components/states'
import { Button } from '@/components/ui/button'

export const isReplayable = (d: Delivery) => d.status === 'dead' || d.status === 'delivered'

function useInvalidateDeliveries() {
  const qc = useQueryClient()
  return () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: ['deliveries'] }),
      qc.invalidateQueries({ queryKey: ['delivery'] }),
    ])
}

/** Confirms, then replays one delivery; shows the delivery as it is after the replay. */
export function ReplayDeliveryDialog({ delivery, onClose }: { delivery: Delivery; onClose: () => void }) {
  const invalidate = useInvalidateDeliveries()
  const mutation = useMutation({ mutationFn: () => replayDelivery(delivery.id), onSuccess: () => invalidate() })
  const queued = mutation.data
  return (
    <Dialog title="Replay delivery" onClose={onClose}>
      {queued ? (
        <div role="status" className="space-y-2 text-sm">
          <p>The delivery is queued again.</p>
          <p>
            <Link to="/deliveries/$id" params={{ id: queued.id }} className="underline" onClick={onClose}>
              Delivery {queued.id}
            </Link>{' '}
            is now <DeliveryStatusBadge status={queued.status} />, attempts so far: {queued.attempts}.
          </p>
        </div>
      ) : (
        <p className="text-sm">
          Queue delivery <code>{delivery.id}</code> of subscription{' '}
          <strong>{delivery.subscription_name ?? '(deleted)'}</strong> again? It is sent with the same event and
          idempotency key, so a receiver that deduplicates may drop it.
        </p>
      )}
      {mutation.isError && (
        <div className="mt-3">
          <ErrorState error={mutation.error} />
        </div>
      )}
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="outline" onClick={onClose}>
          {queued ? 'Close' : 'Cancel'}
        </Button>
        {!queued && (
          <Button disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Replaying…' : 'Replay'}
          </Button>
        )}
      </div>
    </Dialog>
  )
}

/** A short, human description of what a range replay selects. */
export function describeSelector(s: ReplaySelector): string {
  const parts: string[] = []
  if (s.event) parts.push(`event ${s.event}`)
  if (s.subscription) parts.push(`subscription ${s.subscription}`)
  if (s.since && s.until) parts.push(`event time from ${s.since} to ${s.until}`)
  parts.push(`status ${s.status.join(' or ')}`)
  return parts.join(', ')
}

const toIso = (local: string) => (local ? new Date(local).toISOString() : undefined)

const dateInput = 'h-9 rounded-md border border-border bg-card px-3 text-sm'

/** "Replay range": the API's `createReplay`, with its dry run first. */
export function ReplayRangeDialog({ onClose }: { onClose: () => void }) {
  const invalidate = useInvalidateDeliveries()
  const [mode, setMode] = useState<'event' | 'range'>('event')
  const [event, setEvent] = useState('')
  const [subscription, setSubscription] = useState('')
  const [since, setSince] = useState('')
  const [until, setUntil] = useState('')
  const [dead, setDead] = useState(true)
  const [delivered, setDelivered] = useState(false)
  const [selector, setSelector] = useState<ReplaySelector>()
  const subs = useQuery({
    queryKey: ['subscription-names'],
    queryFn: async () => (await fetchCapped(fetchSubscriptions(), 5)).items,
    staleTime: 60_000,
  })

  const dry = useMutation({ mutationFn: (s: ReplaySelector) => createReplay({ ...s, dry_run: true }) })
  const real = useMutation({
    mutationFn: (s: ReplaySelector) => createReplay({ ...s, dry_run: false }),
    onSuccess: () => invalidate(),
  })

  const status = [...(dead ? (['dead'] as const) : []), ...(delivered ? (['delivered'] as const) : [])]
  const valid =
    status.length > 0 && (mode === 'event' ? event.trim() !== '' : subscription.trim() !== '' && since !== '' && until !== '')

  function count() {
    if (!valid) return
    const s: ReplaySelector =
      mode === 'event'
        ? { event: event.trim(), status, dry_run: true }
        : { subscription: subscription.trim(), since: toIso(since), until: toIso(until), status, dry_run: true }
    setSelector(s)
    dry.mutate(s)
  }

  const result = real.data
  const counted = dry.data
  return (
    <Dialog title="Replay range" onClose={onClose}>
      {result ? (
        <div role="status" className="text-sm">
          <p>
            {result.replayed} {result.replayed === 1 ? 'delivery' : 'deliveries'} queued again.
          </p>
          {result.truncated && (
            <p className="text-muted-foreground">Only the first {result.deliveries.length} are listed.</p>
          )}
        </div>
      ) : selector && counted ? (
        <div className="space-y-2 text-sm">
          <p role="status">
            {counted.replayed} {counted.replayed === 1 ? 'delivery' : 'deliveries'} would be replayed.
          </p>
          <p className="text-muted-foreground">Filter: {describeSelector(selector)}.</p>
        </div>
      ) : (
        <form
          aria-label="Replay selector"
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            count()
          }}
        >
          <fieldset className="flex gap-4 text-sm">
            <legend className="sr-only">Select by</legend>
            <label className="flex items-center gap-1">
              <input type="radio" name="mode" checked={mode === 'event'} onChange={() => setMode('event')} /> Event id
            </label>
            <label className="flex items-center gap-1">
              <input type="radio" name="mode" checked={mode === 'range'} onChange={() => setMode('range')} /> Time range
            </label>
          </fieldset>
          {mode === 'event' ? (
            <TextFilter label="Event id" value={event} onChange={setEvent} placeholder="01JBX2A7C8D9E0F1G2H3J4K5M6" />
          ) : (
            <>
              <div className="flex flex-col gap-1">
                <label htmlFor="replay-sub" className="text-xs font-medium text-muted-foreground">
                  Subscription (name or id)
                </label>
                <input
                  id="replay-sub"
                  list="replay-subs"
                  className={dateInput}
                  value={subscription}
                  onChange={(e) => setSubscription(e.target.value)}
                />
                <datalist id="replay-subs">
                  {(subs.data ?? []).map((s) => (
                    <option key={s.id} value={s.name} />
                  ))}
                </datalist>
              </div>
              <div className="flex flex-col gap-1">
                <label htmlFor="replay-since" className="text-xs font-medium text-muted-foreground">
                  Event time from (inclusive)
                </label>
                <input id="replay-since" type="datetime-local" className={dateInput} value={since} onChange={(e) => setSince(e.target.value)} />
              </div>
              <div className="flex flex-col gap-1">
                <label htmlFor="replay-until" className="text-xs font-medium text-muted-foreground">
                  Event time until (exclusive)
                </label>
                <input id="replay-until" type="datetime-local" className={dateInput} value={until} onChange={(e) => setUntil(e.target.value)} />
              </div>
            </>
          )}
          <fieldset className="flex gap-4 text-sm">
            <legend className="mb-1 text-xs font-medium text-muted-foreground">Replay deliveries that are</legend>
            <label className="flex items-center gap-1">
              <input type="checkbox" checked={dead} onChange={(e) => setDead(e.target.checked)} /> Dead
            </label>
            <label className="flex items-center gap-1">
              <input type="checkbox" checked={delivered} onChange={(e) => setDelivered(e.target.checked)} /> Delivered
            </label>
          </fieldset>
        </form>
      )}
      {(dry.isError || real.isError) && (
        <div className="mt-3">
          <ErrorState error={real.error ?? dry.error} />
        </div>
      )}
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="outline" onClick={onClose}>
          {result ? 'Close' : 'Cancel'}
        </Button>
        {!result && !counted && (
          <Button disabled={!valid || dry.isPending} onClick={count}>
            {dry.isPending ? 'Counting…' : 'Count deliveries'}
          </Button>
        )}
        {!result && counted && selector && (
          <>
            <Button
              variant="outline"
              onClick={() => {
                dry.reset()
                real.reset()
                setSelector(undefined)
              }}
            >
              Back
            </Button>
            <Button disabled={counted.replayed === 0 || real.isPending} onClick={() => real.mutate(selector)}>
              {real.isPending ? 'Replaying…' : `Replay ${counted.replayed}`}
            </Button>
          </>
        )}
      </div>
    </Dialog>
  )
}
