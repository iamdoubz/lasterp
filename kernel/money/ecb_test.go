package money

import (
	"strings"
	"testing"
)

const ecbFixture = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<gesmes:subject>Reference rates</gesmes:subject>
	<gesmes:Sender><gesmes:name>European Central Bank</gesmes:name></gesmes:Sender>
	<Cube>
		<Cube time="2026-07-17">
			<Cube currency="USD" rate="1.0873"/>
			<Cube currency="JPY" rate="170.50"/>
			<Cube currency="GBP" rate="0.8412"/>
		</Cube>
	</Cube>
</gesmes:Envelope>`

func TestParseECB(t *testing.T) {
	rates, err := ParseECB(strings.NewReader(ecbFixture))
	if err != nil {
		t.Fatalf("ParseECB: %v", err)
	}
	if len(rates) != 3 {
		t.Fatalf("got %d rates, want 3", len(rates))
	}
	byQuote := map[string]Rate{}
	for _, r := range rates {
		if r.Base != "EUR" || r.Provider != "ECB" {
			t.Fatalf("rate %+v: want base EUR provider ECB", r)
		}
		if got := r.AsOf.Format("2006-01-02"); got != "2026-07-17" {
			t.Fatalf("as_of = %s, want 2026-07-17", got)
		}
		byQuote[r.Quote] = r
	}
	if byQuote["USD"].Rate != "1.0873" {
		t.Fatalf("USD rate = %q, want 1.0873", byQuote["USD"].Rate)
	}
}

func TestParseECBSkipsBadRows(t *testing.T) {
	xml := strings.Replace(ecbFixture,
		`<Cube currency="GBP" rate="0.8412"/>`,
		`<Cube currency="GBP" rate="0.8412"/>
			<Cube currency="ZZZ" rate="1.0"/>
			<Cube currency="CHF" rate="not-a-number"/>`, 1)
	rates, err := ParseECB(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("ParseECB: %v", err)
	}
	// ZZZ (unknown) and CHF (bad rate) are skipped; the 3 good rows remain.
	if len(rates) != 3 {
		t.Fatalf("got %d rates, want 3 (bad rows skipped)", len(rates))
	}
}

// FuzzParseECB — CLAUDE.md: parsers may reject malformed input, never panic or
// corrupt. Seeded with the good fixture and some garbage.
func FuzzParseECB(f *testing.F) {
	f.Add(ecbFixture)
	f.Add("")
	f.Add("<not xml")
	f.Add(`<Cube><Cube time="bad"><Cube currency="USD" rate="x"/></Cube></Cube>`)
	f.Fuzz(func(t *testing.T, data string) {
		// Only assertion: it returns without panicking. Any rate it does return
		// must have a known currency (skipped otherwise).
		rates, err := ParseECB(strings.NewReader(data))
		if err != nil {
			return
		}
		for _, r := range rates {
			if _, err := Lookup(r.Quote); err != nil {
				t.Fatalf("ParseECB returned unknown currency %q", r.Quote)
			}
		}
	})
}
