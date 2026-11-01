#!/usr/bin/env bash
# Coverage gate for internal/: fails when the statement coverage of all
# internal/ packages together is below the threshold.
#
# Usage: scripts/coverage-gate.sh [coverprofile] [threshold-percent]
#   coverprofile       default coverage.out, written by:
#                      go test -race -coverpkg=./internal/... -coverprofile=coverage.out ./...
#   threshold-percent  default 80
#
# Packages without statements (for example doc.go-only packages) have no
# blocks in the profile. They are listed as ignored and never fail the gate.
set -euo pipefail

profile="${1:-coverage.out}"
threshold="${2:-80}"

if [[ ! -s "$profile" ]]; then
	echo "coverage-gate: profile '$profile' is missing or empty" >&2
	exit 2
fi

# Profile lines look like "<import path>/<file>.go:<range> <statements> <count>".
# Sum statements and covered statements per package directory under internal/.
report="$(awk '
	NR == 1 && /^mode:/ { next }
	{
		split($0, f, " ")
		file = f[1]
		sub(/:.*/, "", file)
		dir = file
		sub(/\/[^\/]*$/, "", dir)
		if (index(dir, "/internal/") == 0) next
		stmts[dir] += f[2]
		if (f[3] > 0) covered[dir] += f[2]
		total += f[2]
		hit += (f[3] > 0 ? f[2] : 0)
	}
	END {
		for (d in stmts) printf "%s\t%d\t%d\n", d, stmts[d], covered[d]
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
