// SPDX-License-Identifier: AGPL-3.0-only

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { messages, type Catalog, type MessageKey } from "./messages";
import dePack from "./packs/de.json";
import { pseudoLocalize } from "./pseudo";
import {
  formatMessage,
  formatMoney,
  formatNumber,
  formatPercent,
  type MessageValues,
} from "./format";

// A Locale bundles the resolved BCP-47 tag for Intl, its message catalog, and
// its writing direction. "pseudo" (en-XA) is accented AND right-to-left so a
// single build exercises both the accent and RTL/logical-CSS paths (docs/17).
export type LocaleId = "en" | "de" | "pseudo" | "ar";

export interface Locale {
  id: LocaleId;
  tag: string; // BCP-47 tag passed to Intl
  dir: "ltr" | "rtl";
  catalog: Catalog;
}

const RTL = new Set<LocaleId>(["pseudo", "ar"]);

/** LOCALE_IDS is the switcher's option list. Names are endonyms — a German
 * speaker looks for "Deutsch", not "German", whatever language the UI is
 * currently in — so they are deliberately not catalog entries. */
export const LOCALE_NAMES: Record<LocaleId, string> = {
  en: "English",
  de: "Deutsch",
  ar: "العربية",
  pseudo: "Pseudo (ÀççéñţéÐ)",
};

/** A translation pack as it ships: versioned data, same shape the server's
 * document packs use (kernel/i18n/packs). */
interface Pack {
  locale: string;
  version: number;
  messages: Record<string, string>;
}

/** PACKS are the non-English catalogs compiled into the bundle. They are
 * imported rather than fetched so the *login* screen is already localized —
 * fetching a catalog would need an unauthenticated route, and offline-first
 * (commandment 4) means the client must not need the network to render.
 * Runtime pack installation lands with the plugin registry (WP-3.2). */
const PACKS: Partial<Record<LocaleId, Pack>> = { de: dePack as Pack };

const STORAGE_KEY = "lasterp.locale";

function packCatalog(pack: Pack): Catalog {
  // English underneath: a pack missing a key degrades to the source string
  // instead of rendering "undefined". The completeness test (i18n.test.ts)
  // keeps that from becoming the normal case.
  return { ...(messages as Catalog), ...(pack.messages as Partial<Catalog>) };
}

function pseudoCatalog(): Catalog {
  const out = {} as Catalog;
  for (const key of Object.keys(messages) as MessageKey[]) {
    out[key] = pseudoLocalize(messages[key]);
  }
  return out;
}

export function resolveLocale(id: LocaleId): Locale {
  const tag = id === "pseudo" ? "en" : id;
  const pack = PACKS[id];
  let catalog: Catalog;
  if (id === "pseudo") {
    catalog = pseudoCatalog();
  } else if (pack) {
    catalog = packCatalog(pack);
  } else {
    catalog = messages as Catalog;
  }
  return { id, tag, dir: RTL.has(id) ? "rtl" : "ltr", catalog };
}

export function isLocaleId(value: string | null): value is LocaleId {
  return value === "en" || value === "de" || value === "pseudo" || value === "ar";
}

/**
 * initialLocale answers "what language is this user reading in", most
 * deliberate answer first:
 *
 *   ?locale=      — an explicit request, and what the e2e and pseudo builds use
 *   localStorage  — the last choice made in the switcher, so it survives reloads
 *   navigator     — the browser's languages, so a German user gets German
 *                   without touching anything
 *   en            — the source locale
 */
export function initialLocale(
  search = "",
  stored: string | null = null,
  browserLanguages: readonly string[] = [],
): Locale {
  const requested = new URLSearchParams(search).get("locale");
  if (isLocaleId(requested)) {
    return resolveLocale(requested);
  }
  if (isLocaleId(stored)) {
    return resolveLocale(stored);
  }
  for (const language of browserLanguages) {
    // "de-AT" is served by the "de" pack: a regional variant with no pack of
    // its own is still that language.
    const base = language.split("-")[0];
    if (isLocaleId(base)) {
      return resolveLocale(base);
    }
  }
  return resolveLocale("en");
}

/** rememberLocale persists an explicit choice. Storage can be disabled (private
 * mode, blocked cookies); not being able to remember a choice is no reason to
 * refuse it. */
function rememberLocale(id: LocaleId) {
  try {
    window.localStorage.setItem(STORAGE_KEY, id);
  } catch {
    // Ignored on purpose — see above.
  }
}

/** localeFromEnvironment applies initialLocale to the live browser.
 *
 * A locale named in the URL is remembered, so a shared ?locale=de link stays
 * German across a reload instead of being a one-page effect. A locale merely
 * inferred from the browser is not: that is a default to re-derive every visit,
 * not a decision the user made. */
function localeFromEnvironment(): Locale {
  if (typeof window === "undefined") {
    return resolveLocale("en");
  }
  let stored: string | null = null;
  try {
    stored = window.localStorage.getItem(STORAGE_KEY);
  } catch {
    // A missing preference is not an error — the chain below still resolves.
  }
  const locale = initialLocale(window.location.search, stored, window.navigator.languages ?? []);
  const requested = new URLSearchParams(window.location.search).get("locale");
  if (isLocaleId(requested)) {
    rememberLocale(requested);
  }
  return locale;
}

export interface Translator {
  locale: Locale;
  setLocale: (id: LocaleId) => void;
  t: (key: MessageKey, values?: MessageValues) => string;
  /** label looks up a key the catalog may not contain — schema-derived object
   * and field names, whose keys depend on which modules a tenant runs — and
   * falls back rather than rendering the key. */
  label: (key: string, fallback: string) => string;
  formatNumber: (x: number) => string;
  formatMoney: (minorUnits: number, currency: string) => string;
  /** Renders a relative change carried in basis points (see format.ts). */
  formatPercent: (basisPoints: number) => string;
}

const I18nContext = createContext<Translator | null>(null);

export function I18nProvider({
  locale: initial,
  children,
}: {
  /** Fixes the locale (tests, and the pseudo-locale build). Omitted in the app,
   * where the resolution chain decides and the switcher can change it. */
  locale?: Locale;
  children: ReactNode;
}) {
  const [locale, setLocaleState] = useState<Locale>(() => initial ?? localeFromEnvironment());

  // Reflect direction/language on the document root so CSS logical properties
  // and the RTL layout react — one dir attribute, no hardcoded left/right.
  useEffect(() => {
    document.documentElement.dir = locale.dir;
    document.documentElement.lang = locale.tag;
  }, [locale]);

  const setLocale = useCallback((id: LocaleId) => {
    setLocaleState(resolveLocale(id));
    rememberLocale(id);
  }, []);

  const value = useMemo<Translator>(
    () => ({
      locale,
      setLocale,
      t: (key, values = {}) =>
        formatMessage(locale.catalog[key], values, locale.tag),
      label: (key, fallback) => {
        const pattern = (locale.catalog as Record<string, string | undefined>)[key];
        return pattern ? formatMessage(pattern, {}, locale.tag) : fallback;
      },
      formatNumber: (x) => formatNumber(x, locale.tag),
      formatMoney: (minorUnits, currency) =>
        formatMoney(minorUnits, currency, locale.tag),
      formatPercent: (basisPoints) => formatPercent(basisPoints, locale.tag),
    }),
    [locale, setLocale],
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
