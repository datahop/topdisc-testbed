#!/usr/bin/env bash
# Copy a crawl dataset from where it is kept, verify it and unpack the event log.
#
#   CRAWL_SRC=<source> cmd/crawl/fetch.sh <crawl-date> [dir]
#   e.g. CRAWL_SRC=user@host:/srv/crawls/2026-09-18 cmd/crawl/fetch.sh 2026-09-18 data/crawl
#
# CRAWL_SRC (or a third argument) is the directory holding one crawl's
# SHA256SUMS: an http(s) base URL, an scp-style host:path, or a local path.
# There is no default: the dataset is no longer published on S3.
#
# Gets <dir>/<crawl-date>/{data/events.jsonl, derived/gap10, derived/gap30,
# crawl.log, bootnodes.txt}; then `python3 cmd/crawl/sessions.py
# <dir>/<crawl-date>/data/events.jsonl` rebuilds sessions with any threshold.
set -euo pipefail

date_="${1:?crawl date, e.g. 2026-09-18}"
dir="${2:-data/crawl}/$date_"
src="${3:-${CRAWL_SRC:-}}"

if [ -z "$src" ]; then
  cat >&2 <<EOF
fetch.sh: no source given (set CRAWL_SRC or pass it as the third argument).

The crawl dataset is no longer on S3. The project keeps it on its London
host (events in data/events.jsonl.gz, checksums in SHA256SUMS); ask the
maintainers for access. Scenarios do not need it: they read
scenarios/models/crawl-2026-09-18.json from the repo.
EOF
  exit 2
fi

case "$src" in
  http://*|https://*) get() { curl -fsSL --retry 3 -o "$dir/$2" "$src/$1"; } ;;
  *://*)              echo "fetch.sh: unsupported source $src" >&2; exit 2 ;;
  [!/]*:*)            get() { scp -q "$src/$1" "$dir/$2"; } ;;
  *)                  get() { cp "$src/$1" "$dir/$2"; } ;;
esac

mkdir -p "$dir/data" "$dir/derived/gap10" "$dir/derived/gap30"

get SHA256SUMS SHA256SUMS
get data/events.jsonl.gz data/events.jsonl.gz 2>/dev/null || get events.jsonl.gz data/events.jsonl.gz 2>/dev/null \
  || { echo "fetch: $src has no data/events.jsonl.gz" >&2; exit 1; }
get crawl.log crawl.log
get bootnodes.txt bootnodes.txt
for g in gap10 gap30; do
  for f in sessions nodes enr_changes; do get "derived/$g/$f.csv.gz" "derived/$g/$f.csv.gz"; done
  get "derived/$g/summary.txt" "derived/$g/summary.txt"
done

if command -v sha256sum >/dev/null; then
  (cd "$dir" && sha256sum -c SHA256SUMS)
else
  (cd "$dir" && shasum -a 256 -c SHA256SUMS)
fi
gunzip -kf "$dir/data/events.jsonl.gz"
for g in gap10 gap30; do
  for f in sessions nodes enr_changes; do gunzip -kf "$dir/derived/$g/$f.csv.gz"; done
done
echo "crawl $date_ in $dir ($(du -sh "$dir" | cut -f1))"
