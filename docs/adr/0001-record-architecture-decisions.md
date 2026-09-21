# 1. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-09-21

## Context

Sindri will be built incrementally across many separate working sessions, most of them
with an AI assistant that starts each session without memory of the previous one. Any
decision that is not written down will be re-litigated, and often re-decided differently.

## Decision

We keep architecture decision records in `docs/adr/`, numbered sequentially, in the
lightweight format Michael Nygard described. One file per decision. Each states the
context, the decision, the consequences, and explicitly what was rejected.

Every decision a future session might plausibly reopen gets a record. The rejected
alternatives matter as much as the choice.

## Consequences

- `CLAUDE.md` points here, so a session reads the decision instead of re-making it.
- Superseding a decision means a new record that references the old one. Records are never
  edited to say something different after the fact.
- There is a small tax on making decisions, which is the point.
