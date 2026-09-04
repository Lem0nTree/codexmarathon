#!/usr/bin/env python3
"""Check that protocol schemas and both language constants expose the same API."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


def fail(message: str) -> None:
    raise RuntimeError(message)


def load_schema(path: Path) -> dict:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"cannot read protocol schema {path}: {error}")
    if not isinstance(document, dict):
        fail(f"protocol schema root is not an object: {path}")
    return document


def has_const(node: object, property_name: str, values: set[str]) -> None:
    if isinstance(node, dict):
        properties = node.get("properties")
        if isinstance(properties, dict):
            field = properties.get(property_name)
            if isinstance(field, dict) and isinstance(field.get("const"), str):
                values.add(field["const"])
        for child in node.values():
            has_const(child, property_name, values)
    elif isinstance(node, list):
        for child in node:
            has_const(child, property_name, values)


def schema_constants(path: Path, property_name: str) -> set[str]:
    document = load_schema(path)
    definitions = document.get("$defs", {})
    if not isinstance(definitions, dict):
        fail(f"protocol schema has no $defs object: {path}")
    values: set[str] = set()
    has_const(definitions, property_name, values)
    if not values:
        fail(f"protocol schema contains no {property_name!r} constants: {path}")
    return values


def schema_definition_names(path: Path, property_name: str) -> set[str]:
    document = load_schema(path)
    definitions = document.get("$defs", {})
    if not isinstance(definitions, dict):
        fail(f"protocol schema has no $defs object: {path}")
    names = set()
    for name, definition in definitions.items():
        values: set[str] = set()
        has_const(definition, property_name, values)
        if values:
            names.add(name)
    return names


def top_level_refs(path: Path, expected_names: set[str]) -> set[str]:
    document = load_schema(path)
    refs = document.get("oneOf", [])
    if not isinstance(refs, list):
        fail(f"protocol schema oneOf is not an array: {path}")
    names = set()
    for ref in refs:
        if isinstance(ref, dict) and isinstance(ref.get("$ref"), str):
            names.add(ref["$ref"].rsplit("/", 1)[-1])
    # Every method/event definition must be reachable from the schema entry
    # point. This catches a definition that exists but can never validate.
    unreachable = {
        name
        for name in expected_names
        if name not in names
    }
    if unreachable:
        fail(f"protocol definitions are unreachable from {path.name}: {sorted(unreachable)}")
    return names


def constants_from_source(path: Path, pattern: str) -> set[str]:
    text = path.read_text(encoding="utf-8")
    return set(re.findall(pattern, text))


def validate(root: Path) -> None:
    commands = root / "protocol" / "commands.json"
    events = root / "protocol" / "events.json"
    command_values = schema_constants(commands, "method")
    event_values = schema_constants(events, "event_type")

    top_level_refs(commands, schema_definition_names(commands, "method"))
    top_level_refs(events, schema_definition_names(events, "event_type"))

    go_source = root / "controller" / "internal" / "runtime" / "wire.go"
    rust_source = root / "runtime" / "codexmarathon-adapter" / "src" / "protocol.rs"
    go_methods = constants_from_source(go_source, r'Method\w+\s+Method\s*=\s*"([^"]+)"')
    go_events = constants_from_source(go_source, r'Event\w+\s+EventType\s*=\s*"([^"]+)"')
    rust_methods = constants_from_source(
        rust_source, r'pub const METHOD_[A-Z0-9_]+:\s*&str\s*=\s*"([^"]+)"'
    )
    rust_events = constants_from_source(
        rust_source, r'pub const EVENT_[A-Z0-9_]+:\s*&str\s*=\s*"([^"]+)"'
    )
    # The adapter also exposes the JSON-RPC notification method under the
    # EVENT_ prefix, but it is transport metadata rather than an event type.
    rust_events.discard("codexmarathon/event")
    if command_values != go_methods or command_values != rust_methods:
        fail(
            "command constants differ: "
            f"schema-only={sorted(command_values - go_methods - rust_methods)}, "
            f"go-only={sorted(go_methods - command_values)}, "
            f"rust-only={sorted(rust_methods - command_values)}"
        )
    if event_values != go_events or event_values != rust_events:
        fail(
            "event constants differ: "
            f"schema-only={sorted(event_values - go_events - rust_events)}, "
            f"go-only={sorted(go_events - event_values)}, "
            f"rust-only={sorted(rust_events - event_values)}"
        )


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    args = parser.parse_args(argv)
    try:
        validate(args.root.resolve())
    except (OSError, RuntimeError, json.JSONDecodeError) as error:
        print(f"FAIL: protocol consistency: {error}")
        return 1
    print("PASS: protocol consistency: schema, Go, and Rust method/event catalogs agree")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
