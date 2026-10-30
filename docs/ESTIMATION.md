# Estimation and model selection

How the stories on the [Angelia Roadmap](https://github.com/orgs/angelia-dev/projects) are sized, who executes them, and which model an agent should use. The numbers are a starting point: every closed story records **actuals** (agent attempts, wall-clock, human takeover), and we recalibrate this table once Phase 0 is done.

## Project fields

| Field | Values | Meaning |
|---|---|---|
| Executor | Agent · Agent + human check · Human-led · Human-only | Who does the work (see below) |
| Size | XS · S · M · L | Scope of the change |
| Complexity | Low · Medium · High · Very high | How hard the reasoning is (ambiguity, concurrency, correctness, security) |
| Model | Haiku · Sonnet · Opus · Human | Suggested model, derived from Complexity; the maintainer can override |
| Risk class | Normal · Docs/CI · Core contract · Security · Integration · External API · Performance · Human-only | Why a story is risky; drives review |
| Priority | P0 · P1 · P2 | P0 = blocks the phase exit criteria |
| Human days | number | Likely effort for a human developer, with review and tests |
| Agent hours | number | Expected agent wall-clock, all attempts included |
| Supervision hours | number | Expected human time: spec answers, reviews, takeovers |
| First-pass % | number | Expected chance the first agent PR is mergeable without a rewrite |

## Executor

- **Agent**: an agent implements it; Graft decides whether a human must review.
- **Agent + human check**: an agent implements it; the maintainer checks the result (docs that state decisions, benchmarks, external integrations).
- **Human-led**: security-critical or contract-defining. The agent drafts a design note or ADR first, the human decides, then the agent implements.
- **Human-only**: accounts, secrets, apps, go/no-go decisions. Agents must not pick it up.

## Size → effort (likely)

| Size | Human days | Agent hours |
|---|---|---|
| XS | 0.5 | 0.5 |
| S | 1 | 1.5 |
| M | 2.5 | 3.5 |
| L | 5 | 7 |

Supervision hours = Human days × factor: Agent 1.0 · Agent + human check 2.0 · Human-led 4.0 · Human-only 8.0 (all of it is human time).

First-pass %: Agent 60 (XS 80, L 40) · Agent + human check 45 · Human-led 25 · Human-only 0.

## Complexity → model

| Complexity | Typical work | Suggested model |
|---|---|---|
| Low | Boilerplate, config, docs, CI wiring, fixtures | Haiku |
| Medium | A well-specified component with clear tests (a source, a sink, a CLI command) | Sonnet |
| High | Concurrency, persistence, delivery guarantees, version ordering, public contracts | Opus |
| Very high | Security-critical or architecture-defining spikes; always Human-led | Opus, with a human designing |

Escalation rule: if a story fails its CI checks three times with the suggested model, or needs more than 2× its Agent hours, retry once with the next model up, then hand it to a human (label `human-needed`).
