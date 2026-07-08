# ADR-007: Plugin system — WASM (Extism) server plugins + sandboxed UI modules

**Status:** Accepted · 2026-07-06

## Context
User-made plugins are a core promise. Third-party code must not be able to corrupt the ledger, leak tenant data, or take down the server. Native plugins (shared libs, subprocess) can't be sandboxed cheaply; script-only plugins (Lua/JS) lock authors into one language.

## Decision
- **Server plugins: WebAssembly via Extism** on the wazero runtime (pure Go, no CGO).
  - Authors write in Rust, Go, TypeScript/JS, Python, C#, Zig… (any Extism PDK language).
  - Each plugin ships a **manifest**: id, version, hook subscriptions, object overlays, and **requested capabilities** (read/write per object type, outbound HTTP to named hosts, secret access, schedule). Admin approves capabilities at install — nothing implicit.
  - Sandbox limits: memory cap, execution timeout, fuel/CPU metering, no filesystem, HTTP only through host functions that enforce the allowlist + audit every call.
  - Hook points: `before_validate`, `before_commit` (can reject), `after_commit` (async), scheduled jobs, HTTP endpoint handlers under `/ext/<plugin>/`, connector transforms, MCP tool providers.
- **UI plugins: ES modules** declaring named extension slots (dashboard widget, record sidebar, list action, full page). Unverified plugins render inside sandboxed iframes with a postMessage bridge; verified/first-party ones load in-process.
- **Distribution:** plugins are signed OCI-style bundles; a public registry plus private tenant registries. `lasterp plugin install <ref>`.

## Rationale
- Extism is the 2026 de-facto standard for exactly this (adopted by Navidrome, moonrepo, Helm HIP-0026 discussions); wazero keeps our no-CGO rule.
- Capability-based security turns "infinite customization" from a liability into a governed surface.

## Rejected
- Native Go plugins (`plugin` pkg): platform-fragile, zero sandboxing.
- Embedded JS (goja) only: single language, weaker isolation, no resource metering.
- Server-side Python à la Frappe: powerful but unsandboxable → the very upgrade/security hell we're avoiding.

## Consequences
- Host function ABI is a versioned public contract; breaking it requires a major version + compatibility shims.
- Kernel provides SDKs ("PDKs") with typed bindings generated from object metadata, per language (Rust, Go, TS, Python first).
- Plugin failures degrade gracefully: a crashing `after_commit` hook never rolls back the commit; a timing-out `before_commit` hook rejects with a clear error.
