// Package githubrelease is the "github-release" Source: it lists the
// releases of a GitHub (or GitHub Enterprise) repository.
//
// Target coordinates: {"repo": "owner/name"}. Names are case-insensitive on
// GitHub; callers that coalesce targets should lower-case the coordinate.
//
// # Requests and rate limit
//
// List calls GET {base}/repos/{owner}/{repo}/releases?per_page=100. The ETag of
// the first page is kept in FetchState.ETag and sent as If-None-Match; a 304
// does not count against GitHub's rate limit, which is what lets one token
// watch thousands of repositories. Later pages are fetched without validators.
//
// # Pagination and known versions
//
// FetchState.Cursor holds the highest release id seen so far (a decimal
// string). Release ids only grow, so a page whose releases all have an id at
// or below the cursor holds nothing new and pagination stops after it. The
// first page is always returned in full, which also reports a draft that was
// published later with a lower id. With no cursor every page is fetched, up to
// a cap of maxPages pages.
//
// # Tokens
//
// The token is read at request time from WithToken or WithTokenEnv and is sent
// only as an Authorization header, and only over https (or to a loopback
// host). It is held in a redacting type, never appears in an error or a log,
// and is not sent to other hosts on redirects (net/http drops the header).
package githubrelease
