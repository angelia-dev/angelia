import type { components } from './schema'

type S = components['schemas']

export type Me = S['Me']
export type UiRole = S['UiRole']
export type Watch = S['Watch']
export type WatchPolicy = S['WatchPolicy']
export type Version = S['Version']
export type EventRecord = S['EventRecord']
export type Delivery = S['Delivery']
export type DeliveryStatus = S['DeliveryStatus']
export type Sink = S['Sink']
export type Subscription = S['Subscription']
export type Consumer = S['Consumer']
export type Plugin = S['Plugin']
export type Problem = S['Problem']
export type ShortEventType = S['ShortEventType']

/** The release event types; the envelope's `type` is an open set (spec docs/release-event-v1.md). */
export const RELEASE_EVENT_TYPES = ['dev.angelia.release.published.v1', 'dev.angelia.release.yanked.v1', 'dev.angelia.release.prerelease.v1'] as const
/** The short types of the release events, the ones a request may name (spec ShortEventType). */
export const RELEASE_SHORT_TYPES: ShortEventType[] = ['release.published', 'release.yanked', 'release.prerelease']
export type ReleaseData = S['v1']
/** An event of one of the release types, whose `data` follows the release data schema. */
export type ReleaseEventRecord = EventRecord & {
  event: EventRecord['event'] & { type: (typeof RELEASE_EVENT_TYPES)[number]; data: ReleaseData }
}

/**
 * Whether the event is a release event. New event types can appear in a minor release of the
 * spec, and consumers must ignore events whose type they do not know: the UI skips them.
 */
export function isReleaseRecord(r: EventRecord): r is ReleaseEventRecord {
  return (RELEASE_EVENT_TYPES as readonly string[]).includes(r.event.type)
}
