# WP-3.1 — Plugin host: decisions

Interpretation of [docs/11-ROADMAP.md](../11-ROADMAP.md) WP-3.1 against
[ADR-007](../adr/ADR-007-plugin-system.md), [docs/05](../05-PLUGIN-SYSTEM.md),
[docs/19](../19-DATA-INTEGRITY.md) §1–2 and [ADR-014](../adr/ADR-014-self-evolution-governance.md).
Written before the code, per CLAUDE.md.

## 0. Scope — this WP is split into two PRs

One roadmap line ("wazero + Extism integration, manifest/capabilities, hook points, resource
limits, circuit breakers") covers a sandbox, an authorization model, an install lifecycle, a
dispatch surface in the write path, and a failure policy. That is not one PR, and the two halves
have genuinely different risk: the first is *containment*, the second is *transaction safety*.
Same precedent as WP-1.6, WP-2.2 and WP-2.3.

- **PR-A (WP-3.1a) — the sandbox and the capability model.** `kernel/plugins`: manifest parsing,
  install/approval storage, the wazero+Extism runtime with its limits, the host-function surface,
  and the hostile-plugin corpus. Carries **INV-X1**.
- **PR-B (WP-3.1b) — the hook surface.** `before_*`/`after_*` dispatch at the write choke point,
  veto semantics, async delivery, circuit breakers, `/ext/<plugin>/` endpoints. Carries
  **INV-X2**, which needs a hook inside a transaction before it can be violated.

The AC ("hostile-plugin test suite … all contained") belongs to A. B adds the partial-commit
scenarios to the same suite.

## 1. Extism on wazero, as ADR-007 says

Two new runtime dependencies — `github.com/tetratelabs/wazero` and `github.com/extism/go-sdk` —
both named in ADR-007, so the no-new-dependencies rule is already satisfied by that ADR and this
WP does not reopen it. wazero keeps the no-CGO rule (docs/02).

Extism rather than raw wazero because the ABI is the expensive part: memory ownership, host
function calling convention, plugin lifecycle and the PDK story for five languages. Writing our
own would be inventing what ADR-007 chose off the shelf, and the "afternoon plugin" promise
(WP-3.2) rests on authors having a PDK already.

## 2. Fuel metering does not exist on wazero, and docs/05 is amended rather than quietly missed

docs/05 §Host runtime rules says "memory cap …, wall-clock timeout …, CPU fuel metering". wazero
has no fuel/gas counter (that is a wasmtime feature); its interruption mechanism is context
cancellation, which is wall-clock. **So the shipped controls are: memory cap, wall-clock timeout
enforced by context cancellation, and a hard cap on host-call count** — the last being the cheap
stand-in for "this plugin is spinning through the API" that fuel would otherwise catch.

That is enough for the AC (an infinite loop is killed by the deadline, a memory bomb by the cap),
but the doc must not keep promising a knob the runtime does not have. docs/05 is amended in this
PR, in the same spirit as WP-2.5 amending docs/08 rather than redefining an AC to match what got
built.

## 3. A plugin is its own principal, and its permissions are an intersection

The question the roadmap line leaves open is *whose authority a host call runs under*. Three
candidates: the plugin acts as the user who triggered it; the plugin acts as an administrator; the
plugin is a principal of its own.

**It is a principal of its own** — `plugin:<id>` — for two reasons. Attribution (INV-T4) must name
the plugin, or an audit trail says a user created a row they never saw. And a plugin acting *as*
the triggering user would inherit whatever that user can do, which makes its effective power vary
per caller and impossible to review at install time.

At install, every permission the manifest requests is checked against the approving
administrator's own grants. A plugin can therefore never exceed the person who approved it, which
is INV-T3's permission-floor rule applied to installation: an approval may narrow, never widen. An
admin who cannot post journal entries cannot install a plugin that posts them.

**A capability the approver lacks refuses the install rather than narrowing it** — the mechanism
changed from "intersect" while building, and this is the better answer. Installing a plugin with
silently less authority than it declared produces something that "installed fine" and then fails
at runtime for reasons nobody can see, and it makes the approval screen a lie. The administrator
is told which capability they lack, and either obtains it or does not install. Same rule as the
manifest validation in §0: refuse, never quietly ignore.

## 4. No ambient authority means the sandbox is empty by default

The wazero module is instantiated with no WASI filesystem, no network, no environment, no clock
beyond a monotonic one, and no host functions except those the manifest's approved capabilities
enable. A plugin that asked for nothing gets a module that can compute and return bytes.

Host functions land in this PR: `lasterp_log`, `lasterp_object_get/query/create/update`,
`lasterp_secret_get`. Each takes its authority from the invocation's plugin principal and
re-checks authz per call — the capability list is what the *host function table* is built from,
and authz is what each call is measured against. Two gates, deliberately: the manifest is reviewed
by a human once, and authz is enforced every time.

**`http.request` is not one of them, and the sandbox has no network at all.** ADR-007 requires
outbound HTTP to be allowlisted *and audited on every call*; Extism's built-in client enforces an
allowlist and audits nothing. Shipping it would satisfy the sentence and not the requirement, so
`AllowedHosts` is empty and a manifest that declares `http:` is **refused at install** with the
name of the WP that will add an audited client. Refused rather than ignored: a plugin that
installs and then silently cannot do what it declared is the failure mode, and the same silence
could one day drop a capability an administrator believed they were reviewing.

**Discovered while building, and worth writing down: the enforcement is stronger than designed.**
Because the host-function table *is* the module's import surface, a plugin whose imports exceed its
approved capabilities cannot be instantiated — wazero refuses the link. INV-X1 is therefore not
"the host function checks a capability" but "the function does not exist to call", which also
means an author must declare every capability they import or their plugin will not load at all.
`TestUngrantedHostFunctionsCannotEvenBeImported` is that property.

`secrets.get` is the seam WP-3.0 left: `secrets.Get` already takes a `Grants` predicate, and this
PR supplies the one that consults the plugin's approved `secrets:` list instead of `AllowAll`.

## 5. The hostile corpus is Go compiled by the pinned toolchain, not committed binaries

The AC needs modules that loop forever, allocate without bound, and try to reach data they were
not granted. Three ways to get them: commit prebuilt `.wasm`, require TinyGo in CI, or compile Go
to `wasip1/wasm` with the toolchain the repo already pins.

**The third.** `go tool dist list` includes `wasip1/wasm` on Go 1.26.5, so the corpus is readable
Go source in `kernel/plugins/testdata/` compiled at test time. Committed binaries are unreviewable
blobs in an integrity-critical repo — a hostile corpus nobody can read is a hostile corpus nobody
can tell has stopped being hostile. A second toolchain in CI is a dependency in everything but
name.

Containment is a property of the *runtime configuration* rather than of the plugin ABI, so the
corpus needs no Extism PDK: a bare wasip1 module is a fair adversary for the memory cap and the
deadline. The happy-path plugin, which does need the ABI, is the one place the PDK is used.

Every containment test asserts the fault actually fired (the loop really did not terminate on its
own, the allocation really was refused), in the shape WP-2.3a established: a fault-injection suite
that quietly stops injecting still reports green.

## 6. Installation is an operator action with an approval record

`plugins` is a tenant-scoped table holding the manifest, the approved capability set, the module
bytes and who approved them. Module bytes live in the database rather than on disk so a
multi-node deployment needs no shared filesystem, with a size cap.

Signing, registries and version solving are **WP-3.2** and are not smuggled in here; this PR
installs a module the administrator already has, and records the bytes' SHA-256 so WP-3.2 has
something to verify against. The hash is re-computed on every load, not trusted from the column: a
module that no longer matches the digest recorded at approval is not the module anyone approved.

**No `lasterp plugin install` CLI.** The roadmap line does not ask for one, and installing
untrusted code is an approval decision that must be attributable to a person (INV-T4) and bounded
by *that person's* grants (INV-T3) — both of which the authenticated API gives exactly and a CLI
running as the operator's database role would have to fake. `POST /api/v1/plugins` is the install
path. WP-3.2's `lasterp plugin install <ref>` is a registry client and belongs with the registry.

## 7. Nothing here is reachable by an autonomous process

ADR-014's constitutional list is untouchable, and a plugin host is exactly the side door the
autonomy rules warn about. Installation requires a human administrator's approval of a capability
set; no host function grants, installs, or approves anything, and there is no API by which a
plugin installs a plugin. Recorded here because the absence is deliberate and easy to erode.

## 8. PR-B plan review (2026-08-05, before any B code)

B was reviewed against the code rather than against its roadmap line, which changed its shape and
its size. Recorded here because the *reasons* are what a later reader needs; the outcomes live in
[docs/05](../05-PLUGIN-SYSTEM.md) and the roadmap.

**Sync hooks run before the transaction opens, not inside it.** `CRUD.Create` authorizes and
validates before calling `tenancy.WithTenant`, which hands us a dispatch point outside any
transaction. Running a wazero call inside one would hold a Postgres transaction — or SQLite's
single write lock — for the length of a plugin's wall-clock budget, which is the starvation
WP-2.3b already fought. The payoff is that **INV-X2 becomes structural**, in the shape of INV-S2:
no plugin code runs inside a transaction, asserted against the dispatch sites, rather than a
runtime check hoping to catch a partial commit. The residual is real and stated rather than
hidden: a hook that vetoes on state it read can be raced by a concurrent write before the commit,
which is the window ordinary validation already has.

**Async delivery rides the change feed, not a broker.** `changefeed.Read` is ordered, resumable
and exactly-once-observed (INV-S5, since WP-2.1). An `after_commit` runner is a feed consumer with
a per-plugin cursor; dead-letter is a table of failed deliveries and the circuit breaker parks the
cursor. docs/05's "at-least-once from JetStream" is satisfied without deploying JetStream, and
solo mode stays one binary.

**Sync-hook latency is a tenant dial, priced rather than refused.** Default 50ms; a manifest may
raise it to a hard ceiling it cannot exceed; raising it warns the installing administrator in
plain language what it costs per write. The docs/09 p95 describes the system LastERP ships, and a
tenant adding work to every write has made a business trade-off — commandment 9's "infinite dials"
applied to latency.

The load-bearing half is not the warning. **Per-plugin hook latency is measured and attributed at
runtime**, because the person who feels the latency is usually not the person who installed the
plugin, and without attribution a slow plugin makes "the ERP is slow" — the incumbent failure
docs/14 exists to avoid. With it, the same slowness reads as "com.acme.x adds 180ms to every
Invoice write", which is actionable and true.

**A plugin does not react to its own writes.** Self-suppression on the audit actor (`plugin:<id>`,
which already exists) rather than a depth counter: cut the loop at the source instead of bounding
it after the fact. The consequence is deliberate — a plugin genuinely cannot chain off its own
output.

**Three things left B, each to an owner rather than to silence** — `/ext/<plugin>/` endpoints to
WP-3.2 (a plugin-declared route needs its own authorization answer, and designing it before an
example plugin exists to call it means guessing), `enqueue_job` + `schedule:` to WP-3.3 (which owns
the job runner, since a scheduler built inside the plugin host would be a second one the moment
automations needed theirs), and `emit_event` to nothing yet (untrusted code writing the event store
is INV-E territory and deserves its own design). All three are already refused by A's manifest
validation, which names the owning WP — so an author is told what they are waiting on rather than
installing something that silently never fires.
