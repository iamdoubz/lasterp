# ADR-001: Server language — Go

**Status:** Accepted · 2026-07-06

## Context
Requirements: very fast, single-binary self-hosting, 50k concurrent users, high agent (Claude Code) development velocity, large hiring pool. Candidates: Rust, Go, Elixir, TypeScript/Node, JVM.

## Decision
**Go** for the entire server. Toolchain policy (set 2026-07-07, Dan): pin the latest stable release — currently **1.26.4** — via the `toolchain` directive in go.mod; adopt new patch releases within 2 weeks (they carry security fixes), new minor releases after one patch cycle. Performance-critical hotspots may later be implemented in Rust and loaded via WASM (same mechanism as plugins) — only with profiling evidence.

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
