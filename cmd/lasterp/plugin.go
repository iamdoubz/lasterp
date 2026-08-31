// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/plugins"
)

// `lasterp plugin …` — the author's and the operator's half of WP-3.2b.
//
// **Everything that changes a server goes over the authenticated API**, never
// through the database. An install is an approval decision that has to be
// attributable to a person (INV-T4) and bounded by *that person's* grants
// (INV-T3), and a CLI holding the database's own credentials can offer neither
// (WP-3.1-decisions.md §6). So `install` and `bindings` take a server URL and a
// token, and everything else — `new`, `keygen`, `pack` — is local file work
// that touches no deployment at all.

const pluginUsage = "usage: lasterp plugin <new|keygen|pack|install|bindings>"

func pluginCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(pluginUsage)
	}
	switch args[0] {
	case "new":
		return pluginNew(args[1:])
	case "keygen":
		return pluginKeygen(args[1:])
	case "pack":
		return pluginPack(args[1:])
	case "install":
		return pluginInstall(ctx, args[1:])
	case "bindings":
		return pluginBindings(ctx, args[1:])
	default:
		return fmt.Errorf("unknown plugin command %q; %s", args[0], pluginUsage)
	}
}

// pluginNew scaffolds a starter project.
func pluginNew(args []string) error {
	fs := flag.NewFlagSet("plugin new", flag.ExitOnError)
	lang := fs.String("lang", "go", "language: "+strings.Join(plugins.ScaffoldLangs, ", "))
	id := fs.String("id", "", "plugin id, e.g. com.acme.commission-calc")
	dir := fs.String("dir", "", "directory to write into (default: the id's last segment)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("plugin new: -id is required")
	}
	target := *dir
	if target == "" {
		target = lastSegment(*id)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return err
	}
	written, err := plugins.NewPlugin(target, *lang, *id)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s/\n", target)
	for _, f := range written {
		fmt.Printf("  %s\n", f)
	}
	fmt.Printf("\nnext: read manifest.yaml, then build (see the header of the source file)\n")
	return nil
}

// pluginKeygen makes a publisher signing key.
func pluginKeygen(args []string) error {
	fs := flag.NewFlagSet("plugin keygen", flag.ExitOnError)
	out := fs.String("out", "publisher.key", "file to write the signing key to")
	id := fs.String("id", "", "key id, e.g. acme-2026 — what a trust file names")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := plugins.NewSigningKey(*out, *id)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (mode 0600 — keep it secret, and back it up)\n\n", *out)
	fmt.Printf("Add this line to the trust file of every deployment that should accept\nyour bundles (%s):\n\n  %s\n",
		plugins.EnvTrustFile, key.PublicLine())
	return nil
}

