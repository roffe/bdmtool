package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestPacketFraming(t *testing.T) {
	// readmem: 4 bytes, set address, addr 0x00f00000 (big-endian payload)
	got := packet(cmdBDMReadMem, []byte{4, 1, 0x00, 0xf0, 0x00, 0x00})
	want := []byte{0x45, 0x00, 0x06, 4, 1, 0x00, 0xf0, 0x00, 0x00, termAck}
	if !bytes.Equal(got, want) {
		t.Fatalf("packet = % 02X, want % 02X", got, want)
	}
}

func TestRecv(t *testing.T) {
	// Two back-to-back replies, as the firmware streams them during readflash.
	stream := []byte{
		0x20, 0x00, 0x02, 0x03, 0x01, termAck, // fw version 1.3
		0x40, 0x00, 0x00, termAck, // bdm stop ack
		0x40, 0x00, 0x00, termNak, // bdm stop failure
	}
	c := &Combi{rd: bufio.NewReader(bytes.NewReader(stream))}

	d, err := c.recv(cmdFWVersion, 2)
	if err != nil || d[1] != 1 || d[0] != 3 {
		t.Fatalf("version reply = % 02X, %v", d, err)
	}
	if _, err := c.recv(cmdBDMStop, 0); err != nil {
		t.Fatalf("stop ack: %v", err)
	}
	if _, err := c.recv(cmdBDMStop, 0); err == nil {
		t.Fatal("NAK terminator should be an error")
	}
}

func TestMemPayloads(t *testing.T) {
	// Aligned word: one packet, value big-endian.
	got, err := memPayloads(0xfffa04, 0x7f00, 2)
	if err != nil || len(got) != 1 ||
		!bytes.Equal(got[0], []byte{0x00, 0xff, 0xfa, 0x04, 0x7f, 0x00}) {
		t.Fatalf("aligned word = % 02X, %v", got, err)
	}

	// Odd address (0xfffa21 appears in every T5/T7 prepare table): split into
	// byte writes, LOW byte first.
	got, err = memPayloads(0xfffa15, 0x00fe, 2)
	if err != nil || len(got) != 2 ||
		!bytes.Equal(got[0], []byte{0x00, 0xff, 0xfa, 0x15, 0xfe}) ||
		!bytes.Equal(got[1], []byte{0x00, 0xff, 0xfa, 0x16, 0x00}) {
		t.Fatalf("unaligned word = % 02X, %v", got, err)
	}

	if _, err := memPayloads(0x1000, 0, 3); err == nil {
		t.Fatal("size 3 should be rejected")
	}
}

func TestPrepareTables(t *testing.T) {
	// The T7 table embeds two word runs; check the first expands consecutively.
	for i, w := range prepT7 {
		if w.addr == 0xfffa4c {
			for j, want := range []uint32{0xf003, 0x6830, 0x0006, 0x1030} {
				got := prepT7[i+j]
				if got.addr != 0xfffa4c+uint32(2*j) || got.val != want || got.size != 2 {
					t.Fatalf("word run [%d] = %+v", j, got)
				}
			}
			return
		}
	}
	t.Fatal("T7 word run at 0xfffa4c missing")
}

func TestSRAMRoundTrip(t *testing.T) {
	e := &ECUs[3] // Trionic 7, declares an odd 0xffff SRAM size
	if got := sramBytes(e); got != 0x10000 {
		t.Fatalf("sramBytes = %#x, want 0x10000", got)
	}

	// ReadSRAM writes each long word big-endian; WriteSRAM must hand the same
	// bytes back to the adapter in the same order.
	file := []byte{0x12, 0x34, 0x56, 0x78}
	p, err := memPayloads(e.SRAMAddr, binary.BigEndian.Uint32(file), 4)
	if err != nil || len(p) != 1 || !bytes.Equal(p[0][4:], file) {
		t.Fatalf("write payload = % 02X, %v", p, err)
	}
}

// ---- USB BDM (FTDI) ----

func TestBDMFrame(t *testing.T) {
	// read long word at 0x00f00000
	got := frame(grpMemory, cmdReadLong, hex32(0x00f00000))
	if string(got) != "ml00F00000\r" {
		t.Fatalf("frame = %q", got)
	}
}

func TestBDMMemCmds(t *testing.T) {
	// aligned word write
	c, err := memCmds(0xfffa04, 0x7f00, 2)
	if err != nil || len(c) != 1 || c[0].code != cmdWriteWord || c[0].args != "00FFFA047F00" {
		t.Fatalf("word write = %+v, %v", c, err)
	}
	// odd address: split into byte writes, low byte first
	c, _ = memCmds(0xfffa21, 0x1234, 2)
	if len(c) != 2 || c[0].args != "00FFFA2134" || c[1].args != "00FFFA2212" {
		t.Fatalf("split write = %+v", c)
	}
	if _, err := memCmds(0, 0, 3); err == nil {
		t.Fatal("size 3 should be rejected")
	}
}

func TestBDMStripStatus(t *testing.T) {
	// One full 64 byte packet (2 status bytes + 62 payload), then a short one.
	raw := append([]byte{0x31, 0x60}, bytes.Repeat([]byte("A"), 62)...)
	raw = append(raw, 0x01, 0x60, 'B', '\r')
	if got := string(stripStatus(raw)); got != strings.Repeat("A", 62)+"B\r" {
		t.Fatalf("stripStatus = %q", got)
	}
}

