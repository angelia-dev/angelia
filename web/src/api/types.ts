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
