// Package main: USB BDM (the original FTDI-based adapter) transport.
//
// Transcribed from caFTDIAdapter in the stock bdmtool's combilib-net.dll. The
// adapter is a plain FTDI UART at 921600 8N1 with RTS/CTS, speaking a line
// protocol: <group><code><ASCII-hex args>\r, answered with <ASCII-hex reply>
// plus a terminator byte -- 0x0D ok, 0x07 error. All numbers are uppercase
// hex, most significant nibble first. 0x1B breaks off a streaming operation.
//
// The chip is driven over libusb directly rather than through the kernel's
// ftdi_sio tty, because the streaming flash dump needs RTS/CTS and the tty
// API Go can reach does not offer it.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gotmc/libusb/v2"
)

const (
	ftdiVID = 0x0403

	// Untyped: gotmc's transfer methods take an unexported endpoint type.
	ftdiIn  = 0x81
	ftdiOut = 0x02

	// FTDI vendor requests (bmRequestType 0x40).
	ftdiReqReset      = 0x00
	ftdiReqSetFlow    = 0x02
	ftdiReqSetBaud    = 0x03
	ftdiReqSetData    = 0x04
	ftdiReqSetLatency = 0x09

	// Command groups and codes.
	grpAdapter = 'a'
	grpFlash   = 'f'
	grpMCU     = 'c'
	grpMemory  = 'm'
	grpRegs    = 'r'

	cmdVersion  = 'v'
	cmdPinState = 's'

	cmdReadFlashF  = 'd'
	cmdEraseFlashF = 'E'
	cmdWriteFlashF = 'w'
	cmdGetVerify   = 'v'
	cmdSetVerify   = 'V'

	cmdStopMCU    = 'S'
	cmdResetMCU   = 'R'
	cmdRestartMCU = 's'
	// The stock tool sends 'b' for both run-from-address and single step; the
	// firmware distinguishes them by whether an address follows.
	cmdRunMCU = 'b'

	cmdReadLong  = 'l'
	cmdDumpLong  = 'c'
	cmdWriteLong = 'L'
	cmdWriteWord = 'W'
	cmdWriteByte = 'B'

	cmdWriteSysRegF = 'W'

	bdmTimeout      = 1000   // ms, caFTDIAdapter::default_timeout
	bdmEraseTimeout = 120000 // ms

	termOK  = 0x0D
	termErr = 0x07
	termBrk = 0x1B
)

// ftdiPIDs are the FTDI product ids the original tool would have found; it
// simply opened the first FTDI device on the machine.
var ftdiPIDs = []uint16{0x6001, 0x6010, 0x6011, 0x6014, 0x6015}

// BDM is a USB BDM adapter.
type BDM struct {
	ctx  *libusb.Context
	dev  *libusb.Device
	h    *libusb.DeviceHandle
	tmo  int    // command timeout, ms
	buf  []byte // raw USB scratch, holds FTDI status bytes
	pend []byte // payload read but not yet consumed
}

// OpenBDM connects to a USB BDM adapter.
func OpenBDM() (*BDM, error) {
	ctx, err := libusb.NewContext()
	if err != nil {
		return nil, fmt.Errorf("libusb init: %w", err)
	}
	var (
		dev *libusb.Device
		h   *libusb.DeviceHandle
	)
	for _, pid := range ftdiPIDs {
		if dev, h, err = ctx.OpenDeviceWithVendorProduct(ftdiVID, pid); err == nil {
			break
		}
	}
	if h == nil {
		ctx.Close()
		return nil, errors.New("USB BDM not found: no FTDI device present")
	}
	b := &BDM{ctx: ctx, dev: dev, h: h, tmo: bdmTimeout, buf: make([]byte, 512)}

	_ = h.SetAutoDetachKernelDriver(true)
	if err := h.ClaimInterface(0); err != nil {
		b.Close()
		return nil, fmt.Errorf("claim interface 0: %w", err)
	}
	if err := b.setup(); err != nil {
		b.Close()
		return nil, err
	}
	if _, _, err := b.Version(); err != nil {
		b.Close()
		return nil, fmt.Errorf("adapter did not respond: %w", err)
	}
	return b, nil
}

