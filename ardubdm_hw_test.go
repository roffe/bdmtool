package main

import (
	"os"
	"testing"
)

// TestHardwareIdentify talks to a real adapter: ARDUBDM_PORT=/dev/ttyUSB5 go test -run Hardware -v
func TestHardwareIdentify(t *testing.T) {
	port := os.Getenv("ARDUBDM_PORT")
	if port == "" {
		t.Skip("set ARDUBDM_PORT to run against an adapter")
	}
	a, err := OpenArdu(port)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	info, err := a.Info()
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + info)
	e, chips, err := a.Identify()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", e.Name, chips)
}
