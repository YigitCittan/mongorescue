#!/usr/bin/env bash
# ==============================================================================
# Coverage gates over Go cover profiles (the format of -coverprofile).
#
#   scripts/coverage.sh check PROFILE [MIN_TOTAL] [FLOORS]
#       Fails when the total statement coverage is below MIN_TOTAL percent (default
#       COVERAGE_MIN, or 0) or a package listed in FLOORS (default
#       coverage-floors.txt, lines of "<package directory> <percent>", # comments)
#       is below its floor, or is missing from the profile. Prints every package.
#   scripts/coverage.sh merge OUT PROFILE...
#       Merges profiles of several runs (unit and integration, each integration
#       matrix cell) into OUT: a block is covered when any run covered it. Blocks
#       are compared by position, so the profiles must come from the same revision;
#       integration profiles need -coverpkg=./... to carry the other packages.
#   scripts/coverage.sh report PROFILE...
#       Prints a Markdown table of the per-package coverage of each profile.
#
# Percentages are statement-weighted per package directory, as in
# "go tool cover -func" (whose total this reproduces).
# ==============================================================================
set -euo pipefail

MODULE="github.com/yigitcittan/mongorescue"

# stats PROFILE prints "<package> <statements> <covered>" lines and a "TOTAL" line.
# Duplicate blocks (from merged profiles) count once and are covered when any count
# is above zero.
stats() {
  awk -v module="$MODULE/" '
    /^mode:/ { next }
    {
      key = $1; stmts[key] = $2
      if ($3 + 0 > cnt[key] + 0) cnt[key] = $3
    }
    END {
      for (k in stmts) {
        file = k; sub(/:.*/, "", file)
        pkg = file; sub(/\/[^\/]*$/, "", pkg); sub("^" module, "", pkg)
        tot[pkg] += stmts[k]; all += stmts[k]
        if (cnt[k] + 0 > 0) { cov[pkg] += stmts[k]; allcov += stmts[k] }
      }
      for (p in tot) printf "%s %d %d\n", p, tot[p], cov[p] + 0
      printf "TOTAL %d %d\n", all, allcov + 0
    }' "$1" | sort
}

# pct STATEMENTS COVERED prints the percentage with one decimal.
pct() { awk -v t="$1" -v c="$2" 'BEGIN { printf "%.1f", (t > 0 ? 100 * c / t : 0) }'; }

cmd_check() {
  local profile=${1:?profile} min=${2:-${COVERAGE_MIN:-0}} floors=${3:-coverage-floors.txt}
  local status=0 out total_line p t c v
  [ -f "$profile" ] || { echo "no coverage profile $profile" >&2; exit 1; }
  out="$(stats "$profile")"
  echo "$out" | while read -r p t c; do
    [ "$p" != TOTAL ] || continue
    printf '  %-40s %6s%%\n' "$p" "$(pct "$t" "$c")"
  done
  total_line="$(echo "$out" | grep '^TOTAL ')"
  read -r _ t c <<<"$total_line"
  v="$(pct "$t" "$c")"
  echo "==> Total coverage: ${v}% (minimum ${min}%)"
  if ! awk -v v="$v" -v m="$min" 'BEGIN { exit (v + 0 < m + 0) ? 1 : 0 }'; then
    echo "coverage ${v}% is below ${min}%" >&2
    status=1
  fi
  if [ -f "$floors" ]; then
    local floor line
    while read -r line; do
      line="${line%%#*}"
      [ -n "${line//[[:space:]]/}" ] || continue
      read -r p floor <<<"$line"
      if ! grep -q "^${p} " <<<"$out"; then
        echo "package ${p} has a floor of ${floor}% but is not in the profile" >&2
        status=1
        continue
      fi
      read -r _ t c <<<"$(grep "^${p} " <<<"$out")"
      v="$(pct "$t" "$c")"
      if ! awk -v v="$v" -v m="$floor" 'BEGIN { exit (v + 0 < m + 0) ? 1 : 0 }'; then
        echo "package ${p}: coverage ${v}% is below its floor of ${floor}%" >&2
        status=1
      fi
    done <"$floors"
  fi
  [ "$status" -eq 0 ] || exit 1
  echo "==> coverage gates passed"
}

cmd_merge() {
  local out=${1:?output} f
  shift
  [ $# -gt 0 ] || { echo "merge needs at least one profile" >&2; exit 2; }
  {
    echo "mode: atomic"
    for f in "$@"; do grep -hv '^mode:' "$f"; done \
      | awk '{ if ($3 + 0 > cnt[$1] + 0 || !($1 in cnt)) cnt[$1] = $3; st[$1] = $2 }
             END { for (k in st) printf "%s %d %d\n", k, st[k], cnt[k] }' \
      | sort
  } >"$out"
}

cmd_report() {
  local f p t c
  echo "| Profile | Package | Coverage |"
  echo "| :--- | :--- | ---: |"
  for f in "$@"; do
    stats "$f" | while read -r p t c; do
      printf '| %s | %s | %s%% |\n' "$(basename "$f")" "$p" "$(pct "$t" "$c")"
    done
  done
}

main() {
  local cmd=${1:-}
  [ $# -gt 0 ] && shift
  case "$cmd" in
    check) cmd_check "$@" ;;
    merge) cmd_merge "$@" ;;
    report) cmd_report "$@" ;;
    *) sed -n '3,/^# ====/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | head -n 24; exit 2 ;;
  esac
}
main "$@"
