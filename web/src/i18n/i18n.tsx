// SPDX-License-Identifier: AGPL-3.0-only

import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  type ReactNode,
} from "react";
import { messages, type Catalog, type MessageKey } from "./messages";
import { pseudoLocalize } from "./pseudo";
import {
  formatMessage,
  formatMoney,
  formatNumber,
  type MessageValues,
} from "./format";

// A Locale bundles the resolved BCP-47 tag for Intl, its message catalog, and
// its writing direction. "pseudo" (en-XA) is accented AND right-to-left so a
// single build exercises both the accent and RTL/logical-CSS paths (docs/17).
export type LocaleId = "en" | "pseudo" | "ar";

export interface Locale {
  id: LocaleId;
  tag: string; // BCP-47 tag passed to Intl
  dir: "ltr" | "rtl";
  catalog: Catalog;
}

const RTL = new Set<LocaleId>(["pseudo", "ar"]);

function pseudoCatalog(): Catalog {
  const out = {} as Catalog;
  for (const key of Object.keys(messages) as MessageKey[]) {
    out[key] = pseudoLocalize(messages[key]);
  }
  return out;
}

export function resolveLocale(id: LocaleId): Locale {
  const tag = id === "pseudo" ? "en" : id;
  const catalog = id === "pseudo" ? pseudoCatalog() : (messages as Catalog);
  return { id, tag, dir: RTL.has(id) ? "rtl" : "ltr", catalog };
}

// localeFromSearch reads ?locale=… (defaults to "en"), so the pseudo/RTL build
// is reachable without a rebuild — the AC render check drives it this way.
export function localeFromSearch(search: string): Locale {
  const id = new URLSearchParams(search).get("locale");
  if (id === "pseudo" || id === "ar" || id === "en") {
    return resolveLocale(id);
  }
  return resolveLocale("en");
}

export interface Translator {
  locale: Locale;
  t: (key: MessageKey, values?: MessageValues) => string;
  formatNumber: (x: number) => string;
  formatMoney: (minorUnits: number, currency: string) => string;
}

const I18nContext = createContext<Translator | null>(null);

export function I18nProvider({
  locale,
  children,
}: {
  locale: Locale;
  children: ReactNode;
}) {
  // Reflect direction/language on the document root so CSS logical properties
  // and the RTL layout react — one dir attribute, no hardcoded left/right.
  useEffect(() => {
    document.documentElement.dir = locale.dir;
    document.documentElement.lang = locale.tag;
  }, [locale]);

  const value = useMemo<Translator>(
    () => ({
      locale,
      t: (key, values = {}) =>
        formatMessage(locale.catalog[key], values, locale.tag),
      formatNumber: (x) => formatNumber(x, locale.tag),
      formatMoney: (minorUnits, currency) =>
        formatMoney(minorUnits, currency, locale.tag),
    }),
    [locale],
  );

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): Translator {
  const ctx = useContext(I18nContext);
  if (!ctx) {
    throw new Error("useI18n must be used within an I18nProvider");
  }
  return ctx;
}

// useT is the common path: const t = useT(); t("app.title").
export function useT(): Translator["t"] {
  return useI18n().t;
}
