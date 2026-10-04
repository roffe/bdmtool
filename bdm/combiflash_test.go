package bdm

import (
	"os"
	"testing"
	"time"

	"github.com/roffe/bdmtool/firmwares"
)

func TestBootBlock(t *testing.T) {
	fw := []byte{0x01, 0xab, 0xcd, 0xef, 0x9f}
	if got := string(bootBlock(fw, 0, 4)); got != "01ABCDEF\r" {
		t.Fatalf("full block = %q", got)
	}
	// Last block: zero-padded to the block size.
	if got := string(bootBlock(fw, 4, 4)); got != "9F000000\r" {
		t.Fatalf("padded block = %q", got)
	}
}

// TestHardwareCombiFlash flashes the embedded latest firmware to a real
// CombiAdapter: COMBI_HW=1 go test -run CombiFlash -v
func TestHardwareCombiFlash(t *testing.T) {
	if os.Getenv("COMBI_HW") == "" {
		t.Skip("set COMBI_HW=1 to run against an adapter")
	}
	last := uint32(0)
	err := flashCombi(firmwares.CombiAdapterBin, t.Logf, func(done uint32) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if int(last) != len(firmwares.CombiAdapterBin) {
		t.Fatalf("progress ended at %d of %d", last, len(firmwares.CombiAdapterBin))
	}
	// The new firmware has to come up and answer.
	time.Sleep(6 * time.Second)
	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	major, minor, err := c.Version()
	t.Logf("firmware %d.%d %v", major, minor, err)
}
