# 23 — Write a LastERP plugin in an afternoon

This is the walkthrough behind ADR-007's promise: a developer with no LastERP
experience ships a working server plugin in an afternoon. Every step below is
executed by CI (`TestAfternoonTutorialCompletes`, `internal/app`), so if the
tutorial drifts from the product, the build goes red rather than the reader.

Design docs: [05-PLUGIN-SYSTEM.md](05-PLUGIN-SYSTEM.md) ·
[ADR-007](adr/ADR-007-plugin-system.md) · decisions in
[WP-3.2-decisions.md](notes/WP-3.2-decisions.md).

## What you need

- Go 1.26.7 (`wasip1/wasm` is a target of the standard toolchain — no TinyGo, no
  second compiler). Rust, TypeScript and Python scaffolds exist too; this
  tutorial uses Go because it is the one CI compiles.
- A running LastERP and a session token for an administrator who may install
  plugins (`plugin:manage`).

```sh
export LASTERP_URL=https://erp.example.com
export LASTERP_TOKEN=…            # an administrator's session token
```

## 1. Scaffold

```sh
lasterp plugin new --lang go -id com.acme.afternoon
cd afternoon
```

You get five files. Read `manifest.yaml` first — it is the whole of what your
plugin may do, and it is what an administrator approves at install:

```yaml
id: com.acme.afternoon
version: 0.1.0
lasterp: ">=0.1"
functions: [hello]
endpoints:
  - {path: /hello, fn: hello, methods: [GET]}
```

Nothing is implicit. A capability you do not list is a host function your module
**cannot import**, so the sandbox refuses to instantiate it rather than refusing
the call — you find out at load time, not in production.

`host.go` is your copy of the host bindings; it is yours to edit, and there is
no separately published SDK to wait on. `main.go` has one exported function.

## 2. Build

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

`-buildmode=c-shared` is what produces a *reactor* module — exported entry
points and no `main()` that runs to completion, which is what a plugin is.

## 3. Get a publisher key

A deployment installs no bundle it cannot verify, so sign yours:

```sh
lasterp plugin keygen -out publisher.key -id acme-2026
```

It prints one line. Send that line to whoever operates the LastERP you are
installing into; they add it to the file named by
`LASTERP_PLUGIN_TRUST_FILE` ([docs/09](09-SCALABILITY-DEPLOYMENT.md#plugin-publisher-trust-wp-32b)).
Keep `publisher.key` secret and back it up — it is how your users know a bundle
is from you.

## 4. Pack

```sh
lasterp plugin pack -manifest manifest.yaml -module plugin.wasm -key publisher.key
```

```
wrote afternoon-0.1.0.tar.gz
  plugin  com.acme.afternoon 0.1.0
  digest  9f2c…                 ← the identity your signature covers
  module  sha256:7a1b…          ← re-checked on every load, forever
  signer  acme-2026
```

The digest is computed from the contents, not from the archive, so re-packing
the same plugin gives the same identity.

## 5. Install

```sh
lasterp plugin install ./afternoon-0.1.0.tar.gz
```

The CLI downloads or reads the bundle, shows you what it contains, and posts it
to the API **as you** — an install is an approval decision, so it is attributed
to a person and bounded by that person's own permissions. A plugin can never be
granted a capability its approver does not hold.

The response is the approval record:

```
installed com.acme.afternoon 0.1.0 (sha256:7a1b…)
  serves   GET /ext/com.acme.afternoon/hello
```

## 6. Call it

```sh
curl -H "Authorization: Bearer $LASTERP_TOKEN" \
  "$LASTERP_URL/ext/com.acme.afternoon/hello"
# {"hello":"01H…"}
```

Callers need a session and `plugin:invoke`. Your plugin runs as **its own**
principal — it never inherits the caller's permissions, and it is never told how
the caller authenticated.

## 7. Uninstall

```sh
curl -X DELETE -H "Authorization: Bearer $LASTERP_TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" \
  "$LASTERP_URL/api/v1/plugins/com.acme.afternoon"
```

The route stops answering, the plugin's role and its stored state go with it.

---

## Now make it do something

### Typed bindings for your tenant's objects

```sh
lasterp plugin bindings -out objects.go -package main
```

Generated from *your tenant's* effective schema, so custom fields are in there
too. Regenerate after a schema change.

### Read data

Add the capability, then the import:

```yaml
capabilities:
  objects:
    - {type: Contact, access: read}
```

```go
//go:wasmimport extism:host/user lasterp_object_get
func hostObjectGet(uint64) uint64
```

Reads are capability-checked, authorized as your plugin, tenant-scoped by the
database, and audited. Module-owned documents — an **Invoice**, for instance —
are readable and never writable: they are created and posted through their
module's pipeline, and no plugin gets a way around that (INV-F5). A manifest
asking to write one is refused at install.

### React to changes

```yaml
hooks:
  - {event: Contact.before_create, fn: validate, mode: sync}   # can veto
  - {event: Contact.changed, fn: react, mode: async}           # after the commit
```

A **sync** hook runs inside the write path and may reject the write with a
message the user sees. It has a latency budget (50ms by default, 500ms ceiling)
and the administrator installing it is shown, in plain language, what raising it
costs per write.

An **async** hook is delivered from the change feed after the commit, at least
once. Write it so running twice is harmless — `lasterp_kv_set` with the record
id is the usual dedupe key, and both example plugins do exactly that. A plugin
never reacts to its own writes.

### Call an external service

```yaml
capabilities:
  secrets: [my_api_key]
  http:
    - {host: api.example.com, methods: [POST]}
```

HTTPS only, no redirects followed, size caps, and every call audited with the
destination — never the headers, the body, or more than the first path segment,
because for many webhook APIs the path *is* the credential. Your allowlist is
checked against the address actually dialled, so a name that resolves into a
private network is refused: put the secret half of a webhook URL in the vault,
and its host in the manifest.

### Worked examples

Two complete plugins live in [`examples/plugins/`](../examples/plugins/), both
built and driven end to end by CI:

- **commission-calc** — an async hook over posted invoices, state in `kv`, and a
  report served on its own `/ext/` route.
- **slack-notifier** — an async hook, a secret from the vault, and one audited
  outbound call.

## What is not here yet

- **`overlays:`** — a plugin that ships its own object or adds a field to one.
  It needs per-tenant metadata, which is **WP-3.2c**; the manifest refuses the
  field by name until then.
- **`schedule:` and `enqueue_job`** — scheduled work, with WP-3.3's job runner.
- **`emit_event`** — writing the event store from a plugin, which needs its own
  design (INV-E territory).
- **`mcp_tools:`** — WP-3.4.

Each is refused at install naming the WP that will land it, so a plugin never
installs with a declaration that silently does nothing.
