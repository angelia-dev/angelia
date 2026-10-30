# Architecture decision records

Each ADR records one architectural decision: its context, the decision, its consequences and the
alternatives considered. All ADRs use the same [template](0000-template.md).

| ADR | Title | Status | Story |
|---|---|---|---|

## Process

- Copy `0000-template.md` to `NNNN-short-title.md` with the next free number, and add a row here.
- A new ADR starts as **Proposed**. The maintainer sets it to **Accepted** when merging the review.
- An accepted ADR is not rewritten. To change a decision, write a new ADR that supersedes it and
  set the old one to **Superseded by ADR NNNN**, linking the new ADR.
- **Deprecated** means the decision no longer applies and nothing replaces it; the ADR is kept for history.
- Phase 0 spikes end with an ADR that records the measurements and the go/no-go.
