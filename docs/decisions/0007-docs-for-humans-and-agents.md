# 0007. Documentation methodology for humans and AI agents

- Status: Accepted
- Date: 2026-11-17

## Context

The project will be developed by a few people working alongside several different AI agents.
Agents start every session with no memory, pick up whatever instruction file their tool looks for, and tend to re-propose alternatives that were already rejected.
Humans need the same context, just less often.

## Decision

- **`AGENTS.md` at the repo root is the canonical instruction file.** It is the cross-tool convention that most agents read.
  `CLAUDE.md` contains only `@AGENTS.md` (Claude Code's import syntax), so there is one source of truth.
  Other tools' files (e.g. `GEMINI.md`, `.cursor/rules`) may be added as one-line pointers, never as copies.
- Subdirectories may have their own short `AGENTS.md` with local rules (e.g. `compiler/AGENTS.md`, covering template conventions).
- `AGENTS.md` holds rules and pointers, not explanations. Keep it under ~150 lines, because agents load it every session.
- Decisions go in ADRs (`docs/decisions/`). The format is in `docs/decisions/README.md`.
- Behavior is specified in docs before it is coded (`docs/config-spec.md` first, then code).
- Unimplemented features are marked *(planned)* everywhere. Docs never claim something works before tests prove it.
- One command interface (`make <target>`). Agents and humans run the same commands that CI runs.
- PRs state which agent or model wrote them, and which commands were run with what results.

## Consequences

- Any agent can be pointed at the repo and be productive after reading one file.
- Maintainers must keep `AGENTS.md`, the ADR index, and the *(planned)* markers current. Reviewers should flag drift.
