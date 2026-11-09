package ws

import (
	"os"
	"testing"
)

// A temporary home, so no test reads the machine's own ws.json.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "hrn-ws-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HRN_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
