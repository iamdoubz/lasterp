// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Role dashboard packs (docs/21 §4): "shipped as customization packages —
// instantly usable... New user logs in → their role's dashboard is simply
// there."
//
// A pack is embedded data, like a capability manifest or a translation pack. v1
// ships the pristine originals and renders them; editable copies, the drag-drop
// grid and chart intelligence are the builder's (WP-4.13). Nothing here writes,
// so there is nothing yet to diverge from the originals.
//
//go:embed packs/*.yaml
var packFS embed.FS

// ErrUnknownDashboard is returned for a pack name that does not exist.
var ErrUnknownDashboard = errors.New("reporting: unknown dashboard")

// Dashboard is a role pack definition.
type Dashboard struct {
	Name  string `yaml:"dashboard" json:"name"`
	Title string `yaml:"title" json:"title"`
	// Roles are *suggested* role names. They decide which dashboard opens by
	// default, never what a viewer may see — tenant role names are arbitrary
	// strings, so a pack cannot depend on one existing (decisions §3).
	Roles []string `yaml:"roles" json:"roles"`
	// Requires are capability names the pack's metrics come from. A pack whose
	// capabilities are not enabled stays out of the catalog rather than
	// rendering empty tiles (decisions §4).
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"`
	// Headline is the dominant top-left tile (docs/21 §3).
	Headline string   `yaml:"headline" json:"headline"`
	Tiles    []string `yaml:"tiles" json:"tiles"`
}

// metricNames lists every metric the pack renders, headline first.
func (d Dashboard) metricNames() []string {
	return append([]string{d.Headline}, d.Tiles...)
}

var dashboards = map[string]Dashboard{}

func init() {
	entries, err := packFS.ReadDir("packs")
	if err != nil {
		panic("reporting: read dashboard packs: " + err.Error())
	}
	for _, e := range entries {
		raw, err := packFS.ReadFile("packs/" + e.Name())
		if err != nil {
			panic("reporting: read dashboard pack " + e.Name() + ": " + err.Error())
		}
		var d Dashboard
		if err := yaml.Unmarshal(raw, &d); err != nil {
			panic("reporting: parse dashboard pack " + e.Name() + ": " + err.Error())
		}
		if d.Name == "" || d.Headline == "" {
			panic("reporting: dashboard pack " + e.Name() + " needs a name and a headline")
		}
		if _, dup := dashboards[d.Name]; dup {
			panic("reporting: duplicate dashboard pack " + d.Name)
		}
		dashboards[d.Name] = d
	}
}

// Dashboards lists the shipped packs, sorted by name.
func Dashboards() []Dashboard {
	out := make([]Dashboard, 0, len(dashboards))
	for _, d := range dashboards {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Listing is a pack as the catalog presents it: what it is, whether this viewer
// can actually render it, and whether it matches one of their roles.
type Listing struct {
	Dashboard
	// Available is false when the viewer may not evaluate the headline metric,
	// or when the pack needs a metric this build does not have (its capability
	// is not installed). An unavailable pack is not rendered with zeros — that
	// would be a permission leak wearing a number (INV-T1).
	Available bool `json:"available"`
	// Suggested marks the pack matching one of the viewer's own roles, which is
	// what "their role's dashboard is simply there" means in practice.
	Suggested bool `json:"suggested"`
}

// Rendered is an evaluated dashboard.
type Rendered struct {
	Name     string    `json:"name"`
	Title    string    `json:"title"`
	Currency string    `json:"currency"`
	Period   string    `json:"period"`
	AsOf     time.Time `json:"as_of"`
	Headline *Card     `json:"headline,omitempty"`
	Cards    []Card    `json:"cards"`
	// Omitted names the tiles this viewer may not see. Naming them is
	// deliberate: "there is a number here you cannot see" is honest, while
	// silently shrinking the dashboard makes a restricted view look complete.
	Omitted []string `json:"omitted,omitempty"`
}

// List returns the catalog for one viewer: every shipped pack, marked with
// whether they can render it and whether it is theirs.
//
// The catalog itself is not filtered — knowing that a CFO dashboard exists is
// not knowing what is on it, and hiding the name would make a permission
// problem look like a missing feature.
func List(ctx context.Context, db *storage.DB, tenant tenancy.ID, roles []string) ([]Listing, error) {
	held := make(map[string]bool, len(roles))
	for _, r := range roles {
		held[r] = true
	}

	out := make([]Listing, 0, len(dashboards))
	for _, d := range Dashboards() {
		listing := Listing{Dashboard: d}
		for _, role := range d.Roles {
			if held[role] {
				listing.Suggested = true
				break
			}
		}

		headline, err := lookup(d.Headline)
		if err != nil {
			// The pack's metrics belong to a capability this build does not
			// have (the AP pack before payables ships). Listed, not renderable.
			out = append(out, listing)
			continue
		}
		if err := authorizeRead(ctx, db, headline.Object); err != nil {
			if isPermissionDenied(err) {
				out = append(out, listing)
				continue
			}
			return nil, err
		}
		listing.Available = true
		out = append(out, listing)
	}
	return out, nil
}

// Render evaluates a pack for a period, dropping the tiles the viewer may not
// see. Rendering is refused outright when the headline is off-limits: a
// dashboard whose main number is missing is not that dashboard.
func Render(ctx context.Context, db *storage.DB, tenant tenancy.ID, name, currency, period string) (Rendered, error) {
	d, ok := dashboards[name]
	if !ok {
		return Rendered{}, fmt.Errorf("%w: %q", ErrUnknownDashboard, name)
	}
	if currency == "" {
		return Rendered{}, errors.New("reporting: currency is required")
	}

	w, err := resolveWindow(ctx, db, tenant, period)
	if err != nil {
		return Rendered{}, err
	}

	out := Rendered{
		Name: d.Name, Title: d.Title, Currency: currency,
		Period: w.Target().Code, AsOf: cardsAsOf(w, time.Now().UTC()),
	}

	headline, err := compareCard(ctx, db, tenant, d.Headline, w, currency)
	if err != nil {
		return Rendered{}, err
	}
	out.Headline = &headline

	for _, tile := range d.Tiles {
		card, err := compareCard(ctx, db, tenant, tile, w, currency)
		if err != nil {
			if isPermissionDenied(err) || errors.Is(err, ErrUnknownMetric) {
				out.Omitted = append(out.Omitted, tile)
				continue
			}
			return Rendered{}, err
		}
		out.Cards = append(out.Cards, card)
	}
	return out, nil
}
