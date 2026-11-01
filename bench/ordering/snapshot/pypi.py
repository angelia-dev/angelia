"""Build the PyPI snapshot for benchmark 6 (version ordering).

Usage (from the repository root; see bench/ordering/README.md):

    pip install packaging==26.3
    python3 -I bench/ordering/snapshot/pypi.py \
        bench/ordering/packages/pypi.txt bench/ordering/testdata/pypi.jsonl.gz

For each project in the list it fetches every version string from the PyPI
Simple API (PEP 691 JSON, PEP 700 "versions" key), splits them into the
versions `packaging` accepts and the ones it rejects, and sorts the accepted
ones with packaging.version.Version: that is the order pip and pypi.org use.
Versions of equal precedence ("1.0" and "1.0.0") form one group.

Output: gzip'd JSON lines. Line 1 is {"meta": {...}}; every other line is
{"name": ..., "order": [[v, ...], ...], "invalid": [...]}, sorted by name.
The gzip header carries no name and no timestamp, so two runs on the same
day over the same registry data give identical bytes.

Only the standard library and `packaging` are used. Network: pypi.org only.
"""

import concurrent.futures
import datetime
import gzip
import io
import json
import re
import sys
import time
import urllib.error
import urllib.request

import packaging
from packaging.version import InvalidVersion, Version

SIMPLE = "https://pypi.org/simple/{}/"
ACCEPT = "application/vnd.pypi.simple.v1+json"
USER_AGENT = "angelia-bench-ordering/1 (+https://github.com/angelia-dev/angelia)"
WORKERS = 8
RETRIES = 3


def normalize(name: str) -> str:
    """PEP 503 name normalization."""
    return re.sub(r"[-_.]+", "-", name).lower()


def read_list(path: str) -> list[str]:
    names = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                names.append(normalize(line))
    return sorted(set(names))


def fetch(name: str) -> list[str] | None:
    req = urllib.request.Request(SIMPLE.format(name), headers={"Accept": ACCEPT, "User-Agent": USER_AGENT})
    for attempt in range(RETRIES):
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                return list(json.load(resp)["versions"])
        except urllib.error.HTTPError as e:
            if e.code == 404:
                return None
            if attempt == RETRIES - 1:
                raise
        except (urllib.error.URLError, TimeoutError):
            if attempt == RETRIES - 1:
                raise
        time.sleep(2**attempt)
    return None


def order(versions: list[str]) -> tuple[list[list[str]], list[str]]:
    valid, invalid = [], []
    for raw in versions:
        try:
            valid.append((Version(raw), raw))
        except InvalidVersion:
            invalid.append(raw)
    valid.sort()  # by precedence, then by raw string within a group
    groups: list[list[str]] = []
    prev = None
    for v, raw in valid:
        if prev is not None and v == prev:
            groups[-1].append(raw)
        else:
            groups.append([raw])
        prev = v
    return groups, sorted(invalid)


def main(list_path: str, out_path: str) -> None:
    names = read_list(list_path)
    with concurrent.futures.ThreadPoolExecutor(WORKERS) as pool:
        results = dict(zip(names, pool.map(fetch, names)))
    missing = sorted(n for n, v in results.items() if v is None)
    records = []
    for name in names:
        versions = results[name]
        if versions is None:
            continue
        groups, invalid = order(versions)
        records.append({"name": name, "order": groups, "invalid": invalid})
    meta = {
        "registry": "pypi",
        "source": "https://pypi.org/simple/ (PEP 691 JSON, PEP 700 versions)",
        "oracle": f"packaging {packaging.__version__} (packaging.version.Version)",
        "generated": datetime.date.today().isoformat(),
        "packages": len(records),
        "versions": sum(sum(len(g) for g in r["order"]) + len(r["invalid"]) for r in records),
        "missing": missing,
    }
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0, compresslevel=9) as gz:
        for obj in [{"meta": meta}, *records]:
            gz.write(json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode() + b"\n")
    with open(out_path, "wb") as f:
        f.write(buf.getvalue())
    print(f"pypi: {meta['packages']} packages, {meta['versions']} versions, missing {missing}", file=sys.stderr)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: pypi.py PACKAGE_LIST OUT.jsonl.gz")
    main(sys.argv[1], sys.argv[2])
