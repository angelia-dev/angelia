// Package githubtag is the "github-tag" Source: it lists the git tags of a
// GitHub (or GitHub Enterprise) repository that does not publish releases.
//
// Target coordinates: {"repo": "owner/name"}. Include and exclude patterns,
// prerelease mode and the rest of the filtering belong to policy, not to this
// source: every tag is returned, with Prerelease nil (a tag has no upstream
// flag).
//
// # Requests and rate limit
//
// List calls GET {base}/repos/{owner}/{repo}/tags?per_page=100, following
// pages until a short one. At most 10 pages (1000 tags) are read: a
// repository with more tags is truncated at that cap, and the tags beyond it
// are never reported. The ETag of the first page is kept in FetchState.ETag
// and sent as If-None-Match; an authenticated 304 does not count against
// GitHub's rate limit. The tags endpoint has no creation order, so a new tag
// can sit on a later page than the one the ETag covers; List therefore
// ignores the validators and walks every page when the last such scan is 24
// hours old or missing.
//
// # Publication date
//
// A tag has no date of its own in the tags listing. For a tag that is new
// (not in the cursor), List makes one extra request, GET
// {base}/repos/{owner}/{repo}/git/commits/{sha}, and uses the committer date
// of the tagged commit as PublishedAt (the date of an annotated tag object is
// not exposed by the listing). Known tags cost no extra request and are
// returned with a zero PublishedAt: the core already holds their first
// sighting.
//
// A first scan, with an empty or unreadable cursor, makes no lookup at all:
// its versions are a baseline (ADR 0010 section 5), their dates would be
// arbitrary and 50 requests would burn the anonymous rate limit. Later scans
// make at most 50 lookups per List call; tags past that bound are returned
// without a date and are not looked up again.
//
// A commit GitHub cannot serve (404, 409, 422) leaves PublishedAt zero. So
// does any other lookup failure (for example a 403 without rate-limit
// headers, or a 500): the tag is returned undated, one slog warning names
// the repository and the tag, and the state advances. Only a context error
// or a rate limit (a *netguard.StatusError with RetryAt set) fails List
// without advancing the state, so the lookups are retried later.
//
// # Cursor
//
// FetchState.Cursor is JSON {"tags":[...],"full_at":unix}: the sorted names
// of the tags already reported and the time of the last validator-free scan.
// It holds one name per tag, so at most 1000 names (tens of kilobytes for
// typical names, bounded by the 10-page cap). A tag that is deleted and created again
// leaves the cursor and comes back as new (one lookup); the core already
// holds the version and reports it as `returned`. A tag that is moved to
// another commit keeps its name and is not reported again.
//
// # Tokens
//
// The token is read once per List call from WithToken or WithTokenEnv; a
// configured but empty token is an error, not an anonymous request. It is sent
// only as an Authorization header, and only over https (or to a loopback
// host). It is held in a redacting type and never appears in an error or a
// log. The injected client is wrapped so that a redirect carrying the token
// to another host (including another port or a subdomain) or to plain http
// fails instead of being followed.
package githubtag
