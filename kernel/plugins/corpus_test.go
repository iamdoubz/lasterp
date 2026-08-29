// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// The hostile-plugin corpus is compiled here, from the Go sources in testdata/,
// with the toolchain this repo already pins (WP-3.1-decisions.md §5).
//
// Not committed .wasm: a corpus of opaque blobs is one nobody can review, and
// "is this still hostile?" stops being an answerable question. Not TinyGo:
// a second toolchain in CI is a dependency in everything but its name.
// `go tool dist list` carries wasip1/wasm on the pinned Go, so the adversary
// is built by the same compiler as the server.

var (
	corpusOnce sync.Once
	corpusDir  string
	corpusErr  error
)

// corpus compiles every testdata module once per test binary and returns the
// directory holding the .wasm files.
func corpus(t *testing.T) string {
	t.Helper()
	corpusOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lasterp-plugin-corpus")
		if err != nil {
			corpusErr = err
			return
		}
		corpusDir = dir
		for _, name := range []string{"hello", "loop", "bomb", "thief", "escape", "hooks", "web"} {
			// -buildmode=c-shared produces a WASI *reactor* module — one with
			// exported entry points and no main() that runs to completion,
			// which is what a plugin is.
			cmd := exec.Command("go", "build", "-buildmode=c-shared",
				"-o", filepath.Join(dir, name+".wasm"), "./"+name)
			cmd.Dir = "testdata"
			cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
			if out, err := cmd.CombinedOutput(); err != nil {
				corpusErr = &buildError{name: name, out: string(out), err: err}
				return
			}
		}
	})
	if corpusErr != nil {
		t.Fatalf("build plugin corpus: %v", corpusErr)
	}
	return corpusDir
}

type buildError struct {
	name string
	out  string
	err  error
}

func (e *buildError) Error() string { return e.name + ": " + e.err.Error() + "\n" + e.out }

// corpusModule returns one compiled module's bytes.
func corpusModule(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpus(t), name+".wasm"))
	if err != nil {
		t.Fatalf("read corpus module %s: %v", name, err)
	}
	return b
}
