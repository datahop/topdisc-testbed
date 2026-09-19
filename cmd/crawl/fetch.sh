#!/usr/bin/env bash
# Download a published crawl from S3, verify it and unpack the event log.
#
#   cmd/crawl/fetch.sh <crawl-date> [dir]      e.g. fetch.sh 2026-09-18 data/crawl
#
# Gets <dir>/<crawl-date>/{data/events.jsonl, derived/gap10, derived/gap30,
# crawl.log, bootnodes.txt}; then `python3 cmd/crawl/sessions.py
# <dir>/<crawl-date>/data/events.jsonl` rebuilds sessions with any threshold.
set -euo pipefail

BASE="https://datahop-testbed-data.s3.amazonaws.com/crawl"
date_="${1:?crawl date, e.g. 2026-09-18}"
dir="${2:-data/crawl}/$date_"
mkdir -p "$dir/data" "$dir/derived/gap10" "$dir/derived/gap30"

get() { curl -fsSL --retry 3 -o "$dir/${2:-$1}" "$BASE/$date_/$1"; }

get SHA256SUMS
get events.jsonl.gz data/events.jsonl.gz
get crawl.log
get bootnodes.txt
for g in gap10 gap30; do
  for f in sessions nodes enr_changes; do get "derived/$g/$f.csv.gz"; done
  get "derived/$g/summary.txt"
done

(cd "$dir" && sha256sum -c SHA256SUMS)
gunzip -kf "$dir/data/events.jsonl.gz"
for g in gap10 gap30; do
  for f in sessions nodes enr_changes; do gunzip -kf "$dir/derived/$g/$f.csv.gz"; done
done
echo "crawl $date_ in $dir ($(du -sh "$dir" | cut -f1))"
