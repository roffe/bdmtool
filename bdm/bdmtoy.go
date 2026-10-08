// Package bdm: bdmtoy transport (STM32F103 multi-target adapter, ffff:0107),
// old-school CPU32 BDM only, firmware 2.2 or later.
//
// Firmware 2.2 is a vendor-specific device (interface 0, bulk EP 0x03 out and
// 0x81 in) that Windows binds WinUSB to on its own; earlier firmware is a
// CDC-ACM modem with the same endpoints on interface 1. Either way it speaks
// raw frames over libusb, as its own host does. Everything is
// 16-bit little-endian words, 32-bit values low word first. To the adapter:
// [total words][count], then per command [cmd][2 + arg words][args]. Back:
// [total words], then per command [cmd][status][3 + data words][data]. A dump
// instead streams [words][0x0050][status][addr lo][addr hi][up to 1 KB]
// frames, or one [5][0x0050][fault][addr lo][addr hi] where it fails. Target
// long words travel little-endian both ways.
//
// The firmware takes one frame at a time and holds the host off (NAK) while
// it works, so this is strict request/response. Firmware 2.2 is the
// bdmtool-compatible release: the upstream firmware has no version command,
// reads slow memory wrong, cannot address 0 and can wedge its USB endpoint,
// and is refused (see the bdmtoy repo's firmware/README.md).
package bdm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gotmc/libusb/v2"
)

const (
	toyVID = 0xFFFF
	toyPID = 0x0107

	// Untyped: gotmc's transfer methods take an unexported endpoint type.
	toyIn  = 0x81
	toyOut = 0x03

	toySetInterface = 0x0001
	toyVersion      = 0x0002 // firmware 1.0+: major << 8 | minor
	toyTargetReady  = 0x0012 // reset into BDM: RESET pulsed with BKPT held
	toyTargetReset  = 0x0013 // reset and run
	toyTargetStart  = 0x0014 // GO
	toyTargetStop   = 0x0015 // BKPT halt, no reset; fails if the target does not halt
	toyTargetStatus = 0x0016 // data: 1 = in BDM (FREEZE), 0 = running
	toyReadMem      = 0x0040
	toyWriteMem     = 0x0041
	toyDumpMem      = 0x0050
	toyFillMem      = 0x0051
	toyReadReg      = 0x0060
	toyWriteReg     = 0x0061

	toyBDMOld    = 0x0000 // TAP_IO_BDMOLD
	toyBigEndian = 0x8000 // config mask
	toyMinFW     = 0x0202

	// BDM clocks. The slow one is safe on a CPU straight out of reset (8 MHz
	// on a 68332); the fast one once a prep has the CPU at 16 MHz or more.
	// Firmware 1.0 dumps a T7 clean at both (bench, 2026-10-08).
	toySlowClock = 1000000 // Hz
	toyFastClock = 6000000 // Hz

	toyFrame   = 2048 // adapter buffers, bytes
	toyLoad    = toyFrame - 16
	toyTimeout = 4000

	// CPU32 BDM register commands
	bdmRDREG = 0x2180 // + 0-7 D0-D7, 8-15 A0-A7
	bdmWRREG = 0x2080
	bdmWSREG = 0x2480 // + system register: 0 = PC
)

// toyStatus names the firmware's return codes (shared/enums.h).
var toyStatus = map[uint16]string{
	0xF000: "not supported", 0xF001: "interface not set up", 0xF002: "malformed request",
	0xF010: "target did not enter BDM", 0xF011: "target did not start",
	0xF020: "bus error", 0xF021: "illegal BDM command", 0xF022: "unknown BDM error",
	0xF023: "target not responding (a bus cycle that never ended?)", 0xF040: "unaligned access",
}

// errToyNotSup is what firmware before 1.0 answers the version command with.
var errToyNotSup = errors.New("not supported")

func toyErr(cmd, st uint16) error {
	if st == 0xF000 {
		return fmt.Errorf("bdmtoy command %04X: %w", cmd, errToyNotSup)
	}
	if s, ok := toyStatus[st]; ok {
		return fmt.Errorf("bdmtoy command %04X: %s", cmd, s)
	}
	return fmt.Errorf("bdmtoy command %04X: status %04X", cmd, st)
}

