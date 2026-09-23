package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakePort scripts the adapter's replies and records what was sent.
type fakePort struct {
	in  bytes.Buffer
	out bytes.Buffer
}

func (f *fakePort) Read(p []byte) (int, error)  { return f.in.Read(p) }
func (f *fakePort) Write(p []byte) (int, error) { return f.out.Write(p) }
func (f *fakePort) Close() error                { return nil }

func TestArduBatch(t *testing.T) {
	f := &fakePort{}
	// version text + CRLF + ok, a bare ok, a word read, then an error flag.
	f.in.WriteString("0100\r\n\r" + "\r" + "FFFF\r\n\r" + "\x07")
	a := &ArduBDM{p: f, tmo: time.Second}

	r, err := a.batch([]req{
		{frame(grpAdapter, cmdVersion, ""), 4},
		wordW(0xAAAA, 0xAAAA),
		wordR(0),
	})
	if err != nil || string(r[0]) != "0100" || r[1] != nil || unhex16(r[2]) != 0xFFFF {
		t.Fatalf("batch = %q, %v", r, err)
	}
	if got := f.out.String(); got != "av\rmW0000AAAAAAAA\rmw00000000\r" {
		t.Fatalf("sent %q", got)
	}
	if _, err := a.cmd(grpMCU, cmdStopMCU, "", 0); err == nil {
		t.Fatal("error flag should fail")
	}
}

