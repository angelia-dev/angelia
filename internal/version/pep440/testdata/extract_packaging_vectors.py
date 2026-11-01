"""Regenerate packaging_vectors.json from the packaging test suite.

Usage (from this directory; see NOTICE):

    curl -sSL https://files.pythonhosted.org/packages/source/p/packaging/packaging-26.3.tar.gz | tar xz -C /tmp
    python3 -I extract_packaging_vectors.py /tmp/packaging-26.3/tests/test_version.py 26.3 > packaging_vectors.json

The script only reads literals with ast.literal_eval; it never imports or
runs the test module.
"""

import ast
import json
import sys

WANT = {
    "test_invalid_versions": "invalid",
    "test_normalized_versions": "normalized",
    "test_version_str_repr": "str",
    "test_version_is_prerelease": "is_prerelease",
}


def main(path: str, release: str) -> None:
    tree = ast.parse(open(path, encoding="utf-8").read())
    found = {}
    for node in tree.body:
        if isinstance(node, ast.Assign) and getattr(node.targets[0], "id", None) == "VERSIONS":
            found["ordered"] = ast.literal_eval(node.value)
        if isinstance(node, ast.ClassDef) and node.name == "TestVersion":
            for fn in node.body:
                if not (isinstance(fn, ast.FunctionDef) and fn.name in WANT):
                    continue
                for dec in fn.decorator_list:
                    if isinstance(dec, ast.Call) and getattr(dec.func, "attr", "") == "parametrize":
                        found[WANT[fn.name]] = [
                            list(x) if isinstance(x, tuple) else x for x in ast.literal_eval(dec.args[1])
                        ]
    missing = (set(WANT.values()) | {"ordered"}) - set(found)
    if missing:
        sys.exit(f"missing vectors: {sorted(missing)}")
    out = {
        "source": f"pypa/packaging {release}, tests/test_version.py (sdist packaging-{release}.tar.gz from PyPI)",
        "license": "Apache-2.0 OR BSD-2-Clause; see NOTICE in this directory",
    }
    for key in ("ordered", "invalid", "normalized", "str", "is_prerelease"):
        out[key] = found[key]
    print(json.dumps(out, indent=1, ensure_ascii=True))


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
