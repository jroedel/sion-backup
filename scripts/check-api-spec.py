#!/usr/bin/env python3
"""Check docs/openapi.yaml is valid and that every example in it is legal.

Two things go wrong with a hand-written OpenAPI document, and both go wrong
quietly:

  * the document itself stops being valid OpenAPI, which nothing notices until
    somebody feeds it to a generator;
  * an example drifts from the schema it illustrates, so the one part of the
    spec an implementer actually copy-pastes is the part that is wrong.

The second is the reason this script exists. Examples are documentation that
can be executed, and the whole value of writing them is lost if they are not.

Run with `make api-check`. It needs the dependencies in
scripts/requirements-api.txt, which CI installs.
"""

import sys
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator
from openapi_spec_validator import validate as validate_openapi
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

SPEC = Path(__file__).resolve().parent.parent / "docs" / "openapi.yaml"

# OpenAPI 3.1 schemas are JSON Schema 2020-12, so they can be validated
# directly — no translation layer, which is the point of 3.1.
BASE = "urn:spec"


def walk(node, pointer, found):
    """Collect every subschema carrying an `examples` list, by JSON pointer.

    Walking rather than checking only the top level, because a property's
    examples are as copy-pasted as a whole body's — a `code` example that does
    not match its own pattern is exactly the kind of thing that wastes an
    afternoon.
    """
    if isinstance(node, dict):
        if isinstance(node.get("examples"), list) and any(
            k in node for k in ("type", "$ref", "allOf", "oneOf", "anyOf")
        ):
            found.append(pointer)

        for key, value in node.items():
            walk(value, f"{pointer}/{escape(key)}", found)

    elif isinstance(node, list):
        for i, value in enumerate(node):
            walk(value, f"{pointer}/{i}", found)


def escape(token: str) -> str:
    return token.replace("~", "~0").replace("/", "~1")


def main() -> int:
    spec = yaml.safe_load(SPEC.read_text())

    try:
        validate_openapi(spec)
    except Exception as err:  # the validator raises several types
        print(f"FAIL  {SPEC.name} is not valid OpenAPI:\n{err}")
        return 1

    print(f"ok    valid OpenAPI {spec['openapi']}, "
          f"{len(spec['paths'])} paths, "
          f"{len(spec['components']['schemas'])} schemas")

    registry = Registry().with_resource(
        BASE, Resource(contents=spec, specification=DRAFT202012)
    )

    pointers: list[str] = []
    walk(spec.get("components", {}).get("schemas", {}), "/components/schemas", pointers)

    failures = 0
    checked = 0

    for pointer in pointers:
        validator = Draft202012Validator(
            {"$ref": f"{BASE}#{pointer}"},
            registry=registry,
            format_checker=Draft202012Validator.FORMAT_CHECKER,
        )

        node = resolve(spec, pointer)

        for i, example in enumerate(node["examples"]):
            checked += 1

            errors = sorted(validator.iter_errors(example), key=lambda e: list(e.path))
            if not errors:
                continue

            failures += 1
            print(f"FAIL  example {i} at {pointer}")

            for err in errors[:5]:
                where = "/".join(str(p) for p in err.path) or "(root)"
                print(f"        {where}: {err.message}")

    if failures:
        print(f"\n{failures} of {checked} examples do not match their own schema")
        return 1

    print(f"ok    {checked} examples match their schemas")

    return 0


def resolve(doc, pointer: str):
    node = doc

    for token in pointer.lstrip("/").split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        node = node[int(token)] if isinstance(node, list) else node[token]

    return node


if __name__ == "__main__":
    sys.exit(main())
