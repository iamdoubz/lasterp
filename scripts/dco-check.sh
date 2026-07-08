#!/usr/bin/env bash
# Verifies every commit in a range carries a "Signed-off-by:" trailer (DCO).
# Usage: dco-check.sh <base-ref> [<head-ref>=HEAD]
set -euo pipefail

base="${1:?usage: dco-check.sh <base-ref> [<head-ref>]}"
head="${2:-HEAD}"

fail=0
while IFS= read -r sha; do
  [ -z "$sha" ] && continue
  if ! git log -1 --format=%B "$sha" | grep -qi '^Signed-off-by: .* <.*>$'; then
    echo "commit $sha missing Signed-off-by trailer (DCO)" >&2
    fail=1
  fi
done < <(git rev-list "$base..$head")

if [ "$fail" -ne 0 ]; then
  echo "DCO check failed" >&2
  exit 1
fi
echo "DCO check OK"
