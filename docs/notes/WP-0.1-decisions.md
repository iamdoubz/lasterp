# WP-0.1 decisions

Interpretations made to unblock implementation. Not ADR material; revisit if wrong.

1. **"Three deploy shapes" (CI build)** = solo single static binary, Docker Compose, Helm chart (docs/02 Deploy row, docs/22 tiers). WP-0.1 CI proves each *builds/lints* (binary compiles, `docker build` succeeds, `helm lint` passes on a minimal chart) — not that it deploys/serves under load. Deploy-topology testing is WP-10.1/22 territory.

2. **Go toolchain pin vs local env.** Local `go` is 1.26.2; CLAUDE.md pins 1.26.4. `go.mod` sets `toolchain go1.26.4` — with default `GOTOOLCHAIN=auto`, `go` auto-fetches 1.26.4 on first build. CI installs 1.26.4 explicitly via actions/setup-go.

3. **pnpm not preinstalled locally.** Corepack was removed from Node's default install as of Node 25+ (confirmed: `corepack enable` fails on Node 26 in CI, the devcontainer, and the Docker web-build stage). All three install pnpm directly via `npm install -g pnpm@9.15.0` instead; the version is also pinned via root `package.json`'s `"packageManager"` field for tooling that still reads it (e.g. editors). No new global-tool dependency beyond what Node already provides.

4. **"Hello-world API + web shell" (AC)** = Go server exposes `GET /healthz` and `GET /api/v1/hello` (JSON), and a minimal Vite+React page fetches `/api/v1/hello` and renders the response. `lasterp dev` (invoked by `make dev`) starts both the Go server and the Vite dev server as one command. "<5 min" is measured from a cold clone including dependency install.

5. **SPDX header lint + DCO check** implemented as small local shell/Go scripts run in the CI lint job — not a third-party GitHub App/action — per CLAUDE.md's "no new runtime dependencies without an ADR" and ponytail's stdlib-first ladder. They are dev-time/CI-time only, not runtime deps.

6. **Directory scaffolding scope.** Only what WP-0.1's AC needs gets real content: `cmd/lasterp`, `kernel/api` (hello-world only), `web/`. `sdk/`, `proto/`, and plugin-ABI dirs get a placeholder `LICENSE` (Apache-2.0) + `.gitkeep` per ADR-012's licensing-boundary requirement, since no code lands there until WP-3.x/3.7. `modules/`, `connectors/`, `clients/desktop/`, `tools/` get `.gitkeep` placeholders matching docs/02's planned layout, no logic.

7. **Invariants (docs/19).** WP-0.1 is pure scaffolding — no storage, events, tenancy, or money paths exist yet. No INV-* is implemented or testable at this WP; the Integrity Gauntlet CI *stage* itself is WP-0.8's job. No invariant property tests are registered here.

8. **DCO enforcement mechanics.** Check runs against the PR's commit range in CI (`git log <base>..HEAD`), verifying every commit trailer contains `Signed-off-by:`. Locally, `make dev` does not force `-s` on commits (that's a habit/hook concern, not a bootstrap concern) — CI is the enforcement point.
