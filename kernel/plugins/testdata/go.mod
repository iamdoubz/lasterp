// The WP-3.1a hostile-plugin corpus (docs/19 §3), kept as readable Go rather
// than committed .wasm blobs: a hostile corpus nobody can read is a hostile
// corpus nobody can tell has stopped being hostile (WP-3.1-decisions.md §5).
//
// A module of its own so its wasip1 build constraints and its PDK dependency
// stay out of the server's build. Compiled at test time by corpus_test.go with
// the toolchain the repo already pins — no TinyGo, no second toolchain.
module lasterp.test/plugin-corpus

go 1.26.5

require github.com/extism/go-pdk v1.1.3
