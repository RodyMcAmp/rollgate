# 0005. One declarative config compiled by a Go tool

- Status: Accepted
- Date: 2026-11-17

## Context

Configuring a router by hand-editing a dozen daemon configs is error-prone, for people and for AI agents alike.
Koozali SME Server has shown for decades that a central settings store plus templates that generate every real config file works well.
The tool that does this runs on a rolling distro, so it must not break when Arch updates a language runtime.

## Decision

- Users describe the router in a single `router.toml` (spec: `docs/config-spec.md`).
- A CLI named `rollgate`, written in **Go**, validates, compiles, diffs and applies it.
  Compiling is a pure function: config in, files out, no side effects.
- Templates use Go's `text/template` and live in `compiler/templates/`.
- The binary is statically linked and shipped as our own pacman package.
- The future web UI (Go `net/http` + `html/template` + htmx) is a front end that edits `router.toml` and calls the same apply path. It never writes system files.

## Alternatives considered

- **Python**: the easiest language for contributors and agents. But on Arch, every Python minor-version bump breaks installed modules until they're rebuilt, which is a real risk for the one tool that has to work during recovery. It also adds runtime dependencies on the router.
- **Rust**: the strongest correctness guarantees, but slower to iterate on and harder for casual contributors.
- **Shell scripts**: fine for glue, but untestable at this size.
- **Ansible or similar**: built for configuring fleets from outside, not as an on-box appliance compiler.

## Consequences

- A static binary keeps working whatever else Arch updates.
- Go's simple type system and explicit errors catch many agent mistakes at compile time.
- Golden-file tests (`docs/testing.md`) can cover the compiler completely.
