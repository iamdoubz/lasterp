// SPDX-License-Identifier: AGPL-3.0-only

// The English message catalog is the source of truth for user-facing strings.
// Every string rendered in the UI must have a key here and be looked up through
// useT()/t() — the hardcoded-string lint gate (scripts/i18n-lint.sh) enforces
// this. Values use the ICU MessageFormat subset understood by formatMessage:
// {arg} interpolation and {arg, plural, one {…} other {…}} / {arg, select, …}.

export const messages = {
  "app.title": "LastERP",
  "app.tagline": "The last ERP anyone will need to build — or buy.",
  "app.status.loading": "Loading…",
  "app.status.unreachable": "Kernel API unreachable",
  "app.items": "{count, plural, one {# item} other {# items}}",

  // Navigation shell
  "nav.label": "Main",
  "nav.signOut": "Sign out",
  "nav.skipToContent": "Skip to content",
  "nav.home": "Home",
  "nav.language": "Language",

  // Authentication
  "login.title": "Sign in",
  "login.tenant": "Tenant",
  "login.email": "Email",
  "login.password": "Password",
  "login.totp": "Authentication code",
  "login.totpHint": "Only required if two-factor authentication is enabled.",
  "login.submit": "Sign in",
  "login.pending": "Signing in…",
  "login.failed": "Sign-in failed. Check your details and try again.",
  "login.sso": "Sign in with single sign-on",
  "login.recoveryCode": "Recovery code",
  "login.recoveryHint": "Use one of your saved recovery codes if you cannot reach your authenticator.",
  "login.useRecovery": "Use a recovery code instead",
  "login.useAuthenticator": "Use an authentication code instead",

  // Account security — second factor (WP-1.12)
  "account.title": "Account security",
  "account.totp.heading": "Two-factor authentication",
  "account.totp.on": "Two-factor authentication is on.",
  "account.totp.off": "Two-factor authentication is off. Your account is protected by a password alone.",
  "account.totp.enable": "Turn on two-factor authentication",
  "account.enroll.password": "Password",
  "account.enroll.why":
    "Confirm it is you before adding a second factor, so a borrowed session cannot add one you do not control.",
  "account.enroll.failed": "That password was not accepted. Check it and try again.",
  "account.totp.starting": "Preparing…",
  "account.totp.scan":
    "Scan this code with your authenticator app, or enter the setup key by hand.",
  "account.totp.secret": "Setup key",
  "account.totp.confirmLabel": "Authentication code",
  "account.totp.confirmHint": "Enter the six-digit code your app is showing now.",
  "account.totp.confirm": "Confirm and turn on",
  "account.totp.confirming": "Confirming…",
  "account.totp.confirmFailed": "That code was not accepted. Check your app and try again.",
  "account.totp.cancel": "Cancel",
  "account.recovery.heading": "Recovery codes",
  "account.recovery.saveNow":
    "Save these now — this is the only time they are shown. Each one works once.",
  "account.recovery.copy": "Copy codes",
  "account.recovery.copied": "Copied.",
  "account.recovery.download": "Download codes",
  "account.recovery.done": "I have saved my codes",
  "account.recovery.remaining":
    "{count, plural, one {# recovery code left} other {# recovery codes left}}",
  "account.recovery.low":
    "You are running low on recovery codes. Turn two-factor authentication off and on again to get a fresh set.",
  "account.recovery.none":
    "You have no recovery codes left. If you lose your authenticator you will not be able to sign in.",
  "account.disable.heading": "Turn off two-factor authentication",
  "account.disable.why":
    "Confirm it is you: your password, plus a code from your authenticator or an unused recovery code.",
  "account.disable.password": "Password",
  "account.disable.totp": "Authentication code",
  "account.disable.recoveryCode": "Recovery code",
  "account.disable.submit": "Turn off two-factor authentication",
  "account.disable.pending": "Turning off…",
  "account.disable.failed": "Those details were not accepted. Check them and try again.",
  "account.disable.done": "Two-factor authentication is off.",
  // "Account security", not "Account": the chart of accounts is an object
  // called Account and sits in the same nav list. Two links reading "Account"
  // that go to unrelated screens is a bug for a user, not just for a test.
  "nav.account": "Account security",

  // Generic object screens
  "object.list.title": "{object}",
  "object.list.empty": "Nothing here yet.",
  "object.list.new": "New {object}",
  "object.list.open": "Open",
  "object.detail.title": "{object} detail",
  "object.detail.edit": "Edit",
  "object.detail.back": "Back to list",
  "object.form.newTitle": "New {object}",
  "object.form.editTitle": "Edit {object}",
  "object.form.save": "Save",
  "object.form.saving": "Saving…",
  "object.form.cancel": "Cancel",
  "object.column.actions": "Actions",
  // WP-2.7: the offline-first screens. "Pending" is the docs/04 §Upstream 1
  // flag — the row is on this device and the server has not seen it yet.
  "object.row.pending": "Not yet sent",
  "object.detail.delete": "Delete",
  "object.detail.deleting": "Deleting…",
  "object.offline.unavailable": "This device has no local copy of your data yet. Connect once to set it up.",

  // Invoicing
  "invoice.title": "Invoice",
  "invoice.status": "Status",
  "invoice.number": "Number",
  "invoice.total": "Total",
  "invoice.post": "Post to ledger",
  "invoice.posting": "Posting…",
  "invoice.period": "Period",
  "invoice.pdf": "Download PDF",
  "invoice.posted": "Posted. Journal entry {entry} created.",
  "invoice.journalEntry": "Journal entry",

  // Schema labels. Objects and fields arrive from /api/v1/meta/objects with
  // machine names; these are what a person should read. A schema key that is
  // absent (an overlay's custom field, a module shipped after this bundle)
  // falls back to the humanized field name, so the renderer stays total —
  // see meta/render.tsx labelFor.
  "schema.object.Account": "Account",
  "schema.object.Contact": "Contact",
  "schema.field.Account.code": "Code",
  "schema.field.Account.name": "Name",
  "schema.field.Account.type": "Account type",
  "schema.field.Account.parent": "Parent account",
  "schema.field.Account.currency": "Currency",
  "schema.field.Contact.name": "Name",
  "schema.field.Contact.email": "Email",
  "schema.field.Contact.kind": "Kind",
  "schema.field.Contact.locale": "Language",

  // Enum options. The values themselves are data — the server stores and
  // validates them — but what a person reads is a UI string, so they resolve
  // like field labels do. Only the objects the client renders need keys; an
  // option with none falls back to its humanized value (meta/render
  // optionLabel), which is why this list does not have to track every enum in
  // every module.
  "schema.option.Account.type.asset": "Asset",
  "schema.option.Account.type.liability": "Liability",
  "schema.option.Account.type.equity": "Equity",
  "schema.option.Account.type.income": "Income",
  "schema.option.Account.type.expense": "Expense",
  "schema.option.Contact.kind.customer": "Customer",
  "schema.option.Contact.kind.vendor": "Vendor",
  "schema.option.Contact.kind.both": "Customer and vendor",

  // Dashboards (docs/21 role packs). Pack titles and metric labels arrive
  // from the server in English; these keys translate them, falling back to
  // the server's own label for anything a pack adds later.
  "nav.dashboard": "Dashboard",
  "dashboard.period": "Period {period}",
  "dashboard.comparison": "vs {period}",
  "dashboard.noComparison": "No earlier period to compare with",
  "dashboard.omitted": "{count, plural, one {# tile is hidden because you cannot access its data} other {# tiles are hidden because you cannot access their data}}",
  "dashboard.none": "No dashboard is available for your permissions yet.",
  "dashboard.needsPeriods": "Add a fiscal period and post something to see live figures here.",
  "dashboard.pack.ceo": "Executive overview",
  "dashboard.pack.cfo": "Finance overview",
  "dashboard.pack.ar": "Accounts receivable",
  "dashboard.pack.ap": "Accounts payable",
  "metric.revenue": "Revenue",
  "metric.expenses": "Expenses",
  "metric.net_income": "Net income",
  "metric.total_assets": "Total assets",
  "metric.total_liabilities": "Total liabilities",
  "metric.cash_position": "Cash position",
  "metric.ar_outstanding": "Accounts receivable outstanding",
  "metric.ar_overdue": "Accounts receivable overdue",
  "metric.open_invoice_count": "Open invoices",

  // Offline sync — the outbox and its tray (WP-2.3b, docs/04 §Upstream)
  "sync.pending": "{count, plural, one {# unsent change} other {# unsent changes}}",
  "sync.pendingCapped": "{count} of {limit} unsent changes",
  "sync.unpersisted":
    "This browser has not granted permanent storage, so it may discard unsent changes without warning. Reconnect to send them.",
  "sync.outboxFull":
    "There are as many unsent changes as can be held safely without permanent storage. Reconnect to send them before making more.",
  "sync.lockedInAnotherTab": "Your offline data is open in another tab. Close it to work here.",
  "sync.unavailable": "Offline data is not available in this browser session.",
  "sync.conflicts.title": "Needs attention",
  "sync.conflicts.none": "Nothing needs your attention. Every change you made offline was accepted.",
  "sync.conflicts.intro":
    "{count, plural, one {# change you made offline was refused} other {# changes you made offline were refused}}. Nothing has been discarded — review each one below.",
  "sync.conflicts.column.change": "Change",
  "sync.conflicts.column.reason": "Why it was refused",
  "sync.conflicts.column.action": "Action",
  "sync.conflicts.change.create": "New {object}",
  "sync.conflicts.change.update": "Edit to a {object}",
  "sync.conflicts.change.delete": "Deletion of a {object}",
  "sync.conflicts.discard": "Discard this change",
  "nav.needsAttention": "Needs attention",

  // Shared status
  "status.loading": "Loading…",
  "status.error": "Something went wrong.",
  "status.capabilityDisabled": "This module is not enabled for your organisation.",
} as const;

export type MessageKey = keyof typeof messages;

export type Catalog = Record<MessageKey, string>;
