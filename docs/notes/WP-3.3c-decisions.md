# WP-3.3c Automation webhooks — decisions

The roadmap line: *"Generalise WP-3.2a's outbound allowlist and audited client so a
non-plugin principal can own one, then land the `webhook` action. AC: an automation's
outbound destination is approved against the creating administrator's own authority and
audited exactly once per call, with the SSRF dialer guard proven to cover the new path."*

The WP exists because [WP-3.3-decisions.md §5a](WP-3.3-decisions.md) refused to half-build
it: WP-3.2a's client is shaped around a *plugin manifest* — the allowlist is
`manifest.Capabilities.HTTP`, the audit row is keyed to a plugin id, the dialer reads its
policy off the installed plugin — and an automation has none of those. What follows is the
shape that gives it one.

## 0. The generalisation is a new package, not an export from `kernel/plugins`

`kernel/automations` deliberately does not import `kernel/plugins`: it declares
consumer-side `Objects` and `Plugins` interfaces and lets the composition root supply
them. Exporting the outbound client from `kernel/plugins` would reverse that for the sake
of a transport.

So the transport half of `kernel/plugins/http.go` moves to **`kernel/outbound`**: the
policy, the client pool, the dialer guard, `isPublicIP`, the https-only target parse, the
redirect refusal, the size caps, the auditable-path truncation. `kernel/plugins/http.go`
keeps what is plugin-shaped — the manifest allowlist and the plugin principal — and calls
it. `plugins.HTTPPolicy` stays as an alias so the composition root and WP-3.2a's tests are
untouched.

The point of one package is that **there is one dialer guard**. A second copy of that code
is a second SSRF surface that will drift from the first, and the drift will be discovered
by whoever finds `169.254.169.254` in an audit log. A structural test asserts it: nothing
outside `kernel/outbound` constructs an `http.Client` or a `net.Dialer` for outbound use.

## 1. Two keys, and neither of them is "may create automations"

The AC says the destination is approved "against the creating administrator's own
authority". An administrator's authority is (object, action) tuples, and no tuple can say
*"may reach api.acme.com"*. So the host is not authorized — it is **registered**, and the
registration is the thing authority bounds.

- A **destination** is a tenant-scoped named row (`outbound_destinations`): an id, a host,
  and a pointer to the vaulted URL. Creating one is gated by **`Webhook:manage`**. That is
  the same decision an administrator makes approving a plugin manifest's `http:` block —
  *where may this deployment call out* — and it belongs to the same kind of person.
- An automation's `webhook` action names a **destination id**, never a URL. Its
  `Permissions()` gains **`Webhook:send`**, so `Save`'s existing INV-T3 bound already
  refuses a creator who does not hold it, and grants the automation exactly that and
  nothing more. A destination that is not registered is refused at `Save`, not at run
  time.

They are split because they are different powers. Folded together, anyone who can write an
automation picks the host — which is precisely the SSRF-and-exfiltration primitive §5a said
an easier-to-create-than-a-plugin surface must not acquire.

The `Webhook:send` check goes through `authz.Can`, which per
[ADR-022](../adr/ADR-022-expression-language.md) and WP-3.3a is satisfied by
**unconditional grants only**. A creator holding only a conditional `Webhook:send` cannot
create a webhook automation. That is the same fail-closed direction every other tuple in
`grantAuthority` already takes, and it is why this WP adds no second evaluation surface.

## 2. The destination's URL lives in the vault; the row holds the reviewable half

WP-3.2b found it the hard way: **a webhook's path is itself a credential**. A Slack
incoming webhook is `/services/T000/B000/<secret>`, and so is every "unguessable URL"
integration built the same way. INV-K1 forbids persisting secret material in plaintext, so
the URL cannot sit in a column and cannot sit in the automation's YAML — a definition that
`GET /api/v1/automations/{id}` returns to anyone holding `Automation:manage`.

So the row records `host` — the half an administrator reviews, and the half that is not a
credential — and a `secret_name` pointing at a `kernel/secrets` entry holding the full
`https://` URL. Sending reads it with reader `{Kind: "automation", ID: <id>}` and a grants
function that permits *exactly that one name*: the automation's analogue of a manifest's
`secrets:` list, so a webhook action is not a way to read the vault.

Before dialling, the resolved URL's host is asserted equal to the row's `host`. The name an
administrator reviewed is what binds; a secret rotated to point somewhere else does not
silently redirect the traffic to a destination nobody approved.

## 3. POST, a JSON envelope, and literals only

`ponytail:` a webhook is a POST. No per-destination method set, no GET webhooks — the
upgrade path is a `methods` column mirroring `plugins.HTTPHost.Methods`, and it can be
added the day someone has a receiver that needs it.

The body is a fixed envelope — `{automation, object, record_id, at}` — merged with the
action's optional literal `body:` map. Deliberately **no record fields and no expression**,
the same rule `field_update`'s `Set` already follows and for a stronger reason: which
fields of a record may leave the tenant is a data-egress policy, and this WP is not the
place to invent one. A receiver that needs the record has a `record_id` and an API.

**Named deferral:** *record projection in a webhook body* → the WP that ships an egress
policy (field-level, reviewable, per destination). No parse-time refusal is needed: the
field does not exist, and `KnownFields(true)` makes an attempt a parse error naming the
key.

No custom headers, for §2's reason: a header map in a YAML document that
`Automation:manage` can read is where an API key would end up in the clear. If a
destination needs an auth header, it belongs on the destination row as a second vault
reference — the shape is already there.

## 4. The call runs on the job queue, not in the feed sweep

The same argument `call_plugin` already makes, and this WP inherits it: network I/O inside
`RunOnce` puts one dead host's five-second dial timeout in front of every other automation
in the tenant. A new job kind `automation.webhook` carries the send, and WP-3.3b's queue
supplies retries, backoff and dead letters — the delivery semantics a webhook actually
needs — for nothing.

**"Audited exactly once per call" means per call, not per firing.** A retry is a second
request that really was made, and it gets its own row; a row per firing would understate
what left the building. The row is written *before* the socket opens, which is WP-3.2a's
ordering and its reasoning verbatim: no call happens without a row, and `audit_log` is
append-only so the row cannot be amended with the outcome afterwards. The outcome is in
`automation_runs`, which is the operator's other half.

## 5. Invariants this WP touches

- **INV-T3** — the creator's own authority bounds the automation's `Webhook:send`, and the
  destination registry is a ceiling no automation escapes. Refused, never narrowed.
- **INV-T4** — every outbound call is attributed to `automation:<id>` and audited exactly
  once before it is dialled.
- **INV-K1** — the URL is vaulted; the audit row records method, host and first path
  segment only; no route returns a destination's URL.
- **INV-X1** — the dialer guard the invariant's WP-3.2a extension rests on now serves a
  non-plugin caller. It is the *same code*, asserted structurally to be the only copy.
- **INV-T1** — `outbound_destinations` is tenant-scoped with an RLS policy, like every
  other table.
