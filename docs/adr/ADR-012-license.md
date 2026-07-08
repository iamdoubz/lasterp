# ADR-012: License — AGPLv3 core, Apache-2.0 SDKs

**Status:** **Accepted** (Dan, 2026-07-07) · Repo: https://github.com/iamdoubz/lasterp

## Decision
- **Core server, web client, modules, migration factory: AGPLv3.** Anyone can use, self-host, modify, and sell hosting — but modifications offered as a network service must be shared back. This blocks the "hyperscaler forks our work into a closed managed SaaS" failure mode while staying 100% free software. Using LastERP (self-hosted or hosted) imposes nothing on users; AGPL obligations bind only those who *modify and redistribute/serve* it.
- **SDKs, client libraries, plugin PDKs, protocol/OpenAPI definitions, connector interfaces: Apache-2.0.** Plugins, integrations, and commercial extensions interact with zero licensing friction — plugin authors' code is theirs, any license they choose. (Plugins run out-of-process via WASM and communicate through the Apache-2.0 ABI — cleanly outside AGPL's derivative scope by design.)
- **DCO** (Developer Certificate of Origin) for all contributions; **no CLA** granting any single company relicensing rights — the project can never pull an open-core rug.
- Trademark ("LastERP") held separately; forks must rename.

## Alternatives considered
Apache-2.0/MIT everything (Dan's initial leaning): maximally adoptable, but leaves the hosted-service moat open to closed cloud forks. MIT rejected outright regardless — no patent grant. Full matrix preserved in git history of this file.

## Consequences
- `LICENSE` = AGPLv3 at repo root; `sdk/**`, `proto/**`, plugin ABI dirs carry Apache-2.0 with per-directory LICENSE files; SPDX headers enforced by CI lint from WP-0.1.
- The plugin ABI boundary doubles as the licensing boundary — keep it crisp; anything meant for third-party linking lives on the Apache side.
- README gets a short licensing FAQ (users owe nothing; hosters of modified versions share source; plugins are yours).
- Decision made before any external contribution — no relicensing consent burden exists.

## Sustainability model (informational)
Hosting, support/SLA, certified-plugin marketplace fees, migration/implementation partner program — never feature gates.
