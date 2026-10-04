package bdm

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestHardwareCombiProbe checks BDM reads on a real CombiAdapter with a T7:
// single word/long READs and DUMPs, then a full flash dump, against a known
// good dump of the same ECU:
// COMBI_HW=1 COMBI_REF=flash.bin go test -run CombiProbe -v
//
// ponytail: diagnostic for the fw 2.0 BDM read failures; delete once 2.0 is trusted.
func TestHardwareCombiProbe(t *testing.T) {
	if os.Getenv("COMBI_HW") == "" {
		t.Skip("set COMBI_HW=1 to run against an adapter")
	}
	ref, _ := os.ReadFile(os.Getenv("COMBI_REF"))
	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	major, minor, err := c.Version()
	t.Logf("firmware %d.%d %v", major, minor, err)
	e := ecuByName("Trionic 7")
	if err := c.enterBDM(e); err != nil {
		t.Fatalf("enterBDM: %v", err)
	}

	// 0x45 read; replies are little-endian, the file big-endian.
	read := func(req []byte, off, size int) {
		d, err := c.cmd(cmdBDMReadMem, req, size)
		if err != nil {
			t.Errorf("0x45 % 02x: %v", req, err)
			return
		}
		got := make([]byte, size)
		for i := range size {
			got[i] = d[size-1-i]
		}
		if len(ref) >= off+size && !bytes.Equal(got, ref[off:off+size]) {
			t.Errorf("0x45 % 02x: % 02x, file % 02x", req, got, ref[off:off+size])
		}
	}
	for range 10 {
		read(append([]byte{2, 1}, be32(0)...), 0, 2) // word READ of 0xffff
		read([]byte{2, 0}, 2, 2)                     // word DUMPs
		read([]byte{2, 0}, 4, 2)
		read(append([]byte{4, 1}, be32(0)...), 0, 4) // long READ
		read([]byte{4, 0}, 4, 4)                     // long DUMP
	}

	start := time.Now()
	var dump bytes.Buffer
	if err := c.ReadFlash(e, &dump, func(uint32) {}); err != nil {
		t.Fatalf("ReadFlash after %d bytes: %v", dump.Len(), err)
	}
	t.Logf("dumped %d bytes in %s", dump.Len(), time.Since(start).Round(time.Millisecond))
	if len(ref) > 0 && !bytes.Equal(dump.Bytes(), ref) {
		got := dump.Bytes()
		off := 0
		for off < len(got) && off < len(ref) && got[off] == ref[off] {
			off++
		}
		t.Fatalf("dump differs from file at %#x (%d vs %d bytes)", off, len(got), len(ref))
	}

	// Abort a dump after two blocks: the read the firmware has in flight
	// must be finished, so the next BDM command still works.
	if err := c.send(cmdBDMReadFlash, append(be32(0), be32(0x10000)...)); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := c.recv(cmdBDMReadFlash, 256); err != nil {
			t.Fatalf("aborted dump, block %d: %v", i, err)
		}
	}
	c.sendBreak(cmdBDMReadFlash)
	for range 3 { // blocks already queued, then the NAK
		c.drain()
		time.Sleep(100 * time.Millisecond)
	}
	read(append([]byte{2, 1}, be32(0)...), 0, 2)
	read(append([]byte{4, 1}, be32(0)...), 0, 4)
}
