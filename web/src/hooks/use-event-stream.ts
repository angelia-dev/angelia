import { useCallback, useEffect, useRef, useState } from 'react'
import type { EventFilters } from '@/api/queries'
import type { EventRecord } from '@/api/types'

export type StreamStatus = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'error' | 'unsupported'

export function streamUrl(filters: EventFilters, after: number): string {
  const q = new URLSearchParams({ after: String(after) })
  if (filters.watch) q.append('watch', filters.watch)
  if (filters.type) q.append('type', filters.type)
  if (filters.source) q.append('source', filters.source)
  return `/v1/events/stream?${q.toString()}`
}

/**
 * Live events from GET /v1/events/stream through EventSource (the session
 * cookie is sent; a bearer token could not be). While `enabled` is false the
 * connection is closed; turning it on again resumes after the last seq seen.
 * The browser reconnects by itself and sends Last-Event-ID; when it gives up
 * (the server refused the stream with a problem) the status becomes "error"
 * and `reconnect()` opens a new connection.
 */
export function useEventStream({
  filters,
  after,
  enabled,
}: {
  filters: EventFilters
  after: number
  enabled: boolean
}) {
  const [events, setEvents] = useState<EventRecord[]>([])
  const [status, setStatus] = useState<StreamStatus>('idle')
  const [attempt, setAttempt] = useState(0)
  const lastSeq = useRef(after)
  const baseAfter = useRef(after)
  baseAfter.current = after
  const { watch, type, source } = filters

  // A new filter set means a new timeline: forget what was streamed.
  useEffect(() => {
    setEvents([])
    lastSeq.current = 0
  }, [watch, type, source])

  useEffect(() => {
    if (!enabled) {
      setStatus('idle')
      return
    }
    if (typeof EventSource === 'undefined') {
      setStatus('unsupported')
      return
    }
    lastSeq.current = Math.max(lastSeq.current, baseAfter.current)
    setStatus('connecting')
    const es = new EventSource(streamUrl({ watch, type, source }, lastSeq.current), { withCredentials: true })
    es.onopen = () => setStatus('live')
    es.onmessage = (msg: MessageEvent<string>) => {
      let record: EventRecord
      try {
        record = JSON.parse(msg.data) as EventRecord
      } catch {
        return
      }
      if (record.seq <= lastSeq.current) return
      lastSeq.current = record.seq
      setEvents((prev) => [...prev, record])
    }
    es.onerror = () => setStatus(es.readyState === EventSource.CLOSED ? 'error' : 'reconnecting')
    return () => es.close()
  }, [enabled, watch, type, source, attempt])

  const reconnect = useCallback(() => setAttempt((n) => n + 1), [])
  return { events, status, reconnect }
}
