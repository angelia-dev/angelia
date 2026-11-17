---
name: Angelia admin UI
description: The Release Wire, the documentation site's news-wire room, as an operator's instrument panel.
colors:
  midnight: "#14123a"
  ink: "#1e1b4b"
  night: "#0d0b2a"
  wire-paper: "#fffcef"
  code-paper: "#f7efcd"
  canary-pale: "#fef6d3"
  canary: "#fde68a"
  herald-gold: "#fbbf24"
  tape-gold: "#f59e0b"
  burnt-gold: "#a14a07"
  wire-indigo: "#4338ca"
  bulletin-indigo: "#3730a3"
  flash: "#c2361d"
  flash-night: "#ff7a5c"
  ok: "#23774a"
  ok-night: "#6fd49a"
  moonlight: "#e6e7fb"
  moonlight-dim: "#b4b8ee"
  docs-ink: "#1b1938"
  wire-ink: "#2a2614"
typography:
  page-title: { fontFamily: Archivo, fontSize: 2rem, fontWeight: 850, fontStretch: 70%, lineHeight: 1 }
  subtitle: { fontFamily: Archivo, fontWeight: 800, fontStretch: 78%, lineHeight: 1.1 }
  version: { fontFamily: Archivo, fontSize: 2.4rem to 3rem, fontWeight: 850, fontStretch: 62% to 64%, lineHeight: 0.95 }
  body: { fontFamily: Archivo, fontSize: 15px, fontWeight: 400, lineHeight: 1.5 }
  label: { fontFamily: JetBrains Mono, fontSize: 0.72rem, fontWeight: 700, letterSpacing: 0.08em, textTransform: uppercase }
  data: { fontFamily: JetBrains Mono, fontSize: 0.75rem to 0.8rem, fontWeight: 400 }
rounded:
  none: "0"
  slip: "2px"
  dot: "50%"
---

# Design system: Angelia admin UI

The admin UI is the same world as the documentation site (angelia-dev/docs, `DESIGN.md`, "The Release
Wire"), set for an operator who glances, checks and fixes. Tokens live in `src/index.css`; components read
them through Tailwind (`bg-chrome`, `text-gold-ink`, `paper`, …). Light and dark follow
`prefers-color-scheme`.

## Rules

- **Chrome is midnight in both themes.** The header, the nav tabs and the plates of the rack cubbies are
  midnight or ink, with canary text. The gold Baudot tape (`.baudot`, `<Tape />`) runs under the header.
- **Paper means copy.** Anything that is wire output, a release or a delivery slip, sits on canary paper
  (`.paper`) with a long soft drop. Paper keeps its day colours at night; the class re-points the tokens
  inside it, so stamps and links stay legible.
- **Gold is the path.** Gold fills the one primary action per block, the tape, the next-check meters and
  the selected state of segmented controls. Text in gold on paper is burnt gold (`text-gold-ink`).
- **Stamps carry status.** `<Stamp tone>`: `flash` (failure, dead letter, major, yank), `bulletin` (minor,
  in flight), `routine` (patch, pending), `ok` (delivered), `held`. The word is the signal; colour repeats it.
  Flash red is never decoration.
- **Square.** Panels, tables and cubbies have no radius; buttons, badges and stamps take 2px. Circles are
  only holes and signal dots.
- **Mono speaks for the wire.** JetBrains Mono for data, identifiers, slugs, column heads and stamps;
  prose stays in Archivo. Headings squeeze Archivo's width axis (62 to 82 percent) as they get heavier.
- **Only paper and screens cast shadows.** Code panels are deep night with a 3px gold top rule.
- **Contrast.** Body text is at least 4.5:1 in both themes; keyboard focus is a 2px ring (indigo by day,
  gold by night).

## Surfaces

- **Overview, the sorting rack.** A grid of pigeonholes under a heavy rule, one per watch, each with a
  midnight plate (source, scheme, managed by), the name, the target, the latest version set huge and
  condensed, a gold meter to the next check, and the paper slip of the latest release. Compact form past 24
  watches: dense rows grouped by source.
- **Events, the punched tape.** A gold tape for the range with one hole per release, a bulletin slip with
  tractor-feed margins for the selected one, the CloudEvent JSON on a code panel, and the ledger below.

## Fonts

Archivo (variable, width 62 to 125, weight 100 to 900) and JetBrains Mono (variable, 100 to 800), latin
and latin-ext subsets, vendored in `src/fonts/` under the SIL Open Font License 1.1.
