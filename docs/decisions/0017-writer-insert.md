# 0017. The router.toml writer can insert an entry before another

- Status: Proposed
- Date: 2026-11-19
- Amends: 0016

## Context

ADR 0016 gave the writer for web forms (`compiler/edit`) a fixed list of edits. Adding an entry to a list of sections always puts it after the list's last entry.
That's right for port forwards and fixed addresses, whose order doesn't matter. It's wrong for `[[firewall.policy]]`: the rules are checked in order and the first match wins (config-spec.md).
A config like `examples/router.toml` ends with a catch-all rule (`from = "*"`, `to = "*"`, `action = "drop"`). A rule added after it never matches.

The Networks page needs this: a new network (a VLAN with its DHCP section) gets no internet unless a rule lets it out, and that rule must come before the catch-all.

## Decision

- The writer gets one more edit: **insert an entry before entry N** of a list of sections. The new block goes directly above entry N, and above the comment lines that belong to it (ADR 0016's blocks), so an existing comment stays with its rule.
  If N is the number of entries, it's the same as adding at the end.
- Everything else in ADR 0016 holds for it: only the lines that must change are touched, the result is checked by parsing again (the new config must equal the old one with the entry inserted at N, and every other line must be unchanged), and it refuses what it doesn't edit safely.
- Like a change or a removal, an insert counts as the one positional edit a list may get in one change.
- There's no identifying value to check, as there is for a change: the page that asks for the insert already refuses to apply if `router.toml` changed since it was read (ADR 0014's apply).
- **Where the Networks page inserts a new network's rule:** before the first rule that would otherwise match the new network's traffic, which is the first rule with `from = "*"`. Rules naming other zones can't match it, so the order among them doesn't matter. If there's no such rule, the new rule goes at the end.
- If `router.toml` has no `[[firewall.policy]]` at all, the default applies: every non-WAN network may reach the internet and nothing else (config-spec.md). A new network then needs no rule. The page adds none, because adding the first rule would turn that default off for every network.

## Alternatives considered

- **Remove the catch-all, add the new rule, add the catch-all back:** three edits where one will do, and the catch-all's comment would be lost.
- **Leave rules to the Advanced editor:** a network added from the page would then have no internet in configs like the example's, which is the case the page exists for.
- **A general "move entry" edit:** more than the Networks page needs. It can come later with its own ADR if a page needs to reorder rules.

## Consequences

- `compiler/edit` gains `Insert`, with golden cases and refusal tests, and the random test and fuzz target include it.
- The Networks page can add a network that works in configs with and without firewall rules.
