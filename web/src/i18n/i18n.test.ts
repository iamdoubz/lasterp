// SPDX-License-Identifier: AGPL-3.0-only
import { describe, expect, test } from "vitest";
import { pseudoLocalize } from "./pseudo";
import { formatMessage, formatMoney, formatNumber } from "./format";
import { initialLocale, resolveLocale } from "./i18n";
import { messages } from "./messages";
import de from "./packs/de.json";

/** The on-disk pack shape, mirrored from kernel/i18n's Pack. */
interface Pack {
  locale: string;
  version: number;
  messages: Record<string, string>;
}

describe("pseudoLocalize", () => {
  test.each([
    ["Post", "⟦Þóóşţ⟧"],
    ["Hi", "⟦Ĥíí⟧"],
  ])("accents %s", (input, want) => {
    expect(pseudoLocalize(input)).toBe(want);
  });

  test("preserves placeholders and printf verbs", () => {
    expect(pseudoLocalize("Hi {name}")).toBe("⟦Ĥíí {name}⟧");
    expect(pseudoLocalize("Total %s")).toBe("⟦Ţóóţààļ %s⟧");
  });

  test("expands length to surface truncation", () => {
    expect(pseudoLocalize("aeiou").length).toBeGreaterThan("aeiou".length);
  });
});

describe("formatMessage", () => {
  test("interpolates arguments", () => {
    expect(formatMessage("Hi {name}", { name: "Ada" }, "en")).toBe("Hi Ada");
  });

  test("selects plural category and substitutes #", () => {
    const p = "{count, plural, one {# item} other {# items}}";
    expect(formatMessage(p, { count: 1 }, "en")).toBe("1 item");
    expect(formatMessage(p, { count: 5 }, "en")).toBe("5 items");
    expect(formatMessage(p, { count: 0 }, "en")).toBe("0 items");
  });

  test("supports exact-match =n over category", () => {
    const p = "{count, plural, =0 {none} one {# item} other {# items}}";
    expect(formatMessage(p, { count: 0 }, "en")).toBe("none");
  });

  test("select matches on value", () => {
    const p = "{g, select, male {he} female {she} other {they}}";
    expect(formatMessage(p, { g: "female" }, "en")).toBe("she");
    expect(formatMessage(p, { g: "x" }, "en")).toBe("they");
  });
});

describe("Intl formatting helpers", () => {
  test("number grouping is locale-aware", () => {
    expect(formatNumber(1234567.89, "en-US")).toBe("1,234,567.89");
    expect(formatNumber(1234567.89, "de-DE")).toBe("1.234.567,89");
  });

  test("money renders integer minor units with fraction digits", () => {
    expect(formatMoney(123456, "USD", "en-US")).toBe("$1,234.56");
    expect(formatMoney(1000, "JPY", "en-US")).toBe("¥1,000"); // zero-decimal
  });
});

describe("locale resolution", () => {
  test("pseudo is accented and RTL", () => {
    const l = resolveLocale("pseudo");
    expect(l.dir).toBe("rtl");
    expect(l.tag).toBe("en"); // Intl still uses a real tag
    expect(l.catalog["app.title"]).toMatch(/^⟦.*⟧$/);
  });

  test("english is ltr and unwrapped", () => {
    expect(resolveLocale("en").dir).toBe("ltr");
    expect(resolveLocale("en").catalog["app.title"]).toBe("LastERP");
  });

  test("?locale reads the pseudo and RTL builds, and defaults to en", () => {
    expect(initialLocale("?locale=pseudo").id).toBe("pseudo");
    expect(initialLocale("?locale=ar").id).toBe("ar");
    expect(initialLocale("").id).toBe("en");
    expect(initialLocale("?locale=bogus").id).toBe("en");
  });
});

// --- translation packs (WP-1.7) ---

describe("translation packs", () => {
  // The gate that makes a shipped locale mean something: a pack must answer
  // every key the UI can ask for, and may not invent keys nothing reads. A
  // future WP adding a string without translating it fails here instead of
  // rendering half an English screen (docs/notes/WP-1.7-decisions.md §2).
  const packs: Array<[string, Pack]> = [["de", de as Pack]];

  test.each(packs)("the %s pack covers exactly the source catalog", (_id, pack) => {
    const sourceKeys = Object.keys(messages).sort();
    const packKeys = Object.keys(pack.messages).sort();
    expect(packKeys).toEqual(sourceKeys);
  });

  test.each(packs)("the %s pack is versioned and non-empty", (_id, pack) => {
    expect(pack.version).toBeGreaterThanOrEqual(1);
    for (const [key, value] of Object.entries(pack.messages)) {
      expect(value, `${key} is empty`).not.toBe("");
    }
  });

  test.each(packs)("the %s pack keeps ICU placeholders intact", (_id, pack) => {
    for (const [key, source] of Object.entries(messages)) {
      const placeholders = (s: string) => (s.match(/\{(\w+)/g) ?? []).sort();
      expect(placeholders(pack.messages[key]), `${key} placeholders`).toEqual(
        placeholders(source),
      );
    }
  });

  test("a locale with a pack renders it; one without falls back to English", () => {
    expect(resolveLocale("de").catalog["nav.signOut"]).toBe("Abmelden");
    expect(resolveLocale("de").dir).toBe("ltr");
    expect(resolveLocale("ar").catalog["nav.signOut"]).toBe("Sign out");
    expect(resolveLocale("ar").dir).toBe("rtl");
  });
});

describe("initialLocale", () => {
  test.each([
    ["an explicit query wins", "?locale=de", "en", ["en-US"], "de"],
    ["then the remembered choice", "", "de", ["en-US"], "de"],
    ["then the browser's language", "", null, ["de-AT", "en"], "de"],
    ["a regional browser tag resolves to its language", "", null, ["de-CH"], "de"],
    ["nothing recognised is English", "", null, ["fr-FR"], "en"],
    ["a bogus stored value is ignored", "", "klingon", ["fr"], "en"],
    ["a bogus query falls through", "?locale=nope", "de", [], "de"],
  ])("%s", (_name, search, stored, languages, want) => {
    expect(initialLocale(search, stored, languages).id).toBe(want);
  });
});
