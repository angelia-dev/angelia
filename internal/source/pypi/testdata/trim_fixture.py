#!/usr/bin/env python3
"""Trim a PyPI JSON API response (https://pypi.org/pypi/<name>/json) to the
fields the pypi source reads. The input is public registry data.

Usage: trim_fixture.py in.json out.json
"""
import json
import sys

d = json.load(open(sys.argv[1]))
out = {
    "info": {"name": d["info"]["name"], "version": d["info"]["version"]},
    "releases": {
        v: [
            {k: f[k] for k in ("upload_time_iso_8601", "yanked", "yanked_reason") if f.get(k) is not None}
            for f in files[:1]
        ]
        for v, files in d["releases"].items()
    },
}
json.dump(out, open(sys.argv[2], "w"), separators=(",", ":"), sort_keys=True)