func (b *BDM) Close() {
	if b.h != nil {
		_ = b.h.ReleaseInterface(0)
		_ = b.h.Close()
	}
	if b.dev != nil {
		b.dev.Close()
	}
	if b.ctx != nil {
		_ = b.ctx.Close()
	}
}

// =====================
// FTDI transport
// =====================

func (b *BDM) ctrl(req byte, value, index uint16) error {
	_, err := b.h.ControlTransfer(0x40, req, value, index, nil, 0, 1000)
	return err
}

// setup mirrors caFTDIAdapter::Open: 921600 8N1, RTS/CTS, 2 ms latency.
//
// ponytail: the baud divisor assumes an FT232B/R-class chip (3 MHz base
// clock); 3.25 gives 923 kBaud, 0.16% off, well inside UART tolerance. An
// H-series chip would need a 12 MHz base -- add a chip-type check if one shows
// up.
func (b *BDM) setup() error {
	const divisor = 3 | (2 << 14) // 3.25 -> integer 3, fraction code 2 (0.25)
	steps := []struct {
		req          byte
		value, index uint16
	}{
		{ftdiReqReset, 0, 1},         // reset chip
		{ftdiReqSetBaud, divisor, 0}, // 921600 baud
		{ftdiReqSetData, 0x0008, 1},  // 8 data bits, no parity, 1 stop bit
		{ftdiReqSetFlow, 0, 0x0101},  // RTS/CTS on interface A
		{ftdiReqSetLatency, 2, 1},    // 2 ms latency timer
		{ftdiReqReset, 1, 1},         // purge RX
		{ftdiReqReset, 2, 1},         // purge TX
	}
	for _, s := range steps {
		if err := b.ctrl(s.req, s.value, s.index); err != nil {
			return fmt.Errorf("FTDI setup (request %02X): %w", s.req, err)
		}
	}
	return nil
}

func (b *BDM) write(p []byte) error {
	_, err := b.h.BulkTransferOut(ftdiOut, p, b.tmo)
	return err
}

// fill does one bulk IN and strips the two modem-status bytes the chip puts at
// the head of every 64 byte packet. Returns the payload, which is often empty:
// an idle adapter still answers the latency-timer poll with status only.
func (b *BDM) fill() ([]byte, error) {
	n, err := b.h.BulkTransfer(ftdiIn, b.buf, len(b.buf), 200)
	if err != nil && n == 0 {
		return nil, err
	}
	return stripStatus(b.buf[:n]), nil
}

// stripStatus drops the two modem-status bytes at the head of every 64 byte
// packet the chip returns.
func stripStatus(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i += 64 {
		end := min(i+64, len(raw))
		if end-i > 2 {
			out = append(out, raw[i+2:end]...)
		}
	}
	return out
}

// read blocks for exactly n payload bytes.
func (b *BDM) read(n int) ([]byte, error) {
	deadline := time.Now().Add(time.Duration(b.tmo) * time.Millisecond)
	for len(b.pend) < n {
		p, err := b.fill()
		if err != nil && len(p) == 0 && time.Now().After(deadline) {
			return nil, fmt.Errorf("no response from adapter: %w", err)
		}
		if len(p) == 0 {
			if time.Now().After(deadline) {
				return nil, errors.New("no response from adapter")
			}
			continue
		}
		b.pend = append(b.pend, p...)
	}
	out := b.pend[:n]
	b.pend = b.pend[n:]
	return out, nil
}

// pending reports whether the adapter has sent anything we have not read --
// during a flash write that means it aborted with an error.
func (b *BDM) pending() bool {
	if len(b.pend) > 0 {
		return true
	}
	p, _ := b.fill()
	b.pend = append(b.pend, p...)
	return len(b.pend) > 0
}