// Toy is a bdmtoy adapter.
type Toy struct {
	ctx *libusb.Context
	dev *libusb.Device
	h   *libusb.DeviceHandle
	tmo int    // ms
	fw  uint16 // firmware version
}

// OpenToy connects to a bdmtoy on firmware 2.2 or later.
func OpenToy() (*Toy, error) { return openToy(toyMinFW) }

// openToy takes any firmware from minFW on: the updater talks to whatever a
// dongle runs.
func openToy(minFW uint16) (*Toy, error) {
	ctx, err := libusb.NewContext()
	if err != nil {
		return nil, fmt.Errorf("libusb init: %w", err)
	}
	dev, h, err := ctx.OpenDeviceWithVendorProduct(toyVID, toyPID)
	if err != nil {
		ctx.Close()
		return nil, fmt.Errorf("bdmtoy not found: %w", err)
	}
	t := &Toy{ctx: ctx, dev: dev, h: h, tmo: toyTimeout}
	// A USB reset also resets the firmware's frame parser, so whatever an
	// earlier session left half-sent, this starts on a frame boundary.
	if err := h.ResetDevice(); err != nil {
		t.Close()
		return nil, fmt.Errorf("bdmtoy USB reset: %w", err)
	}
	_ = h.SetAutoDetachKernelDriver(true)
	if err := h.ClaimInterface(0); err != nil {
		t.Close()
		return nil, fmt.Errorf("claim interface 0: %w", err)
	}
	// Only there on the CDC layout of firmware before 2.2, which then still
	// answers the version command and is told it is too old.
	_ = h.ClaimInterface(1)
	t.drain()
	v, err := t.cmd(toyVersion)
	if err != nil || len(v) < 1 || v[0] < minFW {
		t.Close()
		if err != nil && !errors.Is(err, errToyNotSup) {
			return nil, fmt.Errorf("bdmtoy did not respond: %w", err)
		}
		return nil, errors.New("bdmtoy firmware is too old: bdmtool needs firmware 2.2 or later (see the README)")
	}
	t.fw = v[0]
	if err := t.setClock(toySlowClock); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

func (t *Toy) Close() {
	if t.h != nil {
		_ = t.h.ReleaseInterface(0)
		_ = t.h.ReleaseInterface(1)
		_ = t.h.Close()
	}
	if t.dev != nil {
		t.dev.Close()
	}
	if t.ctx != nil {
		_ = t.ctx.Close()
	}
}

// =====================
// transport
// =====================

// drain drops whatever an earlier session left unread.
func (t *Toy) drain() {
	buf := make([]byte, 64)
	for range 100 {
		if _, err := t.h.BulkTransfer(toyIn, buf, len(buf), 50); err != nil {
			return
		}
	}
}

// tx sends commands, each [cmd, args...], as one frame.
func (t *Toy) tx(cmds ...[]uint16) error {
	_, err := t.h.BulkTransferOut(toyOut, toyFrameOut(cmds...), t.tmo)
	return err
}

// toyFrameOut builds the frame for cmds: the length words go in here.
func toyFrameOut(cmds ...[]uint16) []byte {
	w := []uint16{0, uint16(len(cmds))}
	for _, c := range cmds {
		w = append(w, c[0], uint16(len(c)+1))
		w = append(w, c[1:]...)
	}
	w[0] = uint16(len(w))
	b := make([]byte, 2*len(w))
	for i, v := range w {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

// rx reads one frame. Frames start on a USB packet, so the first read takes
// one packet, which carries the length; the rest is asked for exactly. A
// frame that is a multiple of 64 bytes ends without a short packet, which a
// larger read would sit waiting for.
func (t *Toy) rx() ([]uint16, error) {
	buf := make([]byte, toyFrame+64)
	n, err := t.h.BulkTransfer(toyIn, buf, 64, t.tmo)
	if err == nil && n == 0 { // the zero-length packet after a frame that filled its last packet
		n, err = t.h.BulkTransfer(toyIn, buf, 64, t.tmo)
	}
	if err != nil {
		return nil, fmt.Errorf("no response from bdmtoy: %w", err)
	}
	total := 0
	if n >= 2 {
		total = 2 * int(binary.LittleEndian.Uint16(buf))
	}
	if total < max(n, 2) || total > len(buf) {
		return nil, fmt.Errorf("bdmtoy: bad frame % X", buf[:n])
	}
	for n < total {
		k, err := t.h.BulkTransfer(toyIn, buf[n:], total-n, t.tmo)
		if err != nil {
			return nil, fmt.Errorf("bdmtoy frame cut short: %w", err)
		}
		n += k
	}
	w := make([]uint16, total/2)
	for i := range w {
		w[i] = binary.LittleEndian.Uint16(buf[2*i:])
	}
	return w, nil
}

// do runs commands in one frame and returns each one's reply data. The
// adapter stops at the first command that fails.
func (t *Toy) do(cmds ...[]uint16) ([][]uint16, error) {
	if err := t.tx(cmds...); err != nil {
		return nil, err
	}
	r, err := t.rx()
	if err != nil {
		return nil, err
	}
	return toyReplies(r, len(cmds))
}

// toyReplies splits a reply frame into each command's data, failing on the
// first bad status.
func toyReplies(r []uint16, n int) ([][]uint16, error) {
	var out [][]uint16
	for p := r[1:]; len(p) > 0; p = p[p[2]:] {
		if len(p) < 3 || p[2] < 3 || int(p[2]) > len(p) {
			return nil, fmt.Errorf("bdmtoy: bad reply % 04X", r)
		}
		if p[1] != 0 {
			return nil, toyErr(p[0], p[1])
		}
		out = append(out, p[3:p[2]])
	}
	if len(out) != n {
		return nil, fmt.Errorf("bdmtoy: %d replies to %d commands", len(out), n)
	}
	return out, nil
}

func (t *Toy) cmd(c ...uint16) ([]uint16, error) {
	r, err := t.do(c)
	if err != nil {
		return nil, err
	}
	return r[0], nil
}

// =====================
// BDM primitives
// =====================

func (t *Toy) Version() (major, minor byte, err error) {
	return byte(t.fw >> 8), byte(t.fw), nil
}

func (t *Toy) setClock(hz uint32) error {
	_, err := t.cmd(toySetInterface, toyBDMOld, toyBigEndian, uint16(hz), uint16(hz>>16))
	return err
}

// Restart resets into BDM, on the slow clock: the CPU comes up at its reset
// clock.
func (t *Toy) Restart() error {
	_, err := t.do(toyClockCmd(toySlowClock), []uint16{toyTargetReady})
	return err
}

// Stop halts a running MCU through BKPT, without a reset. Where BKPT does not
// halt it (a CPU32 whose last reset left BDM off; the T7 halts regardless)
// it fails, and Restart is the way in.
func (t *Toy) Stop() error {
	_, err := t.do(toyClockCmd(toySlowClock), []uint16{toyTargetStop})
	return err
}

func toyClockCmd(hz uint32) []uint16 {
	return []uint16{toySetInterface, toyBDMOld, toyBigEndian, uint16(hz), uint16(hz >> 16)}
}

// Reset resets the MCU and lets it run.
func (t *Toy) Reset() error { _, err := t.cmd(toyTargetReset); return err }

// Run resumes at addr; 0 means the current PC.
func (t *Toy) Run(addr uint32) error {
	if addr != 0 {
		if err := t.writeSysReg(0, addr); err != nil {
			return err
		}
	}
	_, err := t.cmd(toyTargetStart)
	return err
}

// RunWaitFor runs from addr (0: where it stopped) until the target executes
// BGND, at most limit, and returns D0.
func (t *Toy) RunWaitFor(addr uint32, limit time.Duration) (uint32, error) {
	if err := t.Run(addr); err != nil {
		return 0, err
	}
	for deadline := time.Now().Add(limit); ; {
		r, err := t.cmd(toyTargetStatus)
		if err != nil {
			return 0, err
		}
		if len(r) > 0 && r[0] == 1 {
			return t.readReg(0)
		}
		if time.Now().After(deadline) {
			_ = t.Stop()
			return 0, errors.New("target did not return to BDM")
		}
	}
}

// readReg reads a data or address register: 0-7 = D0-D7, 8-15 = A0-A7.
func (t *Toy) readReg(n byte) (uint32, error) {
	r, err := t.cmd(toyReadReg, bdmRDREG+uint16(n), 4)
	if err != nil {
		return 0, err
	}
	if len(r) < 2 {
		return 0, errors.New("bdmtoy: short register reply")
	}
	return uint32(r[0]) | uint32(r[1])<<16, nil
}

func (t *Toy) writeSysReg(reg byte, v uint32) error {
	_, err := t.cmd(toyWriteReg, bdmWSREG+uint16(reg), 4, uint16(v), uint16(v>>16))
	return err
}

func (t *Toy) setFunctionCode(fc uint32) error {
	if err := t.writeSysReg(sysregSFC, fc); err != nil {
		return err
	}
	return t.writeSysReg(sysregDFC, fc)
}

// writeMem writes a byte, word or long word. Unaligned words and long words
// go as bytes, low byte first, as memCmds does for the prep tables.
func (t *Toy) writeMem(addr, val uint32, size int) error {
	if size > 1 && addr%2 != 0 {
		for i := range size {
			if err := t.writeMem(addr+uint32(i), val>>(8*i)&0xff, 1); err != nil {
				return err
			}
		}
		return nil
	}
	c := []uint16{toyWriteMem, uint16(addr), uint16(addr >> 16), uint16(size), 0}
	switch size {
	case 1:
		c = append(c, uint16(val&0xff))
	case 2:
		c = append(c, uint16(val))
	case 4:
		c = append(c, uint16(val), uint16(val>>16))
	default:
		return fmt.Errorf("bad write size %d", size)
	}
	_, err := t.cmd(c...)
	return err
}

// readMem reads a byte (size 1) or a word.
func (t *Toy) readMem(addr uint32, size int) (uint32, error) {
	r, err := t.cmd(toyReadMem, uint16(addr), uint16(addr>>16), uint16(size), 0)
	if err != nil {
		return 0, err
	}
	if len(r) < 1 {
		return 0, errors.New("bdmtoy: short read reply")
	}
	return uint32(r[0]), nil
}

// block reads n bytes at addr (even) with a dump.
func (t *Toy) block(addr, n uint32) ([]byte, error) {
	var b bytes.Buffer
	err := t.dump(addr, n, &b, func(uint32) {})
	return b.Bytes(), err
}

// toyBytes turns little-endian long words, low word first, into target
// (big-endian) byte order.
func toyBytes(w []uint16) []byte {
	b := make([]byte, 2*len(w))
	for i := 0; i+1 < len(w); i += 2 {
		binary.BigEndian.PutUint16(b[2*i:], w[i+1])
		binary.BigEndian.PutUint16(b[2*i+2:], w[i])
	}
	return b
}

// dump streams size bytes from addr to w: one request, then 1 KB frames.
// size is rounded up to whole long words; the extra bytes are dropped.
func (t *Toy) dump(addr, size uint32, w io.Writer, prog progressFn) error {
	n := (size + 3) &^ 3
	if n == 0 {
		return nil
	}
	if err := t.tx([]uint16{toyDumpMem, uint16(addr), uint16(addr >> 16), uint16(n), uint16(n >> 16)}); err != nil {
		return err
	}
	for done := uint32(0); done < size; {
		f, err := t.rx()
		if err != nil {
			return fmt.Errorf("dump at %06X: %w", addr+done, err)
		}
		if len(f) < 3 || f[1] != toyDumpMem {
			return fmt.Errorf("bdmtoy: unexpected frame % 04X", f[:min(len(f), 8)])
		}
		if f[2] != 0 {
			if len(f) >= 5 {
				return fmt.Errorf("dump at %06X: %w", uint32(f[3])|uint32(f[4])<<16, toyErr(f[1], f[2]))
			}
			return toyErr(f[1], f[2])
		}
		if len(f) < 5 || uint32(f[3])|uint32(f[4])<<16 != addr+done {
			return fmt.Errorf("bdmtoy: dump frame out of order at %06X", addr+done)
		}
		b := toyBytes(f[5:])
		b = b[:min(uint32(len(b)), size-done)]
		if _, err := w.Write(b); err != nil {
			return err
		}
		done += uint32(len(b))
		prog(done)
	}
	return nil
}

// load writes data (whole long words) to addr, one fill per frame.
func (t *Toy) load(addr uint32, data []byte, prog progressFn) error {
	if len(data)%4 != 0 {
		return fmt.Errorf("bdmtoy: load of %d bytes, not whole long words", len(data))
	}
	for done := 0; done < len(data); {
		n := min(toyLoad, len(data)-done)
		a := addr + uint32(done)
		c := []uint16{toyFillMem, uint16(a), uint16(a >> 16), uint16(n), uint16(n >> 16)}
		for i := done; i < done+n; i += 4 {
			v := binary.BigEndian.Uint32(data[i:])
			c = append(c, uint16(v), uint16(v>>16))
		}
		if _, err := t.cmd(c...); err != nil {
			return err
		}
		done += n
		prog(uint32(done))
	}
	return nil
}

// =====================
// flash / SRAM operations
// =====================

// enterBDM resets into BDM, applies the ECU's prep table on the slow clock,
// then goes up to the clock the prep allows.
func (t *Toy) enterBDM(e *ECU) error {
	// ponytail: the MCP's CMFI routines are ArduBDM methods; move them onto
	// flasher to get it here.
	if e.FlashType == "cmfi" {
		return fmt.Errorf("%s: only the ardubdm adapter supports its on-chip flash", e.Name)
	}
	if err := t.Restart(); err != nil {
		return fmt.Errorf("target will not enter BDM: %w", err)
	}
	if err := t.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	if err := writeAll(t, e.Prepare...); err != nil {
		return err
	}
	return t.setClock(toyBulkClock(e))
}

// toyBulkClock is the BDM clock for after e's prep: fast once the prep has set
// the CPU clock to 16 MHz or more, keeping DSCLK well under half of it.
func toyBulkClock(e *ECU) uint32 {
	fsys := 0.0
	for _, w := range e.Prepare {
		if w.addr == 0xfffa04 && w.size == 2 {
			fsys = syncrHz(uint16(w.val))
		}
	}
	if fsys >= 16e6 {
		return toyFastClock
	}
	return toySlowClock
}

func (t *Toy) ReadFlash(e *ECU, w io.Writer, prog progressFn) error {
	if err := t.enterBDM(e); err != nil {
		return err
	}
	return t.dump(e.FlashAddr, e.FlashSize, w, prog)
}

func (t *Toy) EraseFlash(e *ECU, prog progressFn) error {
	f, err := (&flasher{cpu32: t}).algo(e)
	if err != nil {
		return err
	}
	if err := t.enterBDM(e); err != nil {
		return err
	}
	return f.erase(e, prog)
}

func (t *Toy) WriteFlash(e *ECU, bin []byte, erase bool, prog progressFn) error {
	f, err := (&flasher{cpu32: t}).algo(e)
	if err != nil {
		return err
	}
	return flashWrite(e, bin, erase, prog, f, t.enterBDM)
}

// enterSRAM: the ECU is reset into BDM and its SRAM reached through the prep
// table's chip selects, so it holds what the ECU left (the reset does not
// clear it). Only the T7's prep maps its SRAM, which also needs the power
// latch at 0xFFF706 that the ECU code would set, as bdmtoy's own T7 init does.
// ponytail: T7 only; add other ECUs' SRAM mapping when one is on the bench.
func (t *Toy) enterSRAM(e *ECU) error {
	if e.Name != "Trionic 7" {
		return fmt.Errorf("bdmtoy: SRAM access is only set up for the Trionic 7")
	}
	if err := t.enterBDM(e); err != nil {
		return err
	}
	return t.writeMem(0xfff706, 0x1000, 2)
}

func (t *Toy) ReadSRAM(e *ECU, w io.Writer, prog progressFn) error {
	if err := t.enterSRAM(e); err != nil {
		return err
	}
	return t.dump(e.SRAMAddr, sramBytes(e), w, prog)
}

func (t *Toy) WriteSRAM(e *ECU, snap []byte, prog progressFn) error {
	if uint32(len(snap)) != sramBytes(e) {
		return fmt.Errorf("snapshot is %d bytes, %s SRAM is %d", len(snap), e.Name, sramBytes(e))
	}
	if err := t.enterSRAM(e); err != nil {
		return err
	}
	return t.load(e.SRAMAddr, snap, prog)
}

func (t *Toy) Identify() (*ECU, string, error) { return identify(t) }
func (t *Toy) Info() (string, error)           { return info(t) }
