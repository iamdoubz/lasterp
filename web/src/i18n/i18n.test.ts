// SPDX-License-Identifier: AGPL-3.0-only
import { describe, expect, test } from "vitest";
import { pseudoLocalize } from "./pseudo";
import { formatMessage, formatMoney, formatNumber } from "./format";
import { resolveLocale, localeFromSearch } from "./i18n";

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

  test("localeFromSearch reads ?locale and defaults to en", () => {
    expect(localeFromSearch("?locale=pseudo").id).toBe("pseudo");
    expect(localeFromSearch("?locale=ar").id).toBe("ar");
    expect(localeFromSearch("").id).toBe("en");
    expect(localeFromSearch("?locale=bogus").id).toBe("en");
  });
});
