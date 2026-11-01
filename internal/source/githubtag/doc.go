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
// pages until a short one (at most 10 pages). The ETag of the first page is
// kept in FetchState.ETag and sent as If-None-Match; an authenticated 304
// does not count against GitHub's rate limit. The tags endpoint has no
// creation order, so a new tag can sit on a later page than the one the ETag
// covers; List therefore ignores the validators and walks every page when the
// last such scan is 24 hours old or missing.
//
// # Publication date
//
// A tag has no date of its own in the tags listing. For a tag that is new
// (not in the cursor), List makes one extra request, GET
// {base}/repos/{owner}/{repo}/git/commits/{sha}, and uses the committer date
// of the tagged commit as PublishedAt (the date of an annotated tag object is
// not exposed by the listing). Known tags cost no extra request and are
// returned with a zero PublishedAt: the core already holds their first
// sighting. A commit that GitHub cannot serve (404, 409, 422) leaves
// PublishedAt zero; any other lookup error fails List without advancing the
// state, so the tag is retried. At most 50 lookups are made per List call
// (a first scan sees every tag as new); tags past that bound are returned
// without a date.
//
// # Cursor
//
// FetchState.Cursor is JSON {"tags":[...],"full_at":unix}: the sorted names
// of the tags already reported and the time of the last validator-free scan.
// A tag that is deleted and created again is new again. A tag that is moved
// to another commit keeps its name and is not reported again.
//
// # Tokens
//
// The token is read at request time from WithToken or WithTokenEnv; a
// configured but empty token is an error, not an anonymous request. It is sent
// only as an Authorization header, and only over https (or to a loopback
// host). It is held in a redacting type and never appears in an error or a
// log. The injected client is wrapped so that a redirect carrying the token
// to another host (including another port or a subdomain) or to plain http
// fails instead of being followed.
package githubtag