// pluginPack builds a signed bundle from a manifest and a module.
func pluginPack(args []string) error {
	fs := flag.NewFlagSet("plugin pack", flag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "manifest file")
	modulePath := fs.String("module", "plugin.wasm", "compiled WebAssembly module")
	keyPath := fs.String("key", "publisher.key", "signing key from `lasterp plugin keygen`")
	out := fs.String("out", "", "bundle to write (default: <id>-<version>.tar.gz)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifestYAML, err := os.ReadFile(*manifestPath) // #nosec G304 -- the author's own manifest
	if err != nil {
		return fmt.Errorf("plugin pack: %w", err)
	}
	module, err := os.ReadFile(*modulePath) // #nosec G304 -- the author's own module
	if err != nil {
		return fmt.Errorf("plugin pack: %w", err)
	}
	key, err := plugins.LoadSigningKey(*keyPath)
	if err != nil {
		return err
	}
	bundle, err := plugins.Pack(manifestYAML, module, key.ID, key.Key)
	if err != nil {
		return err
	}
	// Opened again from its own bytes, so what is printed is what a server will
	// compute rather than what the packer believes it wrote.
	opened, err := plugins.OpenBundle(bundle)
	if err != nil {
		return err
	}
	manifest, err := plugins.ParseManifest(opened.ManifestYAML)
	if err != nil {
		return err
	}
	target := *out
	if target == "" {
		target = fmt.Sprintf("%s-%s.tar.gz", lastSegment(manifest.ID), manifest.Version)
	}
	// #nosec G703 -- target is the publisher's own -out flag, on their own machine.
	if err := os.WriteFile(target, bundle, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n  plugin  %s %s\n  digest  %s\n  module  sha256:%s\n  signer  %s\n",
		target, manifest.ID, manifest.Version, opened.Digest, opened.ModuleSHA256, key.ID)
	return nil
}

// pluginInstall fetches a bundle — from a file or a registry — and installs it
// through the authenticated API.
func pluginInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plugin install", flag.ExitOnError)
	server := fs.String("server", envOr("LASTERP_URL", "http://localhost:8080"), "LastERP server URL")
	token := fs.String("token", os.Getenv("LASTERP_TOKEN"), "session token of the administrator approving the install")
	registry := fs.String("registry", os.Getenv("LASTERP_REGISTRY"), "registry base URL, for `id@version` references")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ref := fs.Arg(0)
	if ref == "" {
		return errors.New("plugin install: give a bundle file or an `id@version` reference")
	}
	if *token == "" {
		return errors.New("plugin install: -token (or LASTERP_TOKEN) is required — an install is attributable to a person")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	var bundle []byte
	var err error
	switch {
	case isLocalPath(ref):
		bundle, err = os.ReadFile(ref) // #nosec G304 -- the operator's own file
	case *registry == "":
		err = errors.New("plugin install: -registry (or LASTERP_REGISTRY) is required for an id@version reference")
	default:
		var index plugins.Index
		if index, err = plugins.FetchIndex(ctx, client, *registry); err == nil {
			var entry plugins.IndexEntry
			if entry, err = index.Resolve(ref); err == nil {
				fmt.Printf("resolved %s to %s %s\n", ref, entry.ID, entry.Version)
				bundle, err = plugins.FetchBundle(ctx, client, *registry, entry)
			}
		}
	}
	if err != nil {
		return err
	}

	// Opened locally first, so an operator sees what they are about to approve
	// — and so a malformed bundle fails on their machine rather than as a 422
	// from a server.
	opened, err := plugins.OpenBundle(bundle)
	if err != nil {
		return err
	}
	manifest, err := plugins.ParseManifest(opened.ManifestYAML)
	if err != nil {
		return err
	}
	fmt.Printf("installing %s %s (digest %s, signed by %s)\n",
		manifest.ID, manifest.Version, opened.Digest, opened.Signature.KeyID)

	body, err := json.Marshal(map[string]string{"bundle": base64.StdEncoding.EncodeToString(bundle)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(*server, "/")+"/api/v1/plugins/bundle", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	req.Header.Set("Content-Type", "application/json")
	// Every write takes an idempotency key (ADR-009). It is a **fresh** key per
	// invocation, deliberately not the bundle digest: keys never expire, so
	// digest-keyed installs would make a legitimate reinstall after an
	// uninstall replay the original response and quietly install nothing. A
	// retry of an interrupted install instead reaches the server twice and is
	// refused the second time with "already installed", which is the honest
	// answer.
	req.Header.Set("Idempotency-Key", idgen.New())

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("plugin install: %s: %s", resp.Status, strings.TrimSpace(string(answer)))
	}
	printInstallResult(answer)
	return nil
}

// printInstallResult shows what the server approved — the latency warnings, the
// routes it now serves and the hosts it may call. An operator who installs
// without reading them is the person the warnings exist for.
func printInstallResult(answer []byte) {
	var result struct {
		ID        string   `json:"id"`
		Version   string   `json:"version"`
		SHA256    string   `json:"sha256"`
		Warnings  []string `json:"warnings"`
		Endpoints []struct {
			Path    string   `json:"path"`
			Methods []string `json:"methods"`
		} `json:"endpoints"`
		OutboundHosts []struct {
			Host    string   `json:"host"`
			Methods []string `json:"methods"`
		} `json:"outbound_hosts"`
	}
	if err := json.Unmarshal(answer, &result); err != nil {
		fmt.Println(string(answer))
		return
	}
	fmt.Printf("installed %s %s (sha256:%s)\n", result.ID, result.Version, result.SHA256)
	for _, e := range result.Endpoints {
		fmt.Printf("  serves   %s %s\n", strings.Join(e.Methods, ","), e.Path)
	}
	for _, h := range result.OutboundHosts {
		fmt.Printf("  calls    %s %s\n", strings.Join(h.Methods, ","), h.Host)
	}
	for _, w := range result.Warnings {
		fmt.Printf("  warning  %s\n", w)
	}
}

// pluginBindings generates Go types for the calling tenant's objects.
func pluginBindings(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plugin bindings", flag.ExitOnError)
	server := fs.String("server", envOr("LASTERP_URL", "http://localhost:8080"), "LastERP server URL")
	token := fs.String("token", os.Getenv("LASTERP_TOKEN"), "session token whose tenant's schema to generate against")
	pkg := fs.String("package", "main", "package name for the generated file")
	out := fs.String("out", "objects.go", "file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		return errors.New("plugin bindings: -token (or LASTERP_TOKEN) is required — schemas are per-tenant")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(*server, "/")+"/api/v1/meta/objects", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("plugin bindings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plugin bindings: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	objects, err := plugins.ParseMetaObjects(body)
	if err != nil {
		return err
	}
	src, err := plugins.GenerateGoBindings(*pkg, objects)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, src, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d objects)\n", *out, len(objects))
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func lastSegment(id string) string {
	if i := strings.LastIndex(id, "."); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

// isLocalPath distinguishes `./commission-calc.tar.gz` from `com.acme.x@1.0.0`.
func isLocalPath(ref string) bool {
	return strings.HasSuffix(ref, ".tar.gz") || strings.ContainsAny(ref, "/\\")
}
