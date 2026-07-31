#!/usr/bin/env bash
# Boot a real LastERP server for the Playwright suite: build the binary and the
# web bundle, bootstrap a tenant on a fresh SQLite database, then serve. This is
# the shipped `lasterp serve` path — the bundle is served by the binary from the
# same origin the API is on, so the e2e exercises the real cookie and CORS
# posture rather than a dev-server proxy.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

PORT="${LASTERP_E2E_PORT:-8099}"
TENANT="${LASTERP_E2E_TENANT:-acme}"
EMAIL="${LASTERP_E2E_EMAIL:-admin@example.com}"
PASSWORD="${LASTERP_E2E_PASSWORD:-e2e-p4ssw0rd}"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

export LASTERP_DSN="$workdir/lasterp.db"
export LASTERP_ADDR=":$PORT"
export LASTERP_WEB_ROOT="$PWD/web/dist"

echo "e2e: building lasterp binary" >&2
CGO_ENABLED=0 go build -o "$workdir/lasterp" ./cmd/lasterp

if [ ! -f "$LASTERP_WEB_ROOT/index.html" ]; then
  echo "e2e: building web bundle" >&2
  pnpm --dir web run build >&2
fi

echo "e2e: bootstrapping tenant $TENANT" >&2
LASTERP_BOOTSTRAP_PASSWORD="$PASSWORD" \
  "$workdir/lasterp" bootstrap --tenant "$TENANT" --name "Acme Inc" --email "$EMAIL" >&2

# A dashboard on an empty tenant is a grid of zeroes, so the suite seeds the
# same demo book an operator would (WP-1.8). It writes through the ordinary
# posting pipeline, so the e2e exercises real figures rather than fixtures.
echo "e2e: seeding demo book" >&2
"$workdir/lasterp" demo --tenant "$TENANT" --email "$EMAIL" >&2

echo "e2e: serving on $LASTERP_ADDR" >&2
exec "$workdir/lasterp" serve
