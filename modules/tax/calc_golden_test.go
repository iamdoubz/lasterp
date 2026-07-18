//go:build integrity

// Golden-file tax suite (docs/19: "Golden files: tax … calculations vs
// certified expected outputs"). Each testdata/golden/*.json is a certified
// scenario — inputs plus the exact expected per-line and per-group net/tax. The
// calculation is a money path, so this proves INV-F4 (integer minor units,
// no floats, cents conserved) against fixed oracles. Pure (no DB); rate
// resolution and tenant isolation are proved separately in the integration
// suite.
package tax

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/money"
)

type goldenLine struct {
	Jurisdiction string `json:"jurisdiction"`
	Category     string `json:"category"`
	Base         struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	} `json:"base"`
	Rate      string `json:"rate"`
	Rounding  string `json:"rounding"`
	Inclusive bool   `json:"inclusive"`
}

type goldenLineResult struct {
	Net   int64 `json:"net"`
	Tax   int64 `json:"tax"`
	Gross int64 `json:"gross"`
}

type goldenTotal struct {
	Jurisdiction string `json:"jurisdiction"`
	Category     string `json:"category"`
	Net          int64  `json:"net"`
	Tax          int64  `json:"tax"`
}

type goldenCase struct {
	Name         string             `json:"name"`
	Lines        []goldenLine       `json:"lines"`
	ExpectLines  []goldenLineResult `json:"expect_lines"`
	ExpectTotals []goldenTotal      `json:"expect_totals"`
}

func TestTaxGoldenFiles(t *testing.T) {
	files, err := filepath.Glob("testdata/golden/*.json")
	if err != nil {
		t.Fatalf("glob golden: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no golden files found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			var gc goldenCase
			if err := json.Unmarshal(data, &gc); err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}

			doc := Document{Lines: make([]Line, len(gc.Lines))}
			for i, gl := range gc.Lines {
				base, err := money.New(gl.Base.Amount, gl.Base.Currency)
				if err != nil {
					t.Fatalf("line %d base: %v", i, err)
				}
				rate, ok := new(big.Rat).SetString(gl.Rate)
				if !ok {
					t.Fatalf("line %d bad rate %q", i, gl.Rate)
				}
				mode, err := roundingMode(gl.Rounding)
				if err != nil {
					t.Fatalf("line %d rounding: %v", i, err)
				}
				doc.Lines[i] = Line{
					Jurisdiction: gl.Jurisdiction, Category: gl.Category,
					Base: base, Rate: rate, Rounding: mode, Inclusive: gl.Inclusive,
				}
			}

			res, err := Calculate(doc)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}

			if len(res.Lines) != len(gc.ExpectLines) {
				t.Fatalf("got %d line results, want %d", len(res.Lines), len(gc.ExpectLines))
			}
			for i, want := range gc.ExpectLines {
				got := res.Lines[i]
				if got.Net.Amount() != want.Net || got.Tax.Amount() != want.Tax || got.Gross.Amount() != want.Gross {
					t.Errorf("line %d: got net=%d tax=%d gross=%d, want net=%d tax=%d gross=%d",
						i, got.Net.Amount(), got.Tax.Amount(), got.Gross.Amount(), want.Net, want.Tax, want.Gross)
				}
				// INV-F4: the cent is conserved on every line.
				if got.Net.Amount()+got.Tax.Amount() != got.Gross.Amount() {
					t.Errorf("line %d: net+tax != gross (%d+%d != %d)", i, got.Net.Amount(), got.Tax.Amount(), got.Gross.Amount())
				}
			}

			if len(res.Totals) != len(gc.ExpectTotals) {
				t.Fatalf("got %d totals, want %d", len(res.Totals), len(gc.ExpectTotals))
			}
			for i, want := range gc.ExpectTotals {
				got := res.Totals[i]
				if got.Jurisdiction != want.Jurisdiction || got.Category != want.Category ||
					got.Net.Amount() != want.Net || got.Tax.Amount() != want.Tax {
					t.Errorf("total %d: got %s/%s net=%d tax=%d, want %s/%s net=%d tax=%d",
						i, got.Jurisdiction, got.Category, got.Net.Amount(), got.Tax.Amount(),
						want.Jurisdiction, want.Category, want.Net, want.Tax)
				}
			}
		})
	}
}
