// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"

	"github.com/iamdoubz/lasterp/kernel/plugins/abi"
)

// `lasterp plugin new` (WP-3.2b): the scaffolds that make the afternoon-plugin
// promise checkable.
//
// Four languages ship, because a scaffold is a template and withholding three
// of them helps nobody. **One is proven in CI** — Go, whose `wasip1/wasm`
// target the repo's own toolchain already builds (it is how the hostile corpus
// is compiled). Rust, TypeScript and Python are asserted to render, to carry a
// manifest this host parses, and to name their build command; whether they
// compile is their toolchain's business. Putting rustc, `extism-js` and a
// Python wasm compiler into CI to prove three templates would be three
// dependencies in everything but name (WP-3.2-decisions.md §5).

// ScaffoldLangs are the languages `lasterp plugin new` can write.
var ScaffoldLangs = []string{"go", "rust", "ts", "python"}

// scaffoldData is what the templates interpolate.
type scaffoldData struct {
	ID             string // com.acme.commission-calc
	Name           string // commission-calc — the directory and package name
	NameUnderscore string // commission_calc — Rust's crate artifact name
	Module         string // the Go module path
	Lang           string
	HostVersion    string
	ABIVersion     string
	GoVersion      string
}

// NewPlugin writes a starter project for id into dir, which must not already
// exist as a non-empty directory — a scaffold that overwrites is a scaffold
// that eats somebody's afternoon of work.
func NewPlugin(dir, lang, id string) ([]string, error) {
	if !declared(ScaffoldLangs, lang) {
		return nil, fmt.Errorf("plugins: unknown language %q (want one of %s)", lang, strings.Join(ScaffoldLangs, ", "))
	}
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("plugins: %q is not a valid plugin id (lowercase, dots and dashes, e.g. com.acme.commission-calc)", id)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("plugins: %s is not empty; refusing to scaffold over it", dir)
	}

	name := shortName(id)
	data := scaffoldData{
		ID:             id,
		Name:           name,
		NameUnderscore: strings.ReplaceAll(name, "-", "_"),
		Module:         "example.com/" + name,
		Lang:           lang,
		HostVersion:    HostVersion,
		ABIVersion:     abi.Version,
		GoVersion:      goDirectiveVersion(),
	}

	root := "scaffold/" + lang
	var written []string
	err := fs.WalkDir(abi.Scaffold, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(abi.Scaffold, path)
		if err != nil {
			return err
		}
		tmpl, err := template.New(path).Parse(string(body))
		if err != nil {
			return fmt.Errorf("plugins: scaffold template %s: %w", path, err)
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			return fmt.Errorf("plugins: render %s: %w", path, err)
		}
		// `main.go.tmpl` → `main.go`; the .tmpl suffix keeps template sources
		// out of this repository's own build.
		rel := strings.TrimSuffix(strings.TrimPrefix(path, root+"/"), ".tmpl")
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(target, out.Bytes(), 0o600); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(written)
	return written, nil
}

// shortName is the last dotted segment of an id: com.acme.commission-calc →
// commission-calc.
func shortName(id string) string {
	if i := strings.LastIndex(id, "."); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

// goDirectiveVersion is HostVersion's toolchain counterpart: the `go` line a
// scaffolded module gets. It is the language version this server was built
// with, so a scaffold never asks for a newer toolchain than the host itself
// needs.
func goDirectiveVersion() string {
	// runtime.Version() is "go1.26.7" or "go1.26.7 X:something"; the go
	// directive wants "1.26.7".
	if m := goVersionRE.FindStringSubmatch(runtime.Version()); m != nil {
		return m[1]
	}
	return "1.26"
}

var goVersionRE = regexp.MustCompile(`^go(\d+\.\d+(?:\.\d+)?)`)
