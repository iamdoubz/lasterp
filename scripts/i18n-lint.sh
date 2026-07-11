#!/usr/bin/env bash
# Hardcoded-string lint gate (WP-0.7, docs/17): all user-facing UI strings must
# flow through the translation layer (t()/useT()), never be written inline.
#
# Scans tracked web/src/**/*.tsx for:
#   - JSX text nodes containing letters that are not a {expression}
#   - user-facing attributes: placeholder=, title=, aria-label=, alt=
# and fails when they hold a literal string instead of a {t(...)} call.
#
# Escapes: a line containing "i18n-ignore", and the brand allowlist below.
# The i18n runtime itself (web/src/i18n) and test files are exempt.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Brand names / proper nouns that are intentionally not translated.
allowlist='LastERP'

fail=0
report() {
  echo "hardcoded user-facing string (route through t()): $1:$2: $3" >&2
  fail=1
}

while IFS= read -r -d '' f; do
  case "$f" in
    *.tsx) ;;                                 # scan JSX components only
    *) continue ;;
  esac
  case "$f" in
    web/src/i18n/*|*.test.tsx) continue ;;    # runtime & tests are exempt
  esac

  lineno=0
  while IFS= read -r line; do
    lineno=$((lineno + 1))

    # Explicit opt-out.
    case "$line" in *i18n-ignore*) continue ;; esac

    # Strip allowlisted tokens so brand-only text doesn't trip the gate.
    scan="${line//$allowlist/}"

    # JSX text node: >...text...< on one line, letters present, no { } braces
    # (a {expr} child is already going through code, not a literal).
    if echo "$scan" | grep -qE '>[^<>{}]*[A-Za-z][^<>{}]*<'; then
      report "$f" "$lineno" "$line"
      continue
    fi

    # User-facing string attributes with a quoted literal value.
    if echo "$scan" | grep -qE '(placeholder|title|aria-label|alt)="[^"]*[A-Za-z][^"]*"'; then
      report "$f" "$lineno" "$line"
    fi
  done < "$f"
done < <(git ls-files -z -- web/src)

if [ "$fail" -ne 0 ]; then
  echo "i18n lint failed: move the strings above into web/src/i18n/messages.ts and render via t()." >&2
  exit 1
fi
echo "i18n lint OK"
