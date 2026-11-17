# Product

<!-- impeccable:product-schema 1 -->

> Interview substitution: written on 2026-11-17 from the maintainer's written brief ("rework the admin UI
> design, several mocks to decide the direction, light/dark following the system, much cooler than the
> current dark version, following maybe the documentation site"), the angelia-docs PRODUCT.md and DESIGN.md,
> the angelia repository (docs/admin-ui.md, ADR 0033, web/) and the live instance. Lines marked *(inferred)*
> were not confirmed by the maintainer.

## Platform

web

## Stack

The admin UI lives in `web/`: React 19, TanStack Router and Query, Tailwind CSS 4, shadcn-style primitives,
lucide icons, built with Vite and embedded in the Go binary, served by the `api` role under `/ui/`.

## Users

- The operator of an Angelia instance (often the maintainer themselves, self-hosting on a small VPS), who
  checks that watches run, sees which releases came in, and fixes failed deliveries.
- Teammates given `viewer` or `editor` roles through `ui.access`, who read the event log or replay a delivery.
- Visits are short and frequent: a glance after a release, a check after a CI run did not start, a fix of a
  dead letter. *(inferred)*

## Product Purpose

Angelia is the self-hosted release event bus: it watches upstream artifacts (PyPI, npm, crates.io, GitHub and
GitLab releases and tags), detects every new version, normalizes it into one signed CloudEvent, and delivers it
to webhooks, GitHub dispatches and chat services, or lets consumers pull it (REST, SSE, MCP). The admin UI is the
web front of one instance: watches, events, deliveries, sinks, subscriptions, consumers, plugins, secrets,
tokens, sessions and the access list.

## Positioning

The UI is the instrument panel of the wire room the docs describe: it should feel like the same product as the
docs site, not like a generic admin template. *(inferred from "following the documentation")*

## Operating Context

Opened in a desktop browser at the instance's public URL (e.g. https://angelia.example.com), sometimes on a
phone. Sign-in with GitHub or OIDC. Light and dark follow the operating system (`prefers-color-scheme`).
Small instances have a handful of watches and a few events a day; larger ones have hundreds of watches.

## Capabilities and Constraints

- Every action goes through the same `/v1` REST API as the CLI, with the same role checks (viewer, editor, admin).
- Live updates come from the SSE event stream. File-managed rows (`managed_by: file`) are read-only in the UI.
- Secret values are never shown. A strict CSP applies: `default-src 'self'`, GitHub avatars allowed.
- Accessibility: keyboard focus always visible, AA contrast in both themes.

## Brand Commitments

- Name, mark and palette as in angelia-docs: the A as a rising peak with a herald's wing of three gold feathers;
  indigo #1E1B4B to #4338CA, gold #F59E0B / #FBBF24 / #FDE68A. The docs world is "The Release Wire" (news-wire
  room, canary paper, gold Baudot tape, FLASH / BULLETIN / ROUTINE stamps). Brand assets in the
  docs repository (angelia-dev/docs, `docs/assets/brand/`).
- Voice: plain, precise, engineering-honest.

## Evidence on Hand

- The live instance: watches gh-cli, home-assistant, pypi-requests; sink csnet-home-ci (github-dispatch);
  subscription ha-to-csnet-home; the event home-assistant/core 2026.11.2, delivered.
- No usage numbers or users beyond the maintainer: never invent them.

## Product Principles

1. State first: what is failing, what is pending, what just arrived.
2. The event is the product: show the release (package, version, bump) before the plumbing.
3. Truth over decoration: file-managed vs API-managed, shipped vs planned, always explicit.
4. Same world as the docs: a reader of the docs recognizes the UI at once.
