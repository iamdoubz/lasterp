# ADR-001: Server language — Go

**Status:** Accepted · 2026-07-06 · **Amended 2026-07-31 (WP-1.10):** patch pin raised 1.26.4 → 1.26.5, and the pinning *mechanism* corrected — the `toolchain` directive cannot carry the pin (see Decision). · **Amended 2026-08-30:** patch pin raised 1.26.5 → **1.26.7**, clearing six standard-library advisories (GO-2026-6218 `net/url`, GO-2026-6090 `crypto/tls`, GO-2026-6089 and GO-2026-5026 `net/http`, GO-2026-6088 `encoding/xml`, GO-2026-5972 `encoding/asn1`) that `govulncheck` reported as reachable through the OIDC client, the server's own listener and the ECB rate parser. **1.27.0 is stable and deliberately not taken**: this ADR's policy is "new minor releases after one patch cycle", so 1.27 waits for 1.27.1.

## Context
Requirements: very fast, single-binary self-hosting, 50k concurrent users, high agent (Claude Code) development velocity, large hiring pool. Candidates: Rust, Go, Elixir, TypeScript/Node, JVM.

## Decision
**Go** for the entire server. Toolchain policy (set 2026-07-07, Dan): pin the latest stable release — currently **1.26.7** — in go.mod; adopt new patch releases within 2 weeks (they carry security fixes), new minor releases after one patch cycle.

The pin lives in the **`go` directive**, not the `toolchain` directive as this ADR originally specified. `go mod tidy` deletes a `toolchain` line whose version equals the `go` line, treating it as redundant — so the mechanism as written could not survive a tidy. `go 1.26.7` achieves the same thing: with the default `GOTOOLCHAIN=auto`, a developer on an older toolchain fetches 1.26.7 rather than building with what they happen to have. A `toolchain` directive becomes meaningful again only if we ever want to build with a *newer* toolchain than the language version we target, which is not the current policy. Bumping the pin means editing `go.mod`, the `setup-go` steps in `.github/workflows/ci.yml`, [docs/02](../02-TECH-STACK.md) and [CLAUDE.md](../../CLAUDE.md) together — and `kernel/plugins/testdata/go.mod`, the hostile corpus's own module, which is compiled by the same toolchain and was missed by the list until 2026-08-30. Performance-critical hotspots may later be implemented in Rust and loaded via WASM (same mechanism as plugins) — only with profiling evidence.

## Rationale
- Goroutines + netpoller comfortably handle tens of thousands of concurrent connections per node; 50k concurrent across a stateless cluster is routine Go territory.
- Static single binary is the backbone of the "solo mode" deployment story; Elixir/JVM/Node can't match it cleanly.
- Fast compiles and a simple language = fast, correct iteration for AI agents and humans; Rust's compile times and borrow-checker friction slow feature velocity on business logic that changes weekly.
- First-class SQLite and Postgres drivers, NATS embeds natively (it's written in Go), Extism has a mature Go host SDK (wazero, pure-Go — no CGO).
- Hiring: Go pool ≫ Elixir pool; ecosystem maturity ≫ both.

## Rejected
- **Rust everywhere:** correctness gold standard, but velocity cost too high for CRUD-heavy business logic.
- **Elixir:** best-in-class concurrency/fault tolerance, but small talent pool, weaker single-binary story, weaker typed-domain modeling.
- **TypeScript/Node:** shared language with frontend is nice, but weaker performance ceiling and worse single-binary/embedding story.

## Consequences
- Interfaces + code generation compensate for Go's modest type system (we codegen from metadata schemas).
- CGO stays banned (pure-Go SQLite driver: modernc.org/sqlite; wazero for WASM) to keep cross-compilation trivial.
