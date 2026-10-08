package bdm

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/roffe/bdmtool/firmwares"
)

// TestHardwareToy identifies the ECU on a real bdmtoy and dumps its flash and
// SRAM, read only:
// BDMTOY_HW=1 BDMTOY_REF=flash.bin go test -run HardwareToy -v
// With BDMTOY_REF the dump must match it; BDMTOY_OUT saves it.
// BDMTOY_WRITE=image first erases and writes that image (absolute paths).
func TestHardwareToy(t *testing.T) {
	if os.Getenv("BDMTOY_HW") == "" {
		t.Skip("set BDMTOY_HW=1 to run against an adapter")
	}
	c, err := OpenToy()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info()
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + info)
	e, chips, err := c.Identify()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", e.Name, chips)

	if img := os.Getenv("BDMTOY_WRITE"); img != "" {
		bin, err := os.ReadFile(img)
		if err != nil {
			t.Fatal(err)
		}
		debugLog = t.Logf
		start := time.Now()
		if err := c.WriteFlash(e, bin, true, func(uint32) {}); err != nil {
			t.Fatal(err)
		}
		t.Logf("erased and wrote %d bytes in %v", len(bin), time.Since(start).Round(time.Millisecond))
	}

	var buf bytes.Buffer
	start := time.Now()
	if err := c.ReadFlash(e, &buf, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	t.Logf("read %d bytes in %v", buf.Len(), time.Since(start).Round(time.Millisecond))
	if out := os.Getenv("BDMTOY_OUT"); out != "" {
		os.WriteFile(out, buf.Bytes(), 0o644)
	}
	if ref := os.Getenv("BDMTOY_REF"); ref != "" {
		want, err := os.ReadFile(ref)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatal("dump differs from BDMTOY_REF")
		}
	}
	buf.Reset()
	if err := c.ReadSRAM(e, &buf, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	t.Logf("SRAM %d bytes: % X ...", buf.Len(), buf.Bytes()[:16])
}

// TestHardwareToyUpdate writes the embedded bdmtoy firmware over USB (DFU):
// BDMTOY_UPDATE=1 go test ./bdm -run HardwareToyUpdate -v
func TestHardwareToyUpdate(t *testing.T) {
	if os.Getenv("BDMTOY_UPDATE") == "" {
		t.Skip("set BDMTOY_UPDATE=1 to update a connected bdmtoy")
	}
	start := time.Now()
	err := flashToy(firmwares.BdmtoyBin, t.Logf, func(uint32) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("updated in %v", time.Since(start).Round(time.Millisecond))
}
