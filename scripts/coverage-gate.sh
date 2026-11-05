#!/usr/bin/env bash
# Coverage gate for internal/: fails when the statement coverage of all
# internal/ packages together is below the threshold.
#
# Usage: scripts/coverage-gate.sh [coverprofile] [threshold-percent]
#   coverprofile       default coverage.out, written by:
#                      go test -race -coverpkg=./internal/... -coverprofile=coverage.out ./...
#   threshold-percent  default 80
#
# Generated code is left out: its statements say nothing about the tests of
# the code written by hand. A file counts as generated only when it is named
# *.gen.go AND its first lines hold the Go convention header
# "// Code generated ... DO NOT EDIT." A *.gen.go file without the header
# fails the gate, so hand-written code cannot hide behind the name. This
# covers every generated file under internal/: the REST server
# (internal/api), the client (internal/apiclient) and the sqlc store code.
#
# Packages without statements (for example doc.go-only packages) have no
# blocks in the profile. They are listed as ignored and never fail the gate.
set -euo pipefail

profile="${1:-coverage.out}"
threshold="${2:-80}"

# The scan for generated files and the profile paths are relative to the
# repository root: make the profile path absolute, then go there.
[[ "$profile" = /* ]] || profile="$PWD/$profile"
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ ! -s "$profile" ]]; then
	echo "coverage-gate: profile '$profile' is missing or empty" >&2
	exit 2
fi

# Profile lines look like "<import path>/<file>.go:<range> <statements> <count>".
# With -coverpkg, one block can appear once per test binary, so blocks are
# deduplicated by their full key ($1), keeping the highest count. Then the
# statements and covered statements are summed per package directory under
# internal/. Packages with no statements are skipped.
# Generated files: *.gen.go with the header; one without it is an error.
generated=""
while IFS= read -r f; do
	if head -n 10 "$f" | grep -Eq '^// Code generated .* DO NOT EDIT\.$'; then
		generated+="$f "
	else
		echo "coverage-gate: $f is named *.gen.go but lacks the '// Code generated ... DO NOT EDIT.' header in its first lines" >&2
		exit 2
	fi
done < <(find internal -name '*.gen.go' -type f | sort)

report="$(awk -v generated="$generated" '
	BEGIN { n = split(generated, g, " ") }
	NR == 1 && /^mode:/ { next }
	{
		key = $1
		nst[key] = $2
		if (!(key in cnt) || $3 + 0 > cnt[key] + 0) cnt[key] = $3
	}
	END {
		for (k in nst) {
			file = k
			sub(/:.*/, "", file)
			skip = 0
			for (i = 1; i <= n; i++) {
				suffix = "/" g[i]
				if (length(file) >= length(suffix) && substr(file, length(file) - length(suffix) + 1) == suffix) skip = 1
			}
			if (skip) continue
			dir = file
			sub(/\/[^\/]*$/, "", dir)
			if (index(dir, "/internal/") == 0) continue
			stmts[dir] += nst[k]
			if (cnt[k] + 0 > 0) covered[dir] += nst[k]
		}
		total = 0
		hit = 0
		for (d in stmts) {
			if (stmts[d] == 0) continue
			printf "%s\t%d\t%d\n", d, stmts[d], covered[d]
			total += stmts[d]
			hit += covered[d]
		}
		printf "TOTAL\t%d\t%d\n", total, hit
	}
' "$profile" | sort)"

# Packages that go list finds under internal/ but that have no profile entry
# have no statements.
profile_dirs=$'\n'"$(cut -f1 <<<"$report")"$'\n'
echo "Coverage of internal/ packages (threshold ${threshold}%):"
while IFS=$'\t' read -r dir stmts hit; do
	[[ "$dir" == TOTAL ]] && continue
	awk -v d="$dir" -v s="$stmts" -v h="$hit" \
		'BEGIN { printf "  %-60s %6.1f%%  (%d/%d statements)\n", d, 100 * h / s, h, s }'
done <<<"$report"

all_pkgs="$(go list ./internal/...)"
while IFS= read -r pkg; do
	case "$profile_dirs" in
	*$'\n'"$pkg"$'\n'*) ;;
	*) echo "  ${pkg}  ignored (no statements)" ;;
	esac
done <<<"$all_pkgs"

total_line="$(grep '^TOTAL' <<<"$report")"
total_stmts="$(cut -f2 <<<"$total_line")"
total_hit="$(cut -f3 <<<"$total_line")"

if [[ "$total_stmts" -eq 0 ]]; then
	echo "coverage-gate: no statements in internal/, nothing to gate: PASS"
	exit 0
fi

pct="$(awk -v h="$total_hit" -v s="$total_stmts" 'BEGIN { printf "%.1f", 100 * h / s }')"
if awk -v p="$pct" -v t="$threshold" 'BEGIN { exit !(p + 0 >= t + 0) }'; then
	echo "coverage-gate: internal/ total ${pct}% >= ${threshold}%: PASS"
else
	echo "coverage-gate: internal/ total ${pct}% < ${threshold}%: FAIL" >&2
	exit 1
fi
