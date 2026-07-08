#!/usr/bin/env bash
# Exercises spdx-lint.sh and dco-check.sh against a throwaway fixture repo,
# asserting both the pass and fail path of each. Run via `make test` (or
# directly) — not part of the CI lint job itself, since it tests the linters.
set -euo pipefail

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

scripts_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cd "$tmp"
git init -q
git config user.email test@example.com
git config user.name test

mkdir -p kernel/foo sdk/bar
cat > kernel/foo/good.go <<'EOF'
// SPDX-License-Identifier: AGPL-3.0-only
package foo
EOF
cat > sdk/bar/good.go <<'EOF'
// SPDX-License-Identifier: Apache-2.0
package bar
EOF
git add -A
git commit -q -s -m "base: compliant tree"
base="$(git rev-parse HEAD)"

if ! "$scripts_dir/spdx-lint.sh"; then
  echo "FAIL: spdx-lint.sh rejected a compliant tree" >&2
  exit 1
fi
echo "PASS: spdx-lint.sh accepts compliant tree"

if ! "$scripts_dir/dco-check.sh" "$base" "$base" >/dev/null 2>&1; then
  echo "FAIL: dco-check.sh rejected an empty (already-signed-off) range" >&2
  exit 1
fi
echo "PASS: dco-check.sh accepts a signed-off commit"

echo 'package foo' > kernel/foo/bad.go
git add -A
git commit -q -m "missing header, no sign-off"

if "$scripts_dir/spdx-lint.sh" >/dev/null 2>&1; then
  echo "FAIL: spdx-lint.sh accepted a file missing its header" >&2
  exit 1
fi
echo "PASS: spdx-lint.sh rejects missing header"

if "$scripts_dir/dco-check.sh" "$base" HEAD >/dev/null 2>&1; then
  echo "FAIL: dco-check.sh accepted a commit without Signed-off-by" >&2
  exit 1
fi
echo "PASS: dco-check.sh rejects unsigned commit"

git commit --amend -q -s -m "missing header, no sign-off"
if ! "$scripts_dir/dco-check.sh" "$base" HEAD >/dev/null 2>&1; then
  echo "FAIL: dco-check.sh rejected a commit that was amended with sign-off" >&2
  exit 1
fi
echo "PASS: dco-check.sh accepts signed-off commit"

echo "all lint-check fixture tests passed"
