// SPDX-License-Identifier: AGPL-3.0-only

import { request, type ListResponse, type Record_ } from "./client";

export {
  ApiError,
  idempotencyKey,
  request,
  type ListResponse,
  type Problem,
  type Record_,
} from "./client";

/** The metadata field types the kernel defines (kernel/metadata/schema.go).
 * The set is closed by design: plugins compose these, they don't invent new
 * ones, which is what makes a total renderer possible. */
export type FieldType =
  | "text"
  | "long_text"
  | "rich_text"
  | "int"
  | "decimal"
  | "money"
  | "currency"
  | "date"
  | "datetime"
  | "bool"
  | "enum"
  | "link"
  | "table"
  | "json"
  | "file"
  | "email"
  | "phone"
  | "address"
  | "duration"
  | "percent"
  | "computed";

export interface MetaField {
  name: string;
  type: FieldType;
  required: boolean;
  /** link/table: the object this field points at. */
  target?: string;
  /** The field's value may exist in several languages; the per-locale ones
   * travel in the record's `translations` key (docs/17). */
  localized?: boolean;
}

export interface MetaObject {
  name: string;
  /** The URL segment the gateway routes on — never re-derive it client-side. */
  resource: string;
  module: string;
  fields: MetaField[];
}

/** Fields the kernel owns on every CRUD record. They are never rendered as form
 * inputs (the server sets them) but do appear in list and detail views. */
export const SYSTEM_FIELDS = [
  "id",
  "tenant_id",
  "created_at",
  "updated_at",
  "archived_at",
  // Not a field: the reserved key carrying {field: {locale: value}} for
  // localized fields. It is never a form input of its own.
  "translations",
];

export interface SessionInfo {
  user_id: string;
  tenant: string;
  expires_at: string;
}

export interface LoginInput {
  tenant: string;
  email: string;
  password: string;
  totp?: string;
}

/** login starts a session. The tokens come back as HttpOnly cookies, so there
 * is nothing to store here — a successful call is the whole side effect. */
export function login(input: LoginInput): Promise<SessionInfo> {
  return request<SessionInfo>("/api/v1/sessions", { method: "POST", body: input });
}

export function logout(): Promise<void> {
  return request<void>("/api/v1/sessions/current", { method: "DELETE" });
}

/** listObjects returns the schemas this tenant can render. Objects whose module
 * is disabled are absent, so navigation built from this is always live. */
export async function listObjects(): Promise<MetaObject[]> {
  const { data } = await request<ListResponse<MetaObject>>("/api/v1/meta/objects");
  return data;
}

export async function listRecords(resource: string): Promise<Record_[]> {
  const { data } = await request<ListResponse<Record_>>(`/api/v1/${resource}`);
  return data;
}

export function getRecord(resource: string, id: string): Promise<Record_> {
  return request<Record_>(`/api/v1/${resource}/${id}`);
}

export function createRecord(resource: string, values: Record_, key?: string): Promise<Record_> {
  return request<Record_>(`/api/v1/${resource}`, {
    method: "POST",
    body: values,
    idempotencyKey: key,
  });
}

export function updateRecord(
  resource: string,
  id: string,
  values: Record_,
  key?: string,
): Promise<Record_> {
  return request<Record_>(`/api/v1/${resource}/${id}`, {
    method: "PATCH",
    body: values,
    idempotencyKey: key,
  });
}

export interface Capabilities {
  enabled: string[];
}

export function listCapabilities(): Promise<Capabilities> {
  return request<Capabilities>("/api/v1/capabilities");
}

// --- invoicing (bespoke lifecycle routes, not generic CRUD — INV-F2/F5/F6) ---

export interface Invoice extends Record_ {
  ID: string;
  Status: string;
  Number?: string;
  GLEntryID?: string;
  NetMinor?: number;
  TaxMinor?: number;
  GrossMinor?: number;
  Currency?: string;
}

export function getInvoice(id: string): Promise<Invoice> {
  return request<Invoice>(`/api/v1/invoices/${id}`);
}

/** postInvoice runs the posting pipeline: tax resolution, the declared GL
 * template, and gapless number allocation all happen server-side. The client
 * cannot set status or totals — that is the point (WP-1.4b-decisions.md §2). */
export function postInvoice(id: string, period: string, key?: string): Promise<Invoice> {
  return request<Invoice>(`/api/v1/invoices/${id}/post`, {
    method: "POST",
    body: { period },
    idempotencyKey: key,
  });
}

export function invoicePdfUrl(id: string): string {
  return `/api/v1/invoices/${id}/pdf`;
}

// --- dashboards (WP-1.8 role packs) ---

/** A KPI card as the server renders it: the number plus the comparison that
 * gives it meaning. Money is integer minor units and stays that way until the
 * very last formatting step (INV-F4). */
export interface Card {
  metric: string;
  label: string;
  unit: "money_minor" | "days" | "count";
  grain: "flow" | "stock";
  currency?: string;
  period?: string;
  value: number;
  good_direction: "up" | "down";
  comparison?: {
    basis: string;
    period: string;
    value: number;
    delta_minor: number;
    /** Hundredths of a percent; absent when the prior value was zero. */
    delta_basis_points?: number;
    /** Whether the move went the metric's good way; absent when unchanged. */
    improved?: boolean;
  };
}

export interface DashboardListing {
  name: string;
  title: string;
  roles: string[];
  requires?: string[];
  headline: string;
  tiles: string[];
  /** False when this viewer cannot evaluate the headline, or the pack needs a
   * module that is not installed. */
  available: boolean;
  /** True when the pack matches one of the viewer's own roles. */
  suggested: boolean;
}

export interface Dashboard {
  name: string;
  title: string;
  currency: string;
  period: string;
  as_of: string;
  headline?: Card;
  cards: Card[];
  /** Tiles this viewer may not see. Named rather than silently dropped. */
  omitted?: string[];
}

export async function listDashboards(): Promise<DashboardListing[]> {
  const { data } = await request<ListResponse<DashboardListing>>("/api/v1/dashboards");
  return data;
}

export function getDashboard(name: string, currency: string, period?: string): Promise<Dashboard> {
  const query = new URLSearchParams({ currency });
  if (period) {
    query.set("period", period);
  }
  return request<Dashboard>(`/api/v1/dashboards/${name}?${query.toString()}`);
}
