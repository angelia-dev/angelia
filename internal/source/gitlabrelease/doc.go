// Package gitlabrelease is the "gitlab-release" Source: it lists the releases
// of a project on gitlab.com or on a self-managed GitLab instance.
//
// Target coordinates: {"project": "group/subgroup/name"} (a full path with any
// number of subgroups) or {"project": "12345"} (the numeric project id).
// Include and exclude patterns, prerelease mode and the rest of the filtering
// belong to policy, not to this source.
//
// # Requests
//
// List calls GET {base}/api/v4/projects/{project}/releases?order_by=created_at&sort=desc&per_page=100,
// where {project} is the URL-encoded path ("group%2Fsub%2Fname"), following
// pages until X-Next-Page is empty (a short page when the header is absent).
// At most 10 pages (1000 releases) are read: a project with more releases is
// truncated at that cap, newest first by created_at, and the older ones are
// never reported. The base URL is the root of the instance,
// "https://gitlab.com" by default, "https://HOST" for a self-managed instance,
// or "https://HOST/prefix" when GitLab is served under a relative URL root; a
// trailing "/api/v4" (any case) is accepted and ignored. A base URL with a
// query or a fragment is rejected.
//
// Pages are ordered by created_at, not released_at, because released_at is
// settable by the releaser (a "historical" release may be backdated): a
// backdated release has a recent created_at and so is found on the first page.
//
// The ETag of the first page, when GitLab sends one, is kept in
// FetchState.ETag and sent as If-None-Match. When it changed, List walks the
// pages only until one that holds a released entry at or below the newest
// created_at already seen (the "max_created_at" of the cursor): a new release
// costs one page, not the whole history. Releases that change without a new
// created_at are not seen by such a scan, so List ignores the validators and
// walks every page when the last such scan is 24 hours old or missing, and
// when an upcoming release is due (see below).
//
// Each page is capped by the netguard client at 10 MiB unless the GitLab
// host has its own netguard.HostLimit.MaxBodyBytes; a project whose page
// of 100 releases is larger fails with an error (gitlab-org/gitlab-runner
// is about 1.3 MB per page).
//
// # Mapping
//
// Raw is the tag name. PublishedAt is released_at (created_at when GitLab
// returns none). released_at is user-settable (a historical release is
// backdated, an upcoming one is dated in the future), so it is not proof of
// when the release appeared: the policy engine treats gitlab-release dates as
// untrusted and counts a cooldown from detection. The real creation time is
// kept in Metadata "created_at" (RFC 3339, UTC). Prerelease is nil: GitLab has no prerelease flag, so policy
// decides. URL and NotesURL are the release page ("_links.self" when it is an
// http(s) URL, otherwise built from the base URL and the project path).
// Metadata carries "created_at", "name" and the tagged commit "sha" when present.
//
// # Upcoming releases
//
// A release whose released_at is in the future has upcoming_release set. It
// is not reported until it is released: List skips it and records the earliest
// such released_at in the cursor, and the first List call after that time
// ignores the validators (the ETag may not change when released_at passes) and
// reports the release. The rescan waits one minute after released_at, so that
// GitLab has flipped the flag.
//
// # Cursor
//
// FetchState.Cursor is JSON {"full_at":unix,"max_created_at":unix_ms,"upcoming_at":unix}: the
// time of the last validator-free scan, the newest created_at seen and the
// earliest pending upcoming release. It
// is a few dozen bytes. This source makes no per-release lookups: the list
// carries everything it reports.
//
// # Tokens
//
// The token is read once per List call from WithToken or WithTokenEnv; a
// configured but empty token is an error, not an anonymous request (which
// cannot see private projects). It is sent as the PRIVATE-TOKEN header
// (personal, project and group access tokens) or, with WithBearerAuth, as
// "Authorization: Bearer" (OAuth 2 tokens), and only over https (or to a
// loopback host). It is held in a redacting type and never appears in an error
// or a log. The injected client is wrapped so that a redirect carrying the
// token to another host (including another port or a subdomain) or to plain
// http fails instead of being followed; net/http alone would forward the
// PRIVATE-TOKEN header to any host.
package gitlabrelease
