// The runaway plugin: it never returns.
//
// Nothing in the sandbox can ask it to stop, which is the point — containment
// here is the wall-clock deadline closing the module out from under it
// (wazero's WithCloseOnContextDone), not cooperation.
package main

//go:wasmexport run
func run() int32 {
	n := uint64(0)
	for {
		n++
		// Kept live so no optimiser can decide the loop is dead code and
		// remove the very thing under test.
		if n == 0 {
			return 1
		}
	}
}

func main() {}