func (b *BDM) drain() {
	old := b.tmo
	b.tmo = 50
	for range 20 {
		p, err := b.fill()
		if err != nil && len(p) == 0 {
			break
		}
	}
	b.pend = nil
	b.tmo = old
}

// =====================
// command layer
// =====================

// frame builds one command: group, code, ASCII args, CR.
func frame(group, code byte, args string) []byte {
	return append([]byte{group, code}, append([]byte(args), termOK)...)
}

func (b *BDM) send(group, code byte, args string) error {
	return b.write(frame(group, code, args))
}

// response reads n reply characters plus the terminator.
func (b *BDM) response(n int) ([]byte, error) {
	r, err := b.read(n + 1)
	if err != nil {
		return nil, err
	}
	if r[0] == termErr || r[n] == termErr {
		return nil, errors.New("command failed")
	}
	return r[:n], nil
}

// cmd sends a command and returns its n reply characters.
func (b *BDM) cmd(group, code byte, args string, n int) ([]byte, error) {
	if err := b.send(group, code, args); err != nil {
		return nil, err
	}
	return b.response(n)
}

// sendBreak aborts a streaming operation.
func (b *BDM) sendBreak() {
	_ = b.write([]byte{termBrk})
	b.drain()
}

func hex32(v uint32) string { return fmt.Sprintf("%08X", v) }

// unhex32 decodes one 8-character reply into a long word.
func unhex32(s []byte) (uint32, error) {
	var raw [4]byte
	if _, err := hex.Decode(raw[:], s); err != nil {
		return 0, fmt.Errorf("bad reply %q: %w", s, err)
	}
	return uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]), nil
}

// =====================
// board
// =====================

// Version returns the adapter firmware version.
func (b *BDM) Version() (major, minor byte, err error) {
	r, err := b.cmd(grpAdapter, cmdVersion, "", 4)
	if err != nil {
		return 0, 0, err
	}
	var v [2]byte
	if _, err := hex.Decode(v[:], r); err != nil {
		return 0, 0, fmt.Errorf("bad version reply %q", r)
	}
	return v[0], v[1], nil
}

// PinState returns the adapter's BDM pin states.
func (b *BDM) PinState() (byte, error) {
	r, err := b.cmd(grpAdapter, cmdPinState, "", 2)
	if err != nil {
		return 0, err
	}
	var v [1]byte
	_, err = hex.Decode(v[:], r)
	return v[0], err
}

// SetVerifyFlash turns the adapter's own write verification on or off.
func (b *BDM) SetVerifyFlash(on bool) error {
	v := "00"
	if on {
		v = "01"
	}
	_, err := b.cmd(grpFlash, cmdSetVerify, v, 0)
	return err
}

// =====================
// BDM primitives
// =====================

func (b *BDM) Stop() error  { _, err := b.cmd(grpMCU, cmdStopMCU, "", 0); return err }
func (b *BDM) Reset() error { _, err := b.cmd(grpMCU, cmdResetMCU, "", 0); return err }

func (b *BDM) Run(addr uint32) error {
	_, err := b.cmd(grpMCU, cmdRunMCU, hex32(addr), 0)
	return err
}

func (b *BDM) writeSysReg(reg byte, v uint32) error {
	_, err := b.cmd(grpRegs, cmdWriteSysRegF, fmt.Sprintf("%02X%s", reg, hex32(v)), 0)
	return err
}

func (b *BDM) setFunctionCode(fc uint32) error {
	if err := b.writeSysReg(sysregSFC, fc); err != nil {
		return err
	}
	return b.writeSysReg(sysregDFC, fc)
}

type memCmd struct {
	code byte
	args string
}

