#!/usr/bin/env bash
# ==============================================================================
# Runs Go fuzz targets one after another, each for FUZZTIME (make fuzz).
#
# Usage: scripts/fuzz.sh FUZZTIME <package>:<FuzzFunc>...
#
# Go 1.26's fuzzing coordinator can end a run with "context deadline exceeded"
# when -fuzztime runs out although no input failed: the deadline races with the
# suppression of the cancellation error (golang/go#75804, fixed in Go 1.27). Such a
# run found nothing, so it passes with a warning. A failing input ("Failing input
# written to testdata/fuzz/..."), a panic or any other error still fails the run.
# The workaround can go once the module requires Go 1.27.
# ==============================================================================
set -euo pipefail

if [ $# -lt 2 ]; then
	echo "usage: $0 FUZZTIME <package>:<FuzzFunc>..." >&2
	exit 2
fi
fuzztime=$1
shift

log=$(mktemp)
trap 'rm -f "$log"' EXIT

# deadline_only reports whether the go test output in $1 failed fuzz target $2
# with nothing but the spurious "context deadline exceeded" of golang/go#75804.
deadline_only() {
	grep -q 'Failing input written' "$1" && return 1
	awk -v name="$2" '
		$0 ~ "^--- FAIL: " name " \\(" { fail++; want = 1; next }
		want == 1 { ok = ($0 == "    context deadline exceeded"); want = 0; next }
		/^    / || /^panic:/ { other++ }
		END { exit !(fail == 1 && ok && other == 0) }
	' "$1"
}

for target in "$@"; do
	pkg=${target%%:*}
	name=${target##*:}
	echo "==> Fuzzing $name in $pkg for $fuzztime..."
	if go test -run='^$' -fuzz="^${name}\$" -fuzztime="$fuzztime" "$pkg" 2>&1 | tee "$log"; then
		continue
	fi
	if deadline_only "$log" "$name"; then
		msg="$name: the fuzzing coordinator reported a spurious 'context deadline exceeded' at the end of -fuzztime (golang/go#75804); no failing input was found"
		if [ -n "${GITHUB_ACTIONS:-}" ]; then
			echo "::warning::$msg"
		else
			echo "warning: $msg"
		fi
		continue
	fi
	exit 1
done
