#!/usr/bin/env bash
# ==============================================================================
# The manual PITR soak run (docs/testing.md#pitr-soak-test): continuous oplog
# collection on a write-heavy three-member replica set for SOAK_DURATION (default
# 168h, seven days), with production-like settings (60-second chunks, a base
# every 6 hours, 4 bases kept) and a JSON report. It wraps
# scripts/test-replset-docker.sh with RS_SUITE=soak; run it on a machine that
# stays up, for example under nohup or in tmux:
#
#   nohup ./scripts/soak-pitr.sh > soak.log 2>&1 &
#
# Every MONGORESCUE_SOAK_* variable can be overridden, and so can MONGO_IMAGE and
# TOOLS_VERSION. The run fails at the first broken invariant and prints why; the
# report (SOAK_REPORT, default ./pitr-soak-<date>.json) holds the samples, the
# largest object count and lag, and the final chain test's replay rates.
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export RS_SUITE=soak
export MONGORESCUE_SOAK_DURATION="${SOAK_DURATION:-${MONGORESCUE_SOAK_DURATION:-168h}}"
export MONGORESCUE_SOAK_CHUNK_SECONDS="${MONGORESCUE_SOAK_CHUNK_SECONDS:-60}"
export MONGORESCUE_SOAK_BASE_EVERY="${MONGORESCUE_SOAK_BASE_EVERY:-6h}"
export MONGORESCUE_SOAK_KEEP_BASES="${MONGORESCUE_SOAK_KEEP_BASES:-4}"
export MONGORESCUE_SOAK_RATE="${MONGORESCUE_SOAK_RATE:-400}"
export MONGORESCUE_SOAK_SAMPLE="${MONGORESCUE_SOAK_SAMPLE:-5m}"
export MONGORESCUE_SOAK_REPORT="${SOAK_REPORT:-$PWD/pitr-soak-$(date -u +%Y%m%dT%H%M%SZ).json}"
# A larger oplog holds more than a day of this load, as in production.
export RS_OPLOG_MB="${RS_OPLOG_MB:-8192}"
export RS_LOG_DIR="${RS_LOG_DIR:-$PWD/pitr-soak-logs}"

printf '==> PITR soak for %s, report %s\n' "$MONGORESCUE_SOAK_DURATION" "$MONGORESCUE_SOAK_REPORT"
exec "$ROOT/scripts/test-replset-docker.sh"