func TestAM29Program(t *testing.T) {
	// One 8-byte image: driver upload, SR, one parameter+data block, one run.
	f := &fakePort{}
	ok := func(n int) {
		for range n {
			f.in.WriteByte(termOK)
		}
	}
	ok(3)                  // resetAM29
	ok(1)                  // RAMBAR
	ok(1)                  // driver upload
	f.in.Write(am29Driver) // readback
	f.in.WriteByte(termOK)
	ok(1)                              // SR
	ok(1)                              // block
	f.in.WriteString("00000000\r\n\r") // cw: D0 = 0
	f.in.Write(make([]byte, 20))       // result block: address 0, result 0
	f.in.WriteByte(termOK)
	ok(3) // resetAM29
	a := &ArduBDM{p: f, tmo: time.Second}
	e := &ECU{FlashAddr: 0, FlashSize: 8, DrvAddr: 0x100000, DrvPrep: ram332}
	bin := []byte{0xFF, 0xFF, 0x12, 0x34, 0xFF, 0xFF, 0xFF, 0xFF}
	if err := a.programAM29(e, bin, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	sent := f.out.Bytes()
	drv := append([]byte(fmt.Sprintf("mS00100000%04X\r", len(am29Driver))), am29Driver...)
	blk := append([]byte("mS00100060001C\r"),
		0, 0, 0, 0, 0, 0, 0xAA, 0xAA, 0, 0, 0x55, 0x54, 0, 0xFF, 0xFA, 0x27, 0, 4, 0, 0)
	blk = append(blk, bin...)
	for _, want := range [][]byte{[]byte("mW00FFFB041000\r"), drv, []byte("rW0B00002700\r"), blk, []byte("cw0010000007D0\r")} {
		if !bytes.Contains(sent, want) {
			t.Fatalf("missing %q in %q", want, sent)
		}
	}
	if !erased([]byte{0xFF, 0xFF}) || erased([]byte{0xFF, 0x00}) {
		t.Fatal("erased()")
	}
}

func TestAM28Program(t *testing.T) {
	// 8-byte image, one block: driver upload+readback, SR, timing run, program run.
	f := &fakePort{}
	ok := func(n int) {
		for range n {
			f.in.WriteByte(termOK)
		}
	}
	hdr := func(result uint16) {
		h := make([]byte, am28DrvHeader)
		h[13] = byte(result)
		f.in.Write(h)
		f.in.WriteByte(termOK)
	}
	ok(2)                  // resetAM28
	ok(1)                  // RAMBAR
	ok(1)                  // driver upload
	f.in.Write(am28Driver) // readback
	f.in.WriteByte(termOK)
	ok(1)                              // SR
	ok(1)                              // timing block
	f.in.WriteString("00000000\r\n\r") // cw
	hdr(0)                             // timing result
	ok(1)                              // program block
	f.in.WriteString("00000000\r\n\r") // cw
	hdr(0)                             // program result
	ok(2)                              // resetAM28
	a := &ArduBDM{p: f, tmo: time.Second}
	e := &ECU{FlashAddr: 0x40000, FlashSize: 8, DrvAddr: 0x100000, DrvPrep: ram332}
	bin := []byte{0x12, 0x34, 0xFF, 0xFF, 0x56, 0x78, 0x9A, 0xBC}
	am28TimingCheck = false
	defer func() { am28TimingCheck = true }()
	if err := a.programAM28(e, bin, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	sent := f.out.Bytes()
	blk := append([]byte("mS001001800020\r"),
		0, 4, 0, 0, 0, 0, 0, 0, 0, 1, 0, 4, 0, 0, 0, 0, 0, 28, 0, 17, 0x6D, 0x60, 0, 0)
	blk = append(blk, bin...)
	if !bytes.Contains(sent, blk) {
		t.Fatalf("missing %q in %q", blk, sent)
	}
	if !bytes.Contains(sent, append([]byte("mS001000000198\r"), am28Driver...)) {
		t.Fatalf("driver upload missing in %q", sent)
	}
}

func TestArduBatchWindows(t *testing.T) {
	// 100 word writes (15 bytes each) span several windows; every reply must
	// land in order.
	f := &fakePort{}
	var reqs []req
	for i := range 100 {
		reqs = append(reqs, wordW(uint32(i), uint16(i)))
		f.in.WriteByte(termOK)
	}
	a := &ArduBDM{p: f, tmo: time.Second}
	r, err := a.batch(reqs)
	if err != nil || len(r) != 100 || f.out.Len() != 1500 {
		t.Fatalf("batch: %d replies, %d bytes sent, %v", len(r), f.out.Len(), err)
	}
}

// bootPort spews boot noise, fails the first version query, and answers the
// second one only once it has been sent, like a real device.
type bootPort struct {
	fakePort
	reads int
}

func (b *bootPort) Read(p []byte) (int, error) {
	b.reads++
	switch {
	case b.reads == 1:
		return copy(p, "\xff\xfe"), nil // optiboot garbage, drained
	case b.reads == 3:
		return copy(p, "\x07"), nil // first query: error flag
	case b.out.Len() >= 2*len("av\r"):
		return b.in.Read(p) // second query sent: real reply
	}
	return 0, nil // quiet
}

func TestArduHandshake(t *testing.T) {
	b := &bootPort{}
	b.in.WriteString("0100\r\n\r")
	a := &ArduBDM{p: b}
	if err := a.handshake(); err != nil {
		t.Fatal(err)
	}
	if a.tmo != arduTimeout {
		t.Fatalf("tmo = %v after handshake", a.tmo)
	}
	if b.out.String() != "av\rav\r" {
		t.Fatalf("sent %q, want two version queries", b.out.String())
	}
}

func TestCMFIProgram(t *testing.T) {
	// 1280-byte image: the first chunk is erased and skipped, the second is
	// programmed. Driver upload + init, then one write run.
	f := &fakePort{}
	ok := func(n int) {
		for range n {
			f.in.WriteByte(termOK)
		}
	}
	ok(2)                  // driver upload, two blocks
	f.in.Write(cmfiDriver) // readback
	f.in.WriteByte(termOK)
	ok(1)                              // SR
	ok(1)                              // D0 = init
	f.in.WriteString("00000001\r\n\r") // init returned ok
	ok(1)                              // buffer block write
	ok(3)                              // D1, A0, A1
	ok(1)                              // D0 = write
	f.in.WriteString("00000001\r\n\r") // write returned ok
	a := &ArduBDM{p: f, tmo: time.Second}
	e := &ECU{Name: "MCP", FlashType: "cmfi", FlashSize: 1280, DrvAddr: 0x100000}
	bin := bytes.Repeat([]byte{0xFF}, 1024)
	bin = append(bin, bytes.Repeat([]byte{0x12, 0x34}, 128)...)
	if err := a.programCMFI(e, bin, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	sent := f.out.Bytes()
	for _, want := range []string{
		"mS001000000400\r",                   // driver, first block
		"rW0B00002700\r",                     // SR
		"rA0000000004\r", "cw001000001388\r", // init and its run
		"mS001008000100\r",                   // the buffer
		"rA0100000080\r",                     // D1 = 128 words
		"rA0800000400\r",                     // A0 = offset 0x400
		"rA0900100800\r",                     // A1 = buffer
		"rA0000000001\r", "cw001000007530\r", // write and its run
	} {
		if !bytes.Contains(sent, []byte(want)) {
			t.Fatalf("missing %q in %q", want, sent)
		}
	}
	if bytes.Contains(sent, []byte("rA0800000000\r")) {
		t.Fatal("erased first chunk was programmed")
	}
}

func TestCMFIShadowRead(t *testing.T) {
	// SIE is set for the last 256 bytes of the image and cleared after.
	f := &fakePort{}
	// one reply per block dump the host asks for
	body := func(n int, fill byte) {
		for done := 0; done < n; done += arduBlock {
			f.in.Write(bytes.Repeat([]byte{fill}, min(arduBlock, n-done)))
			f.in.WriteByte(termOK)
		}
	}
	f.in.WriteString("4800\r\n\r") // CMFIMCR, SIE already clear
	body(cmfiArray, 0xAA)          // the array, one 4 KB block at a time in reality
	f.in.WriteString("4800\r\n\r") // CMFIMCR before setting SIE
	f.in.WriteByte(termOK)         // the write that sets it
	body(cmfiShadow, 0x55)         // the shadow row
	f.in.WriteString("6800\r\n\r") // CMFIMCR before clearing SIE
	f.in.WriteByte(termOK)
	a := &ArduBDM{p: f, tmo: time.Second}
	var buf bytes.Buffer
	e := &ECU{Name: "MCP", FlashType: "cmfi", FlashSize: cmfiArray + cmfiShadow}
	if err := a.readCMFI(e, &buf, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != cmfiArray+cmfiShadow || buf.Bytes()[cmfiArray] != 0x55 {
		t.Fatalf("read %d bytes, shadow starts %02X", buf.Len(), buf.Bytes()[cmfiArray])
	}
	sent := f.out.String()
	for _, want := range []string{"mW00FFF8006800", "mW00FFF8004800"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("missing %q in %q", want, sent)
		}
	}
}

func TestPrepareDelay(t *testing.T) {
	// A delay() step in a prepare sequence pauses instead of writing.
	f := &fakePort{}
	for range 4 {
		f.in.WriteByte(termOK) // restart, SFC, DFC, the one real write
	}
	a := &ArduBDM{p: f, tmo: time.Second}
	e := &ECU{Name: "x", Prepare: []memWrite{delay(1), w16(0xfffa04, 0xd608)}}
	if err := a.enterBDM(e); err != nil {
		t.Fatal(err)
	}
	if got, want := f.out.String(), "cs\rrW0E00000005\rrW0F00000005\rmW00FFFA04D608\r"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
}