// memCmds builds the memory-write commands for a size-byte (1, 2 or 4) write
// at addr. Unaligned multi-byte writes are split into byte writes, low byte
// first -- same rule as the CombiAdapter path, and the T5/T7 prepare tables
// need it.
func memCmds(addr, val uint32, size int) ([]memCmd, error) {
	if size > 1 && addr%2 != 0 {
		out := make([]memCmd, size)
		for i := range size {
			c, _ := memCmds(addr+uint32(i), val>>(8*i)&0xff, 1)
			out[i] = c[0]
		}
		return out, nil
	}
	switch size {
	case 1:
		return []memCmd{{cmdWriteByte, fmt.Sprintf("%s%02X", hex32(addr), val&0xff)}}, nil
	case 2:
		return []memCmd{{cmdWriteWord, fmt.Sprintf("%s%04X", hex32(addr), val&0xffff)}}, nil
	case 4:
		return []memCmd{{cmdWriteLong, hex32(addr) + hex32(val)}}, nil
	}
	return nil, fmt.Errorf("bad write size %d", size)
}

func (b *BDM) writeMem(addr, val uint32, size int) error {
	cmds, err := memCmds(addr, val, size)
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if _, err := b.cmd(grpMemory, c.code, c.args, 0); err != nil {
			return err
		}
	}
	return nil
}

// readMem32 reads a long word. setAddr=false continues from the last address.
func (b *BDM) readMem32(addr uint32, setAddr bool) (uint32, error) {
	var (
		r   []byte
		err error
	)
	if setAddr {
		r, err = b.cmd(grpMemory, cmdReadLong, hex32(addr), 8)
	} else {
		r, err = b.cmd(grpMemory, cmdDumpLong, "", 8)
	}
	if err != nil {
		return 0, err
	}
	return unhex32(r)
}

// enterBDM halts the MCU, selects supervisor data space and applies the ECU's
// chip-select / watchdog setup so flash is reachable.
func (b *BDM) enterBDM(e *ECU) error {
	// The MCP's on-chip flash needs a CPU32 driver uploaded to its DPTRAM;
	// only ardubdm (ardubdm.go) drives it.
	if e.FlashType == "cmfi" {
		return fmt.Errorf("%s: only the ardubdm adapter supports its on-chip flash", e.Name)
	}
	if err := b.Stop(); err != nil {
		return err
	}
	if err := b.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	for _, w := range e.Prepare {
		if w.size == 0 {
			time.Sleep(time.Duration(w.val) * time.Millisecond)
			continue
		}
		if err := b.writeMem(w.addr, w.val, w.size); err != nil {
			return err
		}
	}
	return nil
}

// =====================
// flash / SRAM operations
// =====================

// ReadFlash dumps the ECU flash to w. The adapter streams long words back
// until the end address is reached; only the first one is requested.
func (b *BDM) ReadFlash(e *ECU, w io.Writer, prog progressFn) error {
	if err := b.enterBDM(e); err != nil {
		return err
	}
	args := hex32(e.FlashAddr) + hex32(e.FlashAddr+e.FlashSize)
	var raw [4]byte
	for done := uint32(0); done < e.FlashSize; done += 4 {
		var (
			r   []byte
			err error
		)
		if done == 0 {
			r, err = b.cmd(grpFlash, cmdReadFlashF, args, 8)
		} else {
			r, err = b.response(8)
		}
		if err == nil {
			_, err = hex.Decode(raw[:], r)
		}
		if err == nil {
			_, err = w.Write(raw[:])
		}
		if err != nil {
			b.sendBreak()
			return err
		}
		prog(done + 4)
	}
	return nil
}

// EraseFlash erases the whole ECU flash.
func (b *BDM) EraseFlash(e *ECU, prog progressFn) error {
	if err := b.enterBDM(e); err != nil {
		return err
	}
	return b.eraseFlash(e, prog)
}

func (b *BDM) eraseFlash(e *ECU, prog progressFn) error {
	args := e.FlashType + hex32(e.FlashAddr) + hex32(e.FlashAddr+e.FlashSize)
	if err := b.send(grpFlash, cmdEraseFlashF, args); err != nil {
		return err
	}
	b.tmo = bdmEraseTimeout
	defer func() { b.tmo = bdmTimeout }()

	if _, err := b.response(0); err != nil {
		b.sendBreak()
		return err
	}
	// 28Fxxx chips report progress: one empty reply per word erased.
	if e.EraseFeedback {
		for done := uint32(0); done < e.FlashSize; done += 2 {
			if _, err := b.response(0); err != nil {
				b.sendBreak()
				return err
			}
			prog(done + 2)
		}
	}
	prog(e.FlashSize)
	return nil
}

