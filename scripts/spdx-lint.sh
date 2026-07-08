#!/usr/bin/env bash
# Checks every tracked .go/.ts/.tsx/.js source file for an SPDX-License-Identifier
# header in its first 5 lines. Zone is determined by path: sdk/, proto/, and
# kernel/plugins/abi/ are Apache-2.0; everything else is AGPL-3.0-only.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

fail=0

apache_zone() {
  case "$1" in
    sdk/*|proto/*|kernel/plugins/abi/*) return 0 ;;
    *) return 1 ;;
  esac
}

while IFS= read -r -d '' f; do
  case "$f" in
    */node_modules/*|*/dist/*|*/.git/*) continue ;;
  esac
  if apache_zone "$f"; then
    want="Apache-2.0"
  else
    want="AGPL-3.0-only"
  fi
  if ! head -n 5 "$f" | grep -q "SPDX-License-Identifier: $want"; then
    echo "missing/wrong SPDX header (want $want): $f" >&2
    fail=1
  fi
done < <(git ls-files -z -- '*.go' '*.ts' '*.tsx' '*.js' \
  ':!:*_test.go' ':!:*.test.ts' ':!:*.test.tsx' ':!:web/vite.config.ts')

if [ "$fail" -ne 0 ]; then
  echo "SPDX lint failed" >&2
  exit 1
fi
echo "SPDX lint OK"
