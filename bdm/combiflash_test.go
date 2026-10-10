package bdm

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/roffe/bdmtool/firmwares"
	"go.bug.st/serial"
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

// comFake is the 1.x bootloader behind a COM port, handing out one byte per
// Read: a serial read can split a USB packet.
type comFake struct {
	serial.Port
	in, out []byte
}

func (f *comFake) Write(b []byte) (int, error) {
	f.out = append(f.out, b...)
	switch {
	case string(b) == "UPDT\r":
		f.in = append(f.in, "RDY\r"...)
	case len(b) == 9 && b[8] == '\r':
		f.in = append(f.in, '\r')
	}
	return len(b), nil
}

func (f *comFake) Read(b []byte) (int, error) {
	if len(f.in) == 0 {
		return 0, nil // timeout
	}
	b[0], f.in = f.in[0], f.in[1:]
	return 1, nil
}

func (f *comFake) SetReadTimeout(time.Duration) error { return nil }
func (f *comFake) ResetInputBuffer() error            { return nil }

func TestComBoot(t *testing.T) {
	f := &comFake{}
	c := &comBoot{p: f}
	block, err := bootSignIn(c)
	if err != nil || block != 4 {
		t.Fatalf("sign-in: block %d, %v", block, err)
	}
	if err := bootSend(c, block, []byte{1, 2, 3, 4, 5}, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	if want := "UPDT\r01020304\r05000000\rEXIT\r"; string(f.out) != want {
		t.Fatalf("sent %q, want %q", f.out, want)
	}
	// A reply without \r ends at the silence after it.
	f.in = []byte{0x26, 0xff, 0, 0}
	if r, err := c.bootRead(100); err != nil || !bytes.Equal(r, []byte{0x26, 0xff, 0, 0}) {
		t.Fatalf("NAK read %x, %v", r, err)
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
