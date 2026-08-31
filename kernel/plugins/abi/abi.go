// SPDX-License-Identifier: Apache-2.0

// Package abi carries the plugin ABI's author-facing half: the scaffolds
// `lasterp plugin new` writes and the version of the host contract they target.
//
// **Apache-2.0, unlike the rest of this repository** (ADR-012): the plugin ABI
// boundary doubles as the licensing boundary, and everything a third party
// links against or starts their own code from lives on the Apache side. A
// scaffold becomes the author's plugin, so it is theirs under any license they
// choose. The directory's LICENSE file has held this place since WP-0.1.
package abi

import "embed"

// Version is the host contract these scaffolds target (docs/05: "versioned ABI
// (`lasterp-pdk/v1`); host guarantees compatibility within major version").
//
// It names the *shape* of the host functions — JSON in, JSON out, one argument,
// `{"ok":…}` replies — not the set of them: a plugin compiled against v1 keeps
// working when a capability adds a function, because the table is built from
// what its own manifest asked for.
const Version = "lasterp-pdk/v1"

// Scaffold holds one starter project per language.
//
// **There is no separately published PDK module**, deliberately (WP-3.2b). A
// versioned `lasterp-pdk` package per language is four release pipelines and
// four compatibility promises for what is, per language, forty lines of host
// bindings. The scaffold writes those lines into the author's own project
// instead — they can read them, step through them, and are not waiting on us to
// publish a release before they can build.
//
//go:embed scaffold
var Scaffold embed.FS