func TestMirror(t *testing.T) {
	bin := []byte{1, 2, 3, 4}
	if got := mirror(bin, 8); !bytes.Equal(got, []byte{1, 2, 3, 4, 1, 2, 3, 4}) {
		t.Fatalf("mirror to 8 = %v", got)
	}
	for _, size := range []uint32{4, 6, 2} { // exact, uneven, smaller: untouched
		if got := mirror(bin, size); !bytes.Equal(got, bin) {
			t.Fatalf("mirror to %d = %v, want input", size, got)
		}
	}
}

// TestCombiDriverProgram: on firmware 2.0 a 29F write runs ardubdm's CPU32
// driver through 0x4f/0x50: driver upload and readback (0x4b, a whole block),
// SR, one parameter block, one run, the result block.
func TestCombiDriverProgram(t *testing.T) {
	var in, out bytes.Buffer
	reply := func(code byte, data []byte) { in.Write(packet(code, data)) }
	for range 3 {
		reply(cmdBDMWriteMem, nil) // resetAM29
	}
	reply(cmdBDMWriteMem, nil)   // RAMBAR
	reply(cmdBDMWriteBlock, nil) // driver
	reply(cmdBDMReadFlash, append(bytes.Clone(am29Driver), make([]byte, combiBlock-len(am29Driver))...))
	reply(cmdBDMWriteSysReg, nil)                    // SR
	reply(cmdBDMWriteBlock, nil)                     // parameters + data
	reply(cmdBDMRunWait, []byte{0, 0, 0, 0})         // D0
	reply(cmdBDMReadFlash, make([]byte, combiBlock)) // result 0
	for range 3 {
		reply(cmdBDMWriteMem, nil) // resetAM29
	}
	c := &Combi{rd: bufio.NewReader(&in), out: &out, blk: combiBlock, tmo: defaultTimeout, v2: true}
	e := &ECU{FlashType: "29f400", FlashAddr: 0, FlashSize: 8, DrvAddr: 0x100000, DrvPrep: ram332}
	bin := []byte{0xFF, 0xFF, 0x12, 0x34, 0xFF, 0xFF, 0xFF, 0xFF}
	if err := (&flasher{cpu32: c}).programAM29(e, bin, func(uint32) {}); err != nil {
		t.Fatal(err)
	}
	params := append([]byte{0, 0, 0, 0, 0, 0, 0xAA, 0xAA, 0, 0, 0x55, 0x54, 0, 0xFF, 0xFA, 0x27, 0, 4, 0, 0}, bin...)
	for _, want := range [][]byte{
		packet(cmdBDMWriteBlock, append(be32(0x100000), am29Driver...)),
		packet(cmdBDMWriteBlock, append(be32(0x100060), params...)),
		packet(cmdBDMRunWait, append(be32(0x100000), be32(2000)...)),
		packet(cmdBDMReadFlash, append(be32(0x100060), be32(combiBlock)...)),
	} {
		if !bytes.Contains(out.Bytes(), want) {
			t.Fatalf("missing % 02X", want)
		}
	}
	if in.Len() != 0 {
		t.Fatalf("%d reply bytes left unread", in.Len())
	}

	// Gate: 2.0 and an ECU the drivers cover; everything else keeps 0x4c/0x4d.
	if !c.drivers(ecuByName("Trionic 5.5 (28F010 chips)")) || c.drivers(ecuByName("Volvo CEM")) {
		t.Fatal("drivers() on 2.0")
	}
	c.v2 = false
	if c.drivers(ecuByName("Trionic 7")) {
		t.Fatal("drivers() on 1.x")
	}
}

// TestCombiIdentify: on firmware 2.0 the CombiAdapter identifies a T5.5 with
// Intel 28F010s through 0x44/0x45/0x46 and leaves it running with 0x50 (ms 0).
// On 1.x it must not offer Identify at all.
func TestCombiIdentify(t *testing.T) {
	var in, out bytes.Buffer
	reply := func(code byte, data []byte) { in.Write(packet(code, data)) }
	acks := func(code byte, n int) {
		for range n {
			reply(code, nil)
		}
	}
	reply(cmdBDMRestart, nil)
	acks(cmdBDMWriteSysReg, 2)               // SFC, DFC
	reply(cmdBDMReadMem, []byte{0xCF, 0x00}) // 68332 SIMCR
	acks(cmdBDMWriteMem, 10+3)               // probe prep, autoselect
	reply(cmdBDMReadMem, []byte{0x89, 0x89}) // Intel
	reply(cmdBDMReadMem, []byte{0xB4, 0xB4}) // 28F010
	acks(cmdBDMWriteMem, 4)                  // resets, Vpp off
	reply(cmdBDMRestart, nil)
	reply(cmdBDMRunWait, nil)
	c := combiV2{&Combi{rd: bufio.NewReader(&in), out: &out, blk: combiBlock, tmo: defaultTimeout, v2: true}}
	e, chips, err := c.Identify()
	if err != nil || e.Name != "Trionic 5.5 (28F010 chips)" {
		t.Fatalf("Identify = %v, %q, %v", e, chips, err)
	}
	if !bytes.HasSuffix(out.Bytes(), packet(cmdBDMRunWait, append(be32(0), be32(0)...))) {
		t.Fatalf("did not end with GO: % 02X", out.Bytes())
	}
	if in.Len() != 0 {
		t.Fatalf("%d reply bytes left unread", in.Len())
	}

	var a Adapter = c.Combi
	if _, ok := a.(Identifier); ok {
		t.Fatal("a 1.x CombiAdapter must not offer Identify")
	}
	if _, ok := Adapter(c).(Identifier); !ok {
		t.Fatal("combiV2 must offer Identify")
	}
}
