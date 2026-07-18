// SPDX-License-Identifier: AGPL-3.0-only

package money

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

// ecbDailyURL is the European Central Bank's daily EUR-reference-rate feed.
const ecbDailyURL = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml"

// Provider is an FX rate source (ADR-013 FxRateProvider). Implementations
// return rates to feed into SaveRate; the kernel never hard-depends on a paid
// vendor.
type Provider interface {
	Rates(ctx context.Context) ([]Rate, error)
}

// ecbEnvelope models the ECB eurofxref XML. Namespaced element names are
// matched by local name, so the gesmes:/eurofxref: prefixes are ignored.
type ecbEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Days    []struct {
		Time  string `xml:"time,attr"`
		Rates []struct {
			Currency string `xml:"currency,attr"`
			Rate     string `xml:"rate,attr"`
		} `xml:"Cube"`
	} `xml:"Cube>Cube"`
}

// ParseECB reads the ECB daily XML and returns EUR-based rates. It is defensive
// against malformed input (CLAUDE.md: parsers may reject, never corrupt) — a
// row with an unknown currency or an unparseable rate is skipped, and only
// structurally broken XML or an unparseable date returns an error. It never
// panics (fuzzed).
func ParseECB(r io.Reader) ([]Rate, error) {
	var env ecbEnvelope
	if err := xml.NewDecoder(r).Decode(&env); err != nil {
		return nil, fmt.Errorf("money: parse ECB xml: %w", err)
	}
	var out []Rate
	for _, day := range env.Days {
		asOf, err := time.Parse(dateLayout, day.Time)
		if err != nil {
			return nil, fmt.Errorf("money: bad ECB date %q: %w", day.Time, err)
		}
		for _, rt := range day.Rates {
			c, err := Lookup(rt.Currency)
			if err != nil {
				continue // ECB code x/text doesn't know — skip, don't fail the feed
			}
			if _, ok := new(big.Rat).SetString(rt.Rate); !ok {
				continue // malformed rate — skip the row
			}
			out = append(out, Rate{Base: "EUR", Quote: c.Code, Rate: rt.Rate, AsOf: asOf.UTC(), Provider: "ECB"})
		}
	}
	return out, nil
}

// ECBProvider fetches rates from the ECB daily feed. URL and Client are
// optional (they default to the public feed and http.DefaultClient). The
// network fetch is not exercised in CI; ParseECB carries the tested logic.
type ECBProvider struct {
	URL    string
	Client *http.Client
}

// Rates fetches and parses the ECB daily feed.
func (p ECBProvider) Rates(ctx context.Context) ([]Rate, error) {
	url := p.URL
	if url == "" {
		url = ecbDailyURL
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("money: build ECB request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("money: fetch ECB rates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("money: ECB feed returned %s", resp.Status)
	}
	return ParseECB(resp.Body)
}
