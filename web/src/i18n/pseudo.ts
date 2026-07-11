// SPDX-License-Identifier: AGPL-3.0-only

// Pseudo-localization (Chrome/Android en-XA convention): accents Latin letters
// (ÀççéñţéÐ), wraps in ⟦ … ⟧, and expands length ~40% to surface truncation.
// Printf verbs (%s) and ICU/brace placeholders ({name}) are preserved so
// formatting still resolves. Paired with dir="rtl" it exercises the accent and
// RTL paths in a single build (docs/17 AC: "ÀççéñţéÐ + RTL").

const ACCENTS: Record<string, string> = {
  a: "à", b: "ƀ", c: "ç", d: "ð", e: "é", f: "ƒ", g: "ĝ", h: "ĥ", i: "í",
  j: "ĵ", k: "ķ", l: "ļ", m: "ɱ", n: "ñ", o: "ó", p: "þ", q: "ǫ", r: "ŕ",
  s: "ş", t: "ţ", u: "ú", v: "ṽ", w: "ŵ", x: "ẋ", y: "ý", z: "ž",
  A: "À", B: "Ɓ", C: "Ç", D: "Ð", E: "É", F: "Ƒ", G: "Ĝ", H: "Ĥ", I: "Í",
  J: "Ĵ", K: "Ķ", L: "Ļ", M: "Ṁ", N: "Ñ", O: "Ó", P: "Þ", Q: "Ǫ", R: "Ŕ",
  S: "Ş", T: "Ţ", U: "Ú", V: "Ṽ", W: "Ŵ", X: "Ẋ", Y: "Ý", Z: "Ž",
};

const VOWELS = new Set("aeiouAEIOU");

export function pseudoLocalize(s: string): string {
  let out = "⟦";
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "{") {
      // Copy an ICU/brace placeholder verbatim.
      out += ch;
      while (i + 1 < s.length && s[i] !== "}") {
        i++;
        out += s[i];
      }
      continue;
    }
    if (ch === "%") {
      // Copy a printf verb verbatim (up to the terminating letter or %%).
      out += ch;
      while (i + 1 < s.length) {
        i++;
        out += s[i];
        if (/[a-zA-Z%]/.test(s[i])) break;
      }
      continue;
    }
    const a = ACCENTS[ch];
    if (a) {
      out += a;
      if (VOWELS.has(ch)) out += a; // expand length
    } else {
      out += ch;
    }
  }
  return out + "⟧";
}
