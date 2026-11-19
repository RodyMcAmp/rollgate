# 0016. How web forms change router.toml: small text edits, checked by parsing again

- Status: Proposed
- Date: 2026-11-19
- Builds on: 0014

## Context

ADR 0014 left one question open: the web UI's forms (port forwarding, devices with a fixed address, adding a network) need to change one part of `router.toml` without losing the rest.
"The rest" means the user's comments, the order of their sections, their blank lines and spacing, and anything they wrote by hand over SSH or in the Advanced editor.
Users switch between the forms and the file, so both must keep working on the same file.

What exists today:
- The compiler reads `router.toml` with `github.com/BurntSushi/toml` (ADR 0005). It turns text into Go values, but it can't write a file back with the comments in, and it doesn't say where in the text each key was.
- Writing the whole file again from the Go values would throw away every comment and reorder everything. ADR 0014 promised comments stay.
- Every change already goes through the same safe path: validate, show the changes, apply, then keep or undo (ADR 0010, 0014). The web UI already shows the user a diff of `router.toml` before Apply, and refuses an apply if the file changed on the router since the page loaded it.

The forms planned so far all edit a few kinds of things:

| Form | What it changes in `router.toml` |
|---|---|
| Port forwarding | add, change or remove a `[[firewall.port_forward]]` entry |
| Devices: fixed address | add, change or remove a `[[dhcp.<name>.static]]` entry |
| Networks: add a network | add a `[[vlan]]` entry and its `[dhcp.<name>]` section (and later a firewall rule) |
| Settings such as the router's name | set one key in a section, such as `[system] hostname` |

## Decision

### A small writer of our own, in a new package `compiler/edit`

- It changes the file's **text**, as a person would: only the lines that need to change are touched; every other byte stays as it was.
- It only needs to know where things are: a line-by-line scan that recognizes blank lines, comment lines, section headers (`[x]`, `[[x]]`) and `key = value` lines. It doesn't decode values; the real parser does that (below).
- Standard library only. No new module.

### The file as blocks

- The scan splits the file into the top-level keys at its start, then one **block** per section header: the header and the lines under it, up to the next header.
- Comment lines directly above a header (with no blank line between) belong to that header's block. So removing a port forward removes the comment written above it, and adding one doesn't separate a comment from the section it describes.
- A comment at the end of a line stays with that line.

### A fixed list of edits

1. **Add an entry** to a list of sections (`[[firewall.port_forward]]`, `[[dhcp.lan.static]]`, `[[vlan]]`): a new block after the list's last entry. If the list has no entries yet, the block goes after the section it belongs with (for example `[[dhcp.lan.static]]` after `[dhcp.lan]`), or at the end of the file.
2. **Change an entry:** the form names the entry by its list and position, and the value that identifies it (a port forward's `name`, a fixed address's `hostname`). If that value doesn't match, the edit is refused. Each changed key is rewritten on its own line, keeping that line's comment; a key the form removes loses its line; a new key goes at the end of the block.
3. **Remove an entry:** its block goes, with the comment above it.
4. **Set a key in a section** (`[system] hostname`): the value on its line changes, keeping the line's comment. If the key isn't there, a line is added at the end of the section; if the section isn't there, it's added at the end of the file.

Several edits can make one change (adding a network adds a `[[vlan]]` and a `[dhcp.<name>]`). The user sees them together in one diff and keeps or undoes them together.

New lines use one format, the one `examples/router.toml` uses: `key = value`, one per line, strings in double quotes, and a blank line before each new header.
Values are only the kinds `router.toml` has: strings, whole numbers, true or false, and lists of strings.

### What it refuses to edit

- The writer touches only blocks it fully understands. It refuses an edit that would touch a block with a value spread over several lines (a long list written across lines, a multi-line string), an inline table (`{ ... }`), or a dotted key that defines a section (`firewall.port_forward = [...]`).
- The form then says: "This part of router.toml is written in a way this page can't change safely. Use Advanced to edit it." Nothing is changed.
- Blocks the edit doesn't touch can contain anything TOML allows.

### Every edit is checked by parsing again

Before the web UI shows the result, the writer checks it, and refuses its own result if either check fails (that would be a bug in the writer):
1. **The meaning:** the old and the new text are both read with the real parser (`config.Parse`). The new config must equal the old one with exactly the intended change made, compared as Go values.
2. **The rest of the file:** every line outside the blocks the edit touched must be byte for byte the same.

Then the change takes the normal path: validation, the diff of `router.toml` on the Check page, Apply, and keep or undo.

### Where it runs

- In `rollgate web`, the unprivileged web server. It reads `router.toml` through the API (`GET /v1/config`), edits the text, and sends the result to `diff` and `apply` like the Advanced editor does.
- **The API doesn't change:** no new requests (ADR 0014's fixed list stays as it is). A bug in the writer can only produce text, and that text still goes through validation, the user's review and commit-confirm.
- Other tools can use the same package later, such as a `rollgate set` command or the AI assistant idea in the roadmap.

## Alternatives considered

- **Write the whole file again from the Go values** (the TOML library's encoder): simple, but it drops every comment, reorders sections and reformats everything. That breaks ADR 0014's promise.
- **A second TOML library that keeps comments** (for example go-toml v2's `unstable` parser, which reports positions and comments): it would do the scanning for us, but its API is marked unstable, it adds a module, and the router would then have two TOML parsers that could disagree on an unusual file. With our own small scan, the one real parser stays the judge of what a file means, and every edit is checked against it.
- **Forms write their own file** (say `router.d/forms.toml`) that is merged with `router.toml`: no editing needed, but the settings would live in two places, a user reading `router.toml` wouldn't see what the forms set, and someone has to decide which file wins when both set something. That goes against "keep the user config dead simple" (rule 9).
- **Keep settings in a database and generate `router.toml` from it:** `router.toml` would stop being the user's own file, and hand edits would be overwritten.
- **Run the writer in `rollgate api`, as root:** more code running as root, for no gain, since its output is checked and validated anyway.

## Consequences

- A new package `compiler/edit`, with tests:
  - golden tests: a `router.toml`, an edit, and the exact file it must become (`tests/edit/`, like `tests/golden/`);
  - a test that runs many random edits over the example and test configs and checks both rules above every time (Go's fuzzing, standard library);
  - refusal tests for each construct the writer doesn't edit.
- The forms come one page at a time after this ADR: port forwarding first, then fixed addresses on the Devices page, then adding a network. Each is its own PR, with the Console look the maintainer picked.
- Some hand-written files can't be changed by some forms. The page says so and points to Advanced; nothing breaks.
- Lines a form adds always look the same, whatever style the rest of the file uses.
- This isn't nftables, apply or boot code, so on its own it isn't on the safety-critical list. Its output reaches the router only through validate, apply and confirm, which are.
