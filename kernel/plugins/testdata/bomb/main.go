// The memory bomb: it allocates until something stops it.
//
// Containment is the linear-memory page cap. The module traps when its own
// runtime cannot grow memory — the host process never allocates on its behalf,
// which is the property that matters: a hostile plugin must not be able to
// spend the *server's* memory.
package main

var sink [][]byte

//go:wasmexport run
func run() int32 {
	for i := 0; i < 1_000_000; i++ {
		sink = append(sink, make([]byte, 1<<20))
	}
	return 0
}

func main() {}
