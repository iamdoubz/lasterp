# Example plugins

Worked examples for [docs/23-PLUGIN-TUTORIAL.md](../docs/23-PLUGIN-TUTORIAL.md).
Both are built from source and driven end to end by CI, so they cannot rot into
a snippet that no longer compiles.

| Example | Shows |
|---|---|
| [commission-calc](plugins/commission-calc/) | An async hook over posted invoices, at-least-once dedupe in plugin-scoped `kv`, and a report served on its own `/ext/` route |
| [slack-notifier](plugins/slack-notifier/) | An async hook, a credential read from the vault, and one audited outbound call to a manifest-declared host |

**Apache-2.0**, unlike the rest of this repository (ADR-012): everything a third
party links against or starts their own code from lives on the Apache side, and
copying an example into your own plugin is the point of it existing.
