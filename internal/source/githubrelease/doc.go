// Package githubrelease is the "github-release" Source: it lists the
// releases of a GitHub (or GitHub Enterprise) repository.
//
// Target coordinates: {"repo": "owner/name"}. A repository that was renamed is
// still served through GitHub's 301 redirect, which is followed, but the
// stored coordinate goes stale until it is edited.
//
// # Requests and rate limit
//
// List calls GET {base}/repos/{owner}/{repo}/releases?per_page=100. The ETag of
// the first page is kept in FetchState.ETag and sent as If-None-Match. A 304
// to an authenticated request does not count against GitHub's rate limit
// (authenticated 304s are free; anonymous ones are charged), which is what
// lets one token watch thousands of repositories. Later pages are fetched
// without validators.
//
// # Pagination and known versions
//
// FetchState.Cursor is JSON {"max_id":N,"full_at":unix}: the highest release
// id seen and the time of the last full scan (a bare decimal number from an
// earlier format is read as max_id). Release ids only grow, so a page whose
// releases all have an id at or below max_id holds nothing new and pagination
// stops after it. The first page is always returned in full.
//
// GitHub sorts releases by created_at, which is the date of the tagged commit,
// so a release created later for an old tag can sit below a page that looks
// known. List therefore ignores the cursor and the ETag and walks every page
// (at most maxPages, 10 requests) when the last full scan is 24 hours old or
// missing.
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
package githubrelease
