// SPDX-License-Identifier: AGPL-3.0-only

package tax

import (
	"context"
	"embed"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

//go:embed seed/*.yaml
var seedFS embed.FS

// seedPack is the on-disk shape of a seed YAML file (US sales, EU VAT, …).
type seedPack struct {
	Country       string `yaml:"country"`
	Jurisdictions []struct {
		Code  string `yaml:"code"`
		Name  string `yaml:"name"`
		Level string `yaml:"level"`
	} `yaml:"jurisdictions"`
	Rates []struct {
		Jurisdiction string `yaml:"jurisdiction"`
		Category     string `yaml:"category"`
		Rate         string `yaml:"rate"`
		Rounding     string `yaml:"rounding"`
		AsOf         string `yaml:"as_of"`
		Name         string `yaml:"name"`
	} `yaml:"rates"`
}

// LoadSeedPacks writes the embedded community seed packs (US state sales tax, EU
// VAT) under GlobalTenant, so every tenant can look them up (and override per
// tenant). Idempotency is the caller's concern — the packs are effective-dated
// data, so re-running appends duplicate rows; call once at bootstrap. ADR-013:
// packs carry disclaimers and are not legal advice.
func LoadSeedPacks(ctx context.Context, db *storage.DB) error {
	entries, err := seedFS.ReadDir("seed")
	if err != nil {
		return err
	}
	for _, e := range entries {
		data, err := seedFS.ReadFile("seed/" + e.Name())
		if err != nil {
			return err
		}
		var pack seedPack
		if err := yaml.Unmarshal(data, &pack); err != nil {
			return fmt.Errorf("tax: parse seed %s: %w", e.Name(), err)
		}
		for _, j := range pack.Jurisdictions {
			if err := SaveJurisdiction(ctx, db, GlobalTenant, Jurisdiction{
				Code: j.Code, Name: j.Name, Country: pack.Country, Level: j.Level,
			}); err != nil {
				return fmt.Errorf("tax: seed jurisdiction %s: %w", j.Code, err)
			}
		}
		for _, r := range pack.Rates {
			asOf, err := time.Parse(dateLayout, r.AsOf)
			if err != nil {
				return fmt.Errorf("tax: seed rate %s/%s bad as_of %q: %w", r.Jurisdiction, r.Category, r.AsOf, err)
			}
			if err := SaveRate(ctx, db, GlobalTenant, Rate{
				Jurisdiction: r.Jurisdiction, Category: r.Category, Rate: r.Rate,
				Rounding: r.Rounding, AsOf: asOf, Name: r.Name, Provider: "seed:" + pack.Country,
			}); err != nil {
				return fmt.Errorf("tax: seed rate %s/%s: %w", r.Jurisdiction, r.Category, err)
			}
		}
	}
	return nil
}
