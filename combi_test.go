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
