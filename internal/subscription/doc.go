// Package subscription matches release events to subscription filters.
//
// The filter shape is the approved watch-file one (docs/watch-file.md
// section 2.5): source, labels, bump, package and type. All given keys must
// match; an absent key matches anything, so the zero Filter matches every
// event. Matching is pure: no I/O, no clock, no allocation per match.
//
// Input is the minimal view of an event that matching needs. The event model
// (#106) does not exist yet; when it does, it maps onto Input (one small
// conversion in the dispatcher), and this package does not import it.
package subscription
