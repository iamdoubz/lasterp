#!/usr/bin/env bash
# Exercises i18n-lint.sh against a throwaway fixture repo, asserting both the
# pass path (strings via t(), brand allowlist, i18n-ignore escape) and the fail
# path (inline JSX text and hardcoded user-facing attributes). Run via
# `make test`.
set -euo pipefail

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

scripts_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cd "$tmp"
git init -q
git config user.email test@example.com
git config user.name test
mkdir -p web/src/i18n

# Compliant component: everything through t(); brand name allowlisted; an
# intentional literal opted out with i18n-ignore.
cat > web/src/Good.tsx <<'EOF'
export function Good() {
  return (
    <main>
      <h1>{t("app.title")}</h1>
      <p>{message}</p>
      <span>LastERP</span>
      <input placeholder={t("search.placeholder")} />
      <em>debug only</em> {/* i18n-ignore */}
    </main>
  );
}
EOF
# The i18n runtime itself is exempt and may contain literals.
cat > web/src/i18n/messages.ts <<'EOF'
export const messages = { "app.title": "LastERP" };
EOF
git add -A
git commit -q -m "compliant"

if ! "$scripts_dir/i18n-lint.sh" >/dev/null 2>&1; then
  echo "FAIL: i18n-lint.sh rejected a compliant tree" >&2
  "$scripts_dir/i18n-lint.sh" || true
  exit 1
fi
echo "PASS: i18n-lint.sh accepts compliant tree"

# Hardcoded JSX text node.
cat > web/src/BadText.tsx <<'EOF'
export function BadText() {
  return <h1>Welcome home</h1>;
}
EOF
git add -A
git commit -q -m "bad text"
if "$scripts_dir/i18n-lint.sh" >/dev/null 2>&1; then
  echo "FAIL: i18n-lint.sh accepted a hardcoded JSX text node" >&2
  exit 1
fi
echo "PASS: i18n-lint.sh rejects hardcoded JSX text"
git rm -q web/src/BadText.tsx
git commit -q -m "drop bad text"

# Hardcoded user-facing attribute.
cat > web/src/BadAttr.tsx <<'EOF'
export function BadAttr() {
  return <input placeholder="Search invoices" />;
}
EOF
git add -A
git commit -q -m "bad attr"
if "$scripts_dir/i18n-lint.sh" >/dev/null 2>&1; then
  echo "FAIL: i18n-lint.sh accepted a hardcoded placeholder attribute" >&2
  exit 1
fi
echo "PASS: i18n-lint.sh rejects hardcoded user-facing attribute"

echo "all i18n-lint fixture tests passed"
