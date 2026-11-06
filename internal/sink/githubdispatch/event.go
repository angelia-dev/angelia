package githubdispatch

import "github.com/angelia-dev/angelia/internal/event"

// eventView is the part of the event that client_payload carries.
type eventView struct {
	id, source, pkg, version, watch  string
	previous, bump, purl, releaseURL *string
}

func viewOf(e event.Envelope) eventView {
	d := e.Data
	v := eventView{
		id: e.ID, source: d.Source, pkg: d.Package, version: d.Version,
		previous: d.PreviousVersion, bump: d.Bump, purl: d.PURL, releaseURL: d.URLs.Release,
		watch: d.Watch.ID,
	}
	if d.Watch.Name != nil {
		v.watch = *d.Watch.Name
	}
	return v
}
