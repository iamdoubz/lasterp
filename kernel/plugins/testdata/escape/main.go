// The escape attempt: a plugin reaching for the machine rather than the API.
//
// It tries the filesystem, the environment and the network — the three things
// a WASI module can normally touch. The sandbox mounts no filesystem, exports
// no environment and allows no hosts, so each attempt fails inside the module
// and it reports what happened rather than trapping. A test that only asserted
// "it failed" could not tell containment from a plugin that crashed early, so
// this one returns evidence.
package main

import (
	"encoding/json"
	"net"
	"os"

	"github.com/extism/go-pdk"
)

//go:wasmexport escape
func escape() int32 {
	result := map[string]string{}

	if _, err := os.ReadFile("/etc/passwd"); err != nil {
		result["read_etc_passwd"] = "refused: " + err.Error()
	} else {
		result["read_etc_passwd"] = "READ THE FILE"
	}

	if err := os.WriteFile("/tmp/escaped", []byte("x"), 0o600); err != nil {
		result["write_tmp"] = "refused: " + err.Error()
	} else {
		result["write_tmp"] = "WROTE THE FILE"
	}

	if entries, err := os.ReadDir("/"); err != nil {
		result["list_root"] = "refused: " + err.Error()
	} else {
		result["list_root"] = "LISTED " + string(rune('0'+len(entries)))
	}

	if v, ok := os.LookupEnv("LASTERP_DSN"); ok {
		result["read_env"] = "READ " + v
	} else {
		result["read_env"] = "refused: not set"
	}

	if conn, err := net.Dial("tcp", "127.0.0.1:5432"); err != nil {
		result["dial"] = "refused: " + err.Error()
	} else {
		_ = conn.Close()
		result["dial"] = "CONNECTED"
	}

	out, err := json.Marshal(result)
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(out)
	return 0
}

func main() {}