// WriteFlash programs bin into the ECU flash. bin must be exactly FlashSize.
// After the write command the adapter takes a bare stream of hex long words;
// it only talks back if something went wrong.
func (b *BDM) WriteFlash(e *ECU, bin []byte, erase bool, prog progressFn) error {
	if uint32(len(bin)) != e.FlashSize {
		return fmt.Errorf("file is %d bytes, %s flash is %d", len(bin), e.Name, e.FlashSize)
	}
	if err := b.enterBDM(e); err != nil {
		return err
	}
	if erase {
		if err := b.eraseFlash(e, prog); err != nil {
			return err
		}
	}
	if _, err := b.cmd(grpFlash, cmdWriteFlashF, e.FlashType+hex32(e.FlashAddr), 0); err != nil {
		return err
	}

	frame := make([]byte, 9)
	frame[8] = termOK
	for done := uint32(0); done < e.FlashSize; done += 4 {
		hex.Encode(frame[:8], bin[done:done+4])
		upper(frame[:8])
		if err := b.write(frame); err != nil {
			b.sendBreak()
			return err
		}
		// ponytail: the stock tool polls the adapter after every long word;
		// over libusb that costs a round trip, so check per block instead. A
		// failure is caught up to 256 bytes later.
		if done%256 == 0 && b.pending() {
			b.sendBreak()
			return errors.New("flashing ended prematurely")
		}
		prog(done + 4)
	}
	if err := b.write([]byte{termBrk}); err != nil {
		return err
	}
	_, err := b.response(0)
	return err
}

// upper uppercases hex digits in place; the adapter's parser wants A-F.
func upper(p []byte) {
	for i, c := range p {
		if c >= 'a' && c <= 'f' {
			p[i] = c - 32
		}
	}
}

// enterSRAM halts the MCU in supervisor data space; SRAM is internal, so no
// chip-select setup is needed.
func (b *BDM) enterSRAM(e *ECU) error {
	if e.SRAMSize == 0 {
		return fmt.Errorf("%s has no SRAM defined", e.Name)
	}
	if err := b.Stop(); err != nil {
		return err
	}
	if err := b.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	if err := b.Run(0); err != nil {
		return err
	}
	return b.Stop()
}

// ReadSRAM dumps the ECU SRAM to w.
func (b *BDM) ReadSRAM(e *ECU, w io.Writer, prog progressFn) error {
	if err := b.enterSRAM(e); err != nil {
		return err
	}
	var buf [4]byte
	for done := uint32(0); done < sramBytes(e); done += 4 {
		v, err := b.readMem32(e.SRAMAddr+done, done == 0)
		if err != nil {
			return err
		}
		buf[0], buf[1], buf[2], buf[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
		if _, err := w.Write(buf[:]); err != nil {
			return err
		}
		prog(done + 4)
	}
	return nil
}

// WriteSRAM restores an SRAM snapshot, one write-memory round trip per long
// word -- the adapter has no bulk SRAM command.
func (b *BDM) WriteSRAM(e *ECU, snap []byte, prog progressFn) error {
	if uint32(len(snap)) != sramBytes(e) {
		return fmt.Errorf("snapshot is %d bytes, %s SRAM is %d", len(snap), e.Name, sramBytes(e))
	}
	if err := b.enterSRAM(e); err != nil {
		return err
	}
	for done := uint32(0); done < sramBytes(e); done += 4 {
		v := uint32(snap[done])<<24 | uint32(snap[done+1])<<16 |
			uint32(snap[done+2])<<8 | uint32(snap[done+3])
		if err := b.writeMem(e.SRAMAddr+done, v, 4); err != nil {
			return err
		}
		prog(done + 4)
	}
	return nil
}
