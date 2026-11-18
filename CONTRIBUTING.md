# Contributing

This project is built by people and AI agents together. The process below applies to both.

## Workflow

1. **Start from an issue.** Each issue states a goal, acceptance criteria, and the milestone (`docs/roadmap.md`).
   Issues should be small enough for one PR. Split anything larger.
2. **Read before writing.** Read [AGENTS.md](AGENTS.md), then the docs and ADRs for the area you're touching.
3. **Decide before building.** New daemon, language, package source, or a change to an accepted decision → write an ADR first and get it accepted.
4. **Spec before code.** Config format changes go into `docs/config-spec.md` and `examples/router.toml` first.
5. **Test first.** Write the failing test, then make it pass. Required test levels by area are in [docs/testing.md](docs/testing.md).
6. **Small PRs.** Aim for under ~400 changed lines, not counting golden files.
7. **Open the PR** using the template. CI must be green.

## Definition of done

- [ ] Acceptance criteria in the issue are met
- [ ] Tests at the required levels were added or updated, and pass in CI
- [ ] Docs were updated: spec, architecture, runbook if recovery changes, and *(planned)* markers removed for anything now built
- [ ] No hard rule in AGENTS.md is broken
- [ ] A human reviewed the change if it touches a safety-critical path (listed in AGENTS.md)

## Working with AI agents

- Point the agent at the issue and tell it to read `AGENTS.md` first. Most tools do this automatically.
- One agent per issue and branch at a time, so two agents never edit the same files.
- The person who opens an agent-written PR owns it: they read the diff and answer review questions.
- Agents must report the commands they ran and the results. "Tests pass" without output doesn't count.
- If an agent wants to change an accepted decision, it writes a superseding ADR for humans to decide on. It does not quietly work around the decision.

## Reviews

- Every PR gets at least one review. A different AI agent may give a first-pass review, but a human approves merges to safety-critical paths.
- Reviewers check for doc drift: does `AGENTS.md`, the ADR index, or the spec need updating too?

## Commit messages

Use an imperative summary under ~72 characters, a blank line, then the why.
If an agent co-wrote the commit, add a `Co-Authored-By:` trailer for it.
