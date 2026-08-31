#!/usr/bin/env bash
# Checks every tracked .go/.ts/.tsx/.js source file for an SPDX-License-Identifier
# header in its first 5 lines. Zone is determined by path: sdk/, proto/,
# kernel/plugins/abi/ and examples/ are Apache-2.0; everything else is
# AGPL-3.0-only. (ADR-012: the plugin ABI boundary doubles as the licensing
# boundary — anything a third party links against or starts their own code from
# lives on the Apache side, and an example plugin is exactly that.)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

fail=0

apache_zone() {
  case "$1" in
    sdk/*|proto/*|kernel/plugins/abi/*|examples/*) return 0 ;;
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

# `git ls-files` sees tracked files only, so a green run here says nothing about
# source you have written but not staged — and CI checks it the moment you
# commit. That gap turned a local "SPDX lint OK" into a red CI run on WP-3.1a
# (five new corpus files, all unstaged when the check ran). Warn rather than
# fail: an unstaged scratch file is not an error, but silence about it is how
# the trap works.
untracked=$(git ls-files -o --exclude-standard -- '*.go' '*.ts' '*.tsx' '*.js' \
  ':!:*_test.go' ':!:*.test.ts' ':!:*.test.tsx' | grep -v -e '/node_modules/' -e '/dist/' || true)
if [ -n "$untracked" ]; then
  echo "note: not checked because they are untracked (CI will check them once committed):" >&2
  echo "$untracked" | sed 's/^/  /' >&2
fi

echo "SPDX lint OK"
