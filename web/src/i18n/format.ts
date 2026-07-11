// SPDX-License-Identifier: AGPL-3.0-only

// Minimal ICU MessageFormat subset — no dependency, backed by native Intl.
// Supported:
//   {arg}                                  simple interpolation
//   {arg, plural, one {…} other {…}}       Intl.PluralRules; # → the count
//   {arg, select, male {…} other {…}}      exact-match select
// Nested placeholders inside sub-messages are resolved recursively.

export type MessageValues = Record<string, string | number>;

// formatMessage renders an ICU-subset pattern for a locale with values.
export function formatMessage(
  pattern: string,
  values: MessageValues,
  locale: string,
): string {
  const [out] = parse(pattern, values, locale, 0, false, 0);
  return out;
}

// parse walks the pattern from index i, resolving placeholders until it hits an
// unbalanced '}' (when insideArg) or the end. count is the active plural/select
// operand used to substitute '#'. Returns the rendered text and the index just
// past where it stopped.
function parse(
  s: string,
  values: MessageValues,
  locale: string,
  i: number,
  insideArg: boolean,
  count: number,
): [string, number] {
  let out = "";
  while (i < s.length) {
    const ch = s[i];
    if (ch === "}" && insideArg) {
      return [out, i];
    }
    if (ch === "#" && insideArg) {
      out += new Intl.NumberFormat(locale).format(count);
      i++;
      continue;
    }
    if (ch === "{") {
      const [rendered, next] = parseArg(s, values, locale, i + 1);
      out += rendered;
      i = next;
      continue;
    }
    out += ch;
    i++;
  }
  return [out, i];
}

// parseArg handles one {...} placeholder starting just after the '{'.
function parseArg(
  s: string,
  values: MessageValues,
  locale: string,
  i: number,
): [string, number] {
  // Read the argument name.
  let name = "";
  while (i < s.length && s[i] !== "," && s[i] !== "}") {
    name += s[i];
    i++;
  }
  name = name.trim();

  // Simple interpolation: {name}
  if (s[i] === "}") {
    return [String(values[name] ?? ""), i + 1];
  }

  // Read the format type (plural | select).
  i++; // skip ','
  let type = "";
  while (i < s.length && s[i] !== "," && s[i] !== "}") {
    type += s[i];
    i++;
  }
  type = type.trim();
  i++; // skip ',' before options

  const num = Number(values[name] ?? 0);
  const options = new Map<string, string>();
  while (i < s.length && s[i] !== "}") {
    if (s[i] === " ") {
      i++;
      continue;
    }
    // Read a selector keyword.
    let key = "";
    while (i < s.length && s[i] !== "{" && s[i] !== " ") {
      key += s[i];
      i++;
    }
    while (i < s.length && s[i] === " ") i++;
    // Read its {sub-message}, recursively rendering nested placeholders.
    const [sub, next] = parse(s, values, locale, i + 1, true, num);
    options.set(key.trim(), sub);
    i = next + 1; // skip closing '}'
  }
  i++; // skip closing '}' of the arg

  let chosen: string | undefined;
  if (type === "plural") {
    const cat = new Intl.PluralRules(locale).select(num);
    chosen = options.get(`=${num}`) ?? options.get(cat) ?? options.get("other");
  } else {
    chosen = options.get(String(values[name])) ?? options.get("other");
  }
  return [chosen ?? "", i];
}

// number/date/currency helpers — native Intl, canonical inputs localize at the
// edge (money is integer minor units + ISO-4217, dates are UTC epoch millis).
export function formatNumber(x: number, locale: string): string {
  return new Intl.NumberFormat(locale).format(x);
}

export function formatMoney(
  minorUnits: number,
  currency: string,
  locale: string,
): string {
  const digits = fractionDigits(currency, locale);
  return new Intl.NumberFormat(locale, { style: "currency", currency }).format(
    minorUnits / 10 ** digits,
  );
}

export function formatDate(epochMillis: number, locale: string): string {
  return new Intl.DateTimeFormat(locale).format(new Date(epochMillis));
}

function fractionDigits(currency: string, locale: string): number {
  return (
    new Intl.NumberFormat(locale, { style: "currency", currency }).resolvedOptions()
      .maximumFractionDigits ?? 2
  );
}
