"""L0 check: every example config parses as TOML and declares the current schema.

Replaced by `rollgate validate` against the real schema in M2.
"""

import pathlib
import sys
import tomllib

SCHEMA_VERSION = 1


def main() -> int:
    root = pathlib.Path(__file__).resolve().parent.parent
    examples = sorted((root / "examples").glob("*.toml"))
    if not examples:
        print("check_examples: no examples/*.toml found", file=sys.stderr)
        return 1

    failures = 0
    for path in examples:
        name = path.relative_to(root)
        try:
            config = tomllib.loads(path.read_text(encoding="utf-8"))
        except tomllib.TOMLDecodeError as err:
            print(f"FAIL {name}: {err}", file=sys.stderr)
            failures += 1
            continue
        if config.get("schema_version") != SCHEMA_VERSION:
            print(f"FAIL {name}: schema_version must be {SCHEMA_VERSION}", file=sys.stderr)
            failures += 1
            continue
        print(f"ok   {name}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
