// Package main: ardubdm (ATmega328PB bit-banged CPU32 BDM) transport over a
// serial port, 1 Mbaud 8N1.
//
// Same ASCII line protocol as the USB BDM (usbbdm.go) with two differences:
// text replies carry a CRLF before the flag byte, and there is no flash group.
// Erase and program are therefore driven from here through plain memory
// writes, transcribed from Just4Trionic's bdmtrionic.cpp (Sophie Dexter).
// Commands are pipelined, one window of them in flight ahead of the replies,
// because a USB-serial round trip costs milliseconds; the window keeps the
// firmware's 1 KB receive buffer from overflowing when BDM lags the link.
package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const (
	arduBaud = 1000000
	// ponytail: a Nano auto-resets on DTR when the port opens; optiboot needs
	// ~1.5 s before the sketch answers. Bump if the first connect fails.
	arduConnect = 3 * time.Second
	arduTimeout = 2 * time.Second
	arduBlock   = 4096 // bytes per block dump (mD)
	arduLoad    = 1024 // bytes per block write (mS): the firmware's RX buffer
	arduWindow  = 384  // command bytes in flight; two windows fit that buffer
	arduChunk   = 256  // flash bytes per pipelined batch (28F010 path)

	// CPU32 flash driver (ardubdm/driver/am29prog.s): parameter block offset,
	// its size, and the data per run so params+data fit one block write.
	am29DrvParams = 0x60
	am29DrvHeader = 20
	am29DrvData   = arduLoad - am29DrvHeader
	am29SWSR      = 0xFFFA27 // SIM software watchdog service register

	cmdBlockRead  = 'D'
	cmdBlockWrite = 'S'
	cmdReadWord   = 'w'
	cmdReadByte   = 'b'

	am29EraseTimeout = 200 * time.Second // T8 worst case, per Just4Trionic
	am29EraseTypical = 6 * time.Second   // T7 29F400 chip erase on the bench, for the progress bar

	// CPU32 28F010 driver (ardubdm/driver/am28prog.s): parameter block offset,
	// header size, data per run, and its dbra delay counts. The counts assume
	// the 16.78 MHz the T5 prep sets and ~6 clocks per dbra iteration; the
	// driver's timing mode measures them before any erase.
	am28DrvParams = 0x180
	am28DrvHeader = 24
	am28DrvData   = arduLoad - am28DrvHeader
	am28Delay10us = 28
	am28Delay6us  = 17
	am28Delay10ms = 28000
	am28ModeProg  = 1
	am28ModeErase = 2
	am28ModeTime  = 3
)

// ArduBDM is an ardubdm adapter on a serial port.
type ArduBDM struct {
	p   io.ReadWriteCloser
	tmo time.Duration
	// 28F driver delay counts, calibrated by am28Setup against wall time.
	d10us, d6us, d10ms uint16
}

// arduPorts lists the USB serial ports the adapter could be on.
func arduPorts() []string {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range ports {
		if p.IsUSB {
			out = append(out, p.Name)
		}
	}
	return out
}

// OpenArdu connects to an ardubdm on the given port.
func OpenArdu(name string) (*ArduBDM, error) {
	p, err := serial.Open(name, &serial.Mode{
		BaudRate: arduBaud,
		InitialStatusBits: &serial.ModemOutputBits{
			DTR: false,
			RTS: true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	_ = p.SetReadTimeout(100 * time.Millisecond)
	a := &ArduBDM{p: p}
	if err := a.handshake(); err != nil {
		a.Close()
		return nil, fmt.Errorf("ardubdm on %s did not respond: %w", name, err)
	}
	return a, nil
}

// handshake polls for the firmware instead of sleeping through a possible
// bootloader: a board that did not reset answers at once, one that did
// answers a later try. Boot noise is drained before each attempt.
func (a *ArduBDM) handshake() error {
	a.tmo = 300 * time.Millisecond
	defer func() { a.tmo = arduTimeout }()
	deadline := time.Now().Add(arduConnect)
	for {
		a.drain()
		_, _, err := a.Version()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond) // ponytail: bounds the retry rate if the port errors instead of timing out
	}
}

func (a *ArduBDM) Close() { _ = a.p.Close() }

// =====================
// transport
// =====================

func (a *ArduBDM) write(p []byte) error {
	for len(p) > 0 {
		n, err := a.p.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// read blocks for exactly n bytes.
func (a *ArduBDM) read(n int) ([]byte, error) {
	buf := make([]byte, n)
	deadline := time.Now().Add(a.tmo)
	for got := 0; got < n; {
		k, err := a.p.Read(buf[got:])
		if err != nil {
			return nil, err
		}
		if k == 0 && time.Now().After(deadline) {
			return nil, errors.New("no response from adapter")
		}
		got += k
	}
	return buf, nil
}

// drain discards input until the adapter has been quiet for a read timeout.
func (a *ArduBDM) drain() {
	buf := make([]byte, 512)
	for {
		n, err := a.p.Read(buf)
		if n == 0 || err != nil {
			return
		}
	}
}

// =====================
// command layer
// =====================

// req is one pipelined command and the number of reply characters it returns.
type req struct {
	cmd []byte
	n   int
}

func wordW(addr uint32, v uint16) req {
	return req{frame(grpMemory, cmdWriteWord, fmt.Sprintf("%s%04X", hex32(addr), v)), 0}
}
func byteW(addr uint32, v byte) req {
	return req{frame(grpMemory, cmdWriteByte, fmt.Sprintf("%s%02X", hex32(addr), v)), 0}
}
func wordR(addr uint32) req { return req{frame(grpMemory, cmdReadWord, hex32(addr)), 4} }
func byteR(addr uint32) req { return req{frame(grpMemory, cmdReadByte, hex32(addr)), 2} }

// response reads one reply: the flag byte alone, or n hex characters, CRLF and
// the flag byte. A failed command sends only the error flag.
func (a *ArduBDM) response(n int) ([]byte, error) {
	r, err := a.read(1)
	if err != nil {
		return nil, err
	}
	if r[0] == termErr {
		return nil, errors.New("command failed")
	}
	if r[0] == 'e' { // "err <reason>\r\n" then the error flag
		line := r
		for !bytes.HasSuffix(line, []byte("\r\n")) {
			b, err := a.read(1)
			if err != nil {
				return nil, err
			}
			line = append(line, b...)
		}
		a.read(1)
		return nil, errors.New(string(bytes.TrimSpace(line)))
	}
	if n == 0 {
		if r[0] != termOK {
			return nil, fmt.Errorf("bad reply byte %#02x", r[0])
		}
		return nil, nil
	}
	rest, err := a.read(n + 2)
	if err != nil {
		return nil, err
	}
	r = append(r, rest...)
	if r[n+2] != termOK {
		return nil, fmt.Errorf("bad reply %q", r)
	}
	return r[:n], nil
}

// batch runs the commands pipelined: one window of commands is written
// ahead while the previous window's replies are read, so the firmware never
// holds more than two windows unread.
func (a *ArduBDM) batch(reqs []req) ([][]byte, error) {
	var wins [][]req
	for size, start, i := 0, 0, 0; i <= len(reqs); i++ {
		if i == len(reqs) || size+len(reqs[i].cmd) > arduWindow && i > start {
			wins = append(wins, reqs[start:i])
			start, size = i, 0
		}
		if i < len(reqs) {
			size += len(reqs[i].cmd)
		}
	}
	send := func(w []req) error {
		var buf []byte
		for _, r := range w {
			buf = append(buf, r.cmd...)
		}
		return a.write(buf)
	}
	out := make([][]byte, 0, len(reqs))
	for i := range wins {
		if i == 0 {
			if err := send(wins[0]); err != nil {
				return nil, err
			}
		}
		if i+1 < len(wins) {
			if err := send(wins[i+1]); err != nil {
				return nil, err
			}
		}
		for _, r := range wins[i] {
			v, err := a.response(r.n)
			if err != nil {
				a.drain()
				return nil, fmt.Errorf("%q: %w", bytes.TrimSuffix(r.cmd, []byte{termOK}), err)
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// cmd sends one command and returns its n reply characters.
func (a *ArduBDM) cmd(group, code byte, args string, n int) ([]byte, error) {
	r, err := a.batch([]req{{frame(group, code, args), n}})
	if err != nil {
		return nil, err
	}
	return r[0], nil
}

func unhex16(s []byte) uint16 {
	v, _ := strconv.ParseUint(string(s), 16, 16)
	return uint16(v)
}

// block reads n bytes at addr with one block dump.
func (a *ArduBDM) block(addr, n uint32) ([]byte, error) {
	if err := a.write(frame(grpMemory, cmdBlockRead, fmt.Sprintf("%s%04X", hex32(addr), n))); err != nil {
		return nil, err
	}
	data, err := a.read(int(n) + 1)
	if err != nil {
		return nil, err
	}
	if data[n] != termOK {
		a.drain()
		return nil, errors.New("block read failed")
	}
	return data[:n], nil
}

// debugLog receives adapter diagnostics; the UI points it at its log.
var debugLog = func(string, ...any) {}

// statusLog sets the UI's status line for a phase of a longer operation.
var statusLog = func(string) {}

// dump block-reads size bytes from addr to w.
func (a *ArduBDM) dump(addr, size uint32, w io.Writer, prog progressFn) error {
	for done := uint32(0); done < size; {
		n := min(arduBlock, size-done)
		data, err := a.block(addr+done, n)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		done += n
		prog(done)
	}
	return nil
}

// load block-writes data to addr.
func (a *ArduBDM) load(addr uint32, data []byte, prog progressFn) error {
	for done := 0; done < len(data); {
		n := min(arduLoad, len(data)-done)
		f := frame(grpMemory, cmdBlockWrite, fmt.Sprintf("%s%04X", hex32(addr+uint32(done)), n))
		if err := a.write(append(f, data[done:done+n]...)); err != nil {
			return err
		}
		if _, err := a.response(0); err != nil {
			a.drain()
			return err
		}
		done += n
		prog(uint32(done))
	}
	return nil
}

// =====================
// board / BDM primitives
// =====================

func (a *ArduBDM) Version() (major, minor byte, err error) {
	r, err := a.cmd(grpAdapter, cmdVersion, "", 4)
	if err != nil {
		return 0, 0, err
	}
	return byte(unhex16(r) >> 8), byte(unhex16(r)), nil
}

func (a *ArduBDM) Stop() error    { _, err := a.cmd(grpMCU, cmdStopMCU, "", 0); return err }
func (a *ArduBDM) Restart() error { _, err := a.cmd(grpMCU, 's', "", 0); return err }
func (a *ArduBDM) Reset() error   { _, err := a.cmd(grpMCU, cmdResetMCU, "", 0); return err }

// Run resumes from addr; 0 means the current PC.
func (a *ArduBDM) Run(addr uint32) error {
	_, err := a.cmd(grpMCU, 'r', hex32(addr), 0)
	return err
}

// RunWait runs from addr and waits for the target to drop back into BDM
// (BGND instruction); returns D0.
func (a *ArduBDM) RunWait(addr uint32) (uint32, error) {
	return a.RunWaitFor(addr, 2*time.Second)
}

// RunWaitFor is RunWait with an explicit limit (the adapter's default is 2 s).
func (a *ArduBDM) RunWaitFor(addr uint32, limit time.Duration) (uint32, error) {
	ms := min(limit.Milliseconds(), 0xFFFF)
	tmo := a.tmo
	a.tmo = limit + 2*time.Second
	defer func() { a.tmo = tmo }()
	r, err := a.cmd(grpMCU, 'w', fmt.Sprintf("%s%04X", hex32(addr), ms), 8)
	if err != nil {
		return 0, err
	}
	v, _ := strconv.ParseUint(string(r), 16, 32)
	return uint32(v), nil
}

func (a *ArduBDM) readAReg(n byte) (uint32, error) {
	r, err := a.cmd(grpRegs, 'a', fmt.Sprintf("%02X", 8+n), 8)
	if err != nil {
		return 0, err
	}
	v, _ := strconv.ParseUint(string(r), 16, 32)
	return uint32(v), nil
}

func (a *ArduBDM) writeSysReg(reg byte, v uint32) error {
	_, err := a.cmd(grpRegs, cmdWriteSysRegF, fmt.Sprintf("%02X%s", reg, hex32(v)), 0)
	return err
}

func (a *ArduBDM) setFunctionCode(fc uint32) error {
	if err := a.writeSysReg(sysregSFC, fc); err != nil {
		return err
	}
	return a.writeSysReg(sysregDFC, fc)
}

func (a *ArduBDM) writeMem(addr, val uint32, size int) error {
	cmds, err := memCmds(addr, val, size)
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if _, err := a.cmd(grpMemory, c.code, c.args, 0); err != nil {
			return err
		}
	}
	return nil
}

// enterBDM halts the MCU, selects supervisor data space and applies the ECU's
// chip-select / watchdog setup so flash is reachable.
func (a *ArduBDM) enterBDM(e *ECU) error {
	// Reset into BDM (BKPT held low across reset), as Just4Trionic and bdmtoy
	// do: the ECU code never runs, so Prepare below owns the whole setup,
	// including the write-once SYPCR watchdog register. Stop (halt a running
	// ECU) is only the fallback for a board whose BDM does not enable at reset.
	if err := a.Restart(); err != nil {
		if err := a.Stop(); err != nil {
			return fmt.Errorf("target will not enter BDM: %w", err)
		}
	}
	if err := a.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	for _, w := range e.Prepare {
		if err := a.writeMem(w.addr, w.val, w.size); err != nil {
			return err
		}
	}
	return nil
}

func (a *ArduBDM) enterSRAM(e *ECU) error {
	if e.SRAMSize == 0 {
		return fmt.Errorf("%s has no SRAM defined", e.Name)
	}
	if err := a.Stop(); err != nil {
		return err
	}
	if err := a.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	if err := a.Run(0); err != nil {
		return err
	}
	return a.Stop()
}

// =====================
// flash / SRAM operations
// =====================

func (a *ArduBDM) ReadFlash(e *ECU, w io.Writer, prog progressFn) error {
	if err := a.enterBDM(e); err != nil {
		return err
	}
	return a.dump(e.FlashAddr, e.FlashSize, w, prog)
}

func (a *ArduBDM) ReadSRAM(e *ECU, w io.Writer, prog progressFn) error {
	if err := a.enterSRAM(e); err != nil {
		return err
	}
	return a.dump(e.SRAMAddr, sramBytes(e), w, prog)
}

func (a *ArduBDM) WriteSRAM(e *ECU, snap []byte, prog progressFn) error {
	if uint32(len(snap)) != sramBytes(e) {
		return fmt.Errorf("snapshot is %d bytes, %s SRAM is %d", len(snap), e.Name, sramBytes(e))
	}
	if err := a.enterSRAM(e); err != nil {
		return err
	}
	return a.load(e.SRAMAddr, snap, prog)
}

type flashAlgo struct {
	erase   func(*ECU, progressFn) error
	program func(*ECU, []byte, progressFn) error
}

// algo picks the host-side erase/program routines for the ECU's flash chips.
func (a *ArduBDM) algo(e *ECU) (flashAlgo, error) {
	switch e.FlashType {
	case "29f010", "29f400":
		return flashAlgo{a.eraseAM29, a.programAM29}, nil
	case "28f010":
		return flashAlgo{a.eraseAM28, a.programAM28}, nil
	}
	// ponytail: Volvo CEM's 28F400 needs the Intel block-erase/status flow,
	// which Just4Trionic never had either. Add when someone has one to test.
	return flashAlgo{}, fmt.Errorf("%s flash: erase/write not supported on ardubdm", e.FlashType)
}

func (a *ArduBDM) EraseFlash(e *ECU, prog progressFn) error {
	f, err := a.algo(e)
	if err != nil {
		return err
	}
	if err := a.enterBDM(e); err != nil {
		return err
	}
	return f.erase(e, prog)
}

func (a *ArduBDM) WriteFlash(e *ECU, bin []byte, erase bool, prog progressFn) error {
	if uint32(len(bin)) != e.FlashSize {
		return fmt.Errorf("file is %d bytes, %s flash is %d", len(bin), e.Name, e.FlashSize)
	}
	f, err := a.algo(e)
	if err != nil {
		return err
	}
	if err := a.enterBDM(e); err != nil {
		return err
	}
	if !erase {
		return f.program(e, bin, prog)
	}
	// One bar for both phases: erase fills the first half, programming the second.
	statusLog("Erasing flash...")
	if err := f.erase(e, func(d uint32) { prog(d / 2) }); err != nil {
		return err
	}
	statusLog("Writing flash...")
	return f.program(e, bin, func(d uint32) { prog(e.FlashSize/2 + d/2) })
}

// verify compares a chunk of flash against want.
func (a *ArduBDM) verify(addr uint32, want []byte) error {
	got, err := a.block(addr, uint32(len(want)))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("flash verify failed at %08X", addr)
	}
	return nil
}

// ---- AMD 29Fxxx (T7, T8, retrofitted T5.5) ----

// am29 is the three-write command sequence: 0x5555/0x2AAA word addresses
// doubled for the 16 bit bus, the command byte on both lanes so T5.5's paired
// 29F010 chips both see it. Addresses are absolute -- the chips answer at 0
// on every ECU that carries them (T5.5 mirrors its flash there).
func am29(cmd uint16) []req {
	return []req{wordW(0xAAAA, 0xAAAA), wordW(0x5554, 0x5555), wordW(0xAAAA, cmd)}
}

func (a *ArduBDM) resetAM29() error {
	_, err := a.batch(am29(0xF0F0))
	return err
}

// eraseAM29 issues chip erase and polls the first word: data polling keeps it
// off 0xFFFF until the chip is done.
func (a *ArduBDM) eraseAM29(e *ECU, prog progressFn) error {
	if err := a.resetAM29(); err != nil {
		return err
	}
	if _, err := a.batch(append(am29(0x8080), am29(0x1010)...)); err != nil {
		a.resetAM29()
		return err
	}
	// Done when word 0 has read 0xFFFF three polls in a row and a spot check
	// of the start, middle and end of the array is all 0xFF: a single 0xFFFF
	// can be stale, and a chip mid-erase answers with toggling status bits.
	deadline := time.Now().Add(am29EraseTimeout)
	start := time.Now()
	for clean := 0; ; {
		r, err := a.cmd(grpMemory, cmdReadWord, hex32(e.FlashAddr), 4)
		if err != nil {
			a.resetAM29()
			return fmt.Errorf("erase poll: %w", err)
		}
		if unhex16(r) == 0xFFFF {
			clean++
		} else {
			clean = 0
		}
		// The chip gives no progress, so show elapsed against a typical erase, capped short of done.
		if p := uint64(e.FlashSize) * uint64(time.Since(start)) / uint64(am29EraseTypical); p < uint64(e.FlashSize)-1 {
			prog(uint32(p))
		}
		if clean >= 3 && a.blankAt(e.FlashAddr) && a.blankAt(e.FlashAddr+e.FlashSize/2) && a.blankAt(e.FlashAddr+e.FlashSize-256) {
			break
		}
		if time.Now().After(deadline) {
			a.resetAM29()
			return errors.New("flash erase timed out")
		}
		time.Sleep(100 * time.Millisecond)
	}
	prog(e.FlashSize)
	return a.resetAM29()
}

// blankAt reports whether the 256 bytes at addr read as erased.
func (a *ArduBDM) blankAt(addr uint32) bool {
	got, err := a.block(addr, 256)
	return err == nil && erased(got)
}

//go:embed am29prog.bin
var am29Driver []byte

// programAM29 runs the CPU32 flash driver (ardubdm/driver/am29prog.s) from
// the ECU's SRAM: per run one block write carries the parameter block plus up
// to am29DrvData bytes, then the target programs them at bus speed and drops
// back into BDM. All-0xFFFF blocks are skipped: programming only clears bits.
func (a *ArduBDM) programAM29(e *ECU, bin []byte, prog progressFn) error {
	if err := a.resetAM29(); err != nil {
		return err
	}
	if err := a.uploadDriver(e, am29Driver); err != nil {
		return err
	}
	noprog := func(uint32) {}
	if err := a.writeSysReg(sysregSR, 0x2700); err != nil { // interrupts off while our code runs
		return err
	}
	blk := make([]byte, arduLoad)
	for done := uint32(0); done < e.FlashSize; {
		n := min(uint32(am29DrvData), e.FlashSize-done)
		data := bin[done : done+n]
		if erased(data) {
			done += n
			prog(done)
			continue
		}
		binary.BigEndian.PutUint32(blk[0:], e.FlashAddr+done)
		binary.BigEndian.PutUint32(blk[4:], 0xAAAA)
		binary.BigEndian.PutUint32(blk[8:], 0x5554)
		binary.BigEndian.PutUint32(blk[12:], am29SWSR)
		binary.BigEndian.PutUint16(blk[16:], uint16(n/2))
		binary.BigEndian.PutUint16(blk[18:], 0)
		copy(blk[am29DrvHeader:], data)
		if err := a.load(e.DrvAddr+am29DrvParams, blk[:am29DrvHeader+n], noprog); err != nil {
			return err
		}
		if _, err := a.RunWait(e.DrvAddr); err != nil {
			a.resetAM29()
			return fmt.Errorf("flash driver did not return at %08X: %w", e.FlashAddr+done, err)
		}
		// The driver reports through its parameter block, read back with a block dump.
		res, err := a.block(e.DrvAddr+am29DrvParams, am29DrvHeader)
		if err != nil {
			a.resetAM29()
			return err
		}
		if binary.BigEndian.Uint16(res[18:]) != 0 {
			a.logDriverState(e, done)
			a.resetAM29()
			return fmt.Errorf("flash program failed at %08X", binary.BigEndian.Uint32(res[0:]))
		}
		done += n
		prog(done)
	}
	return a.resetAM29()
}

// logDriverState dumps what the flash driver left behind after a failure:
// its registers, the flash around the failing word, and its parameter block.
func (a *ArduBDM) logDriverState(e *ECU, done uint32) {
	regs := ""
	for i, n := range []string{"D0", "D1", "D2", "D3", "", "", "", "", "A0", "A1", "A2", "A3", "A4"} {
		if n == "" {
			continue
		}
		r, err := a.cmd(grpRegs, 'a', fmt.Sprintf("%02X", i), 8)
		if err != nil {
			r = []byte("????????")
		}
		regs += fmt.Sprintf("%s=%s ", n, r)
	}
	debugLog("driver regs: %s", regs)
	if fl, err := a.block(e.FlashAddr+done, 16); err == nil {
		debugLog("flash at %08X: % X", e.FlashAddr+done, fl)
	}
	if pb, err := a.block(e.DrvAddr+am29DrvParams, 32); err == nil {
		debugLog("params at %08X: % X", e.DrvAddr+am29DrvParams, pb)
	}
}

// Identify works out which ECU is attached from its CPU and flash chips, the
// way Just4Trionic does at connect. Reset into BDM, then: a 68377 module
// configuration register means Trionic 8. Otherwise apply Just4Trionic's
// 68332 prep (boot chip select over 1 MB at 0, byte-lane write selects,
// Vpp on: T5 and T7 both take it) and send AA/55/90: 29F chips need the
// unlock, 28F chips take the final 0x90 alone, both then answer with
// manufacturer and device codes at 0 and 2. Returns the matching ECU table
// entry and a description of the chips.
func (a *ArduBDM) Identify() (*ECU, string, error) {
	if err := a.Restart(); err != nil {
		return nil, "", fmt.Errorf("target will not reset into BDM: %w", err)
	}
	if err := a.setFunctionCode(fcSuperData); err != nil {
		return nil, "", err
	}
	r, err := a.cmd(grpMemory, cmdReadWord, hex32(0xfffa00), 4)
	if err != nil {
		return nil, "", err
	}
	if mcr := unhex16(r); mcr&0x7e4f == 0x7e4f { // 68377 MCR after reset, per Just4Trionic
		return ecuByName("Trionic 8"), fmt.Sprintf("MC68377 (MCR %04X)", mcr), nil
	}
	prep := []memWrite{
		w8(0xfffa21, 0x00), w16(0xfffa44, 0x3fff),
		w16(0xfffa48, 0x0007), w16(0xfffa4a, 0x6870),
		w16(0xfffa50, 0x0007), w16(0xfffa52, 0x3030),
		w16(0xfffa54, 0x0007), w16(0xfffa56, 0x5030),
		w16(0xfffc14, 0x0040), w8(0xfffc17, 0x40), // Vpp on: 28F chips only answer with it
	}
	for _, w := range prep {
		if err := a.writeMem(w.addr, w.val, w.size); err != nil {
			return nil, "", err
		}
	}
	time.Sleep(10 * time.Millisecond)
	ids, err := a.batch([]req{
		wordW(0xaaaa, 0xaaaa), wordW(0x5554, 0x5555), wordW(0xaaaa, 0x9090),
		wordR(0), wordR(2),
		wordW(0xaaaa, 0xf0f0), wordW(0, 0xffff), wordW(0, 0xffff), // 29F reset, 28F reset
		wordW(0xfffc14, 0x0000), // Vpp off
	})
	if err != nil {
		return nil, "", err
	}
	mfr, dev := unhex16(ids[3]), unhex16(ids[4])
	desc := fmt.Sprintf("flash ID %04X/%04X", mfr, dev)
	// Leave the ECU running its own code with BDM enabled (reset with BKPT
	// held, then go), so a later halt sees the ECU's real setup rather than
	// the probe prep above, and any flash operation can still get in.
	if err := a.Restart(); err == nil {
		_ = a.Run(0)
	}
	type match struct {
		mfr, dev   uint16
		ecu, chips string
	}
	for _, m := range []match{
		{0x0001, 0x22ab, "Trionic 7", "AMD 29F400BB"},
		{0x0001, 0x2223, "Trionic 7", "AMD 29F400BT"},
		{0x0101, 0x2020, "Trionic 5.5 (AM29F010 chips)", "2x AMD 29F010"},
		{0x0101, 0xa7a7, "Trionic 5.5 (28F010 chips)", "2x AMD 28F010"},
		{0x8989, 0xb4b4, "Trionic 5.5 (28F010 chips)", "2x Intel 28F010"},
		{0x0101, 0x2525, "Trionic 5.2", "2x AMD 28F512"},
		{0x8989, 0xb8b8, "Trionic 5.2", "2x Intel 28F512"},
	} {
		if mfr == m.mfr && dev == m.dev {
			return ecuByName(m.ecu), m.chips + " (" + desc + ")", nil
		}
	}
	return nil, desc, fmt.Errorf("unknown flash chips, %s", desc)
}

// Info decodes the 68332 SIM registers into a readable summary: clock,
// last reset reason, watchdog/bus monitor, and the chip-select memory map.
// It halts the running ECU first so the values are the ECU's own setup; if
// it cannot be halted (BDM not enabled at its last reset) it resets into BDM
// and reports the reset defaults, saying so.
func (a *ArduBDM) Info() (string, error) {
	state := "halted, ECU's own setup"
	if err := a.Stop(); err != nil {
		if err := a.Restart(); err != nil {
			return "", fmt.Errorf("target will not enter BDM: %w", err)
		}
		state = "after reset into BDM (reset defaults, the ECU code has not run)"
	}
	if err := a.setFunctionCode(fcSuperData); err != nil {
		return "", err
	}
	var reqs []req
	reqs = append(reqs, wordR(0xfffa00), wordR(0xfffa04), byteR(0xfffa07), byteR(0xfffa21),
		wordR(0xfffa44), wordR(0xfffa46))
	for i := 0; i < 12; i++ { // CSBOOT, CS0..CS10
		reqs = append(reqs, wordR(0xfffa48+uint32(4*i)), wordR(0xfffa4a+uint32(4*i)))
	}
	r, err := a.batch(reqs)
	if err != nil {
		return "", err
	}
	simcr, syncr := unhex16(r[0]), unhex16(r[1])
	rsr, sypcr := unhex16(r[2]), unhex16(r[3])
	cspar0, cspar1 := unhex16(r[4]), unhex16(r[5])
	var b strings.Builder
	fmt.Fprintf(&b, "SIM registers (%s)\n", state)
	if simcr&0x7e4f == 0x7e4f {
		fmt.Fprintf(&b, "  MC68377 (MCR %04X); SIM decode not implemented for it\n", simcr)
		return b.String(), nil
	}
	// SYNCR: W bit 15, X bit 14, Y bits 13-8; fsys = 4 * 32768 * (Y+1) * 2^(2W+X)
	w, x, y := syncr>>15&1, syncr>>14&1, syncr>>8&0x3f
	fsys := 4.0 * 32768 * float64(y+1) * float64(uint(1)<<(2*w+x))
	fmt.Fprintf(&b, "  SIMCR %04X  SYNCR %04X: CPU clock %.2f MHz\n", simcr, syncr, fsys/1e6)
	reasons := ""
	for bit, name := range map[uint]string{7: "external", 6: "power-on", 5: "software watchdog", 4: "halt (double bus fault)", 2: "loss of clock", 1: "RESET instruction", 0: "test"} {
		if rsr>>bit&1 == 1 {
			reasons += name + " "
		}
	}
	fmt.Fprintf(&b, "  RSR %02X: last reset by %s\n", rsr, strings.TrimSpace(reasons))
	swe, swp, swt := sypcr>>7&1, sypcr>>6&1, sypcr>>4&3
	wd := "off"
	if swe == 1 {
		clocks := float64(uint(1)<<(9+2*swt)) * float64(1+511*swp)
		wd = fmt.Sprintf("on, timeout %.1f ms", clocks/fsys*1e3)
	}
	bm := "off"
	if sypcr>>2&1 == 1 {
		bm = fmt.Sprintf("on, %d us", []int{64, 32, 16, 8}[sypcr&3])
	}
	fmt.Fprintf(&b, "  SYPCR %02X: watchdog %s; bus monitor %s; halt monitor %s\n", sypcr, wd, bm, []string{"off", "on"}[sypcr>>3&1])
	fmt.Fprintf(&b, "  chip selects (CSPAR0 %04X CSPAR1 %04X):\n", cspar0, cspar1)
	sizes := []string{"2K", "8K", "16K", "64K", "128K", "256K", "512K", "1M"}
	for i := 0; i < 12; i++ {
		bar, or := unhex16(r[6+2*i]), unhex16(r[7+2*i])
		name := "CSBOOT"
		pin := cspar0 & 3
		if i > 0 {
			name = fmt.Sprintf("CS%d", i-1)
			if i-1 < 6 {
				pin = cspar0 >> (2 * uint(i)) & 3
			} else {
				pin = cspar1 >> (2 * uint(i-7)) & 3
			}
		}
		byteSel := or >> 13 & 3
		if pin < 2 || byteSel == 0 {
			continue // pin not a chip select, or select disabled
		}
		rw := []string{"?", "read", "write", "read/write"}[or>>11&3]
		bytes := []string{"", "lower byte", "upper byte", "both bytes"}[byteSel]
		dsack := or >> 6 & 0xf
		ws := fmt.Sprintf("%d wait states", dsack)
		if dsack == 14 {
			ws = "external DSACK"
		} else if dsack == 15 {
			ws = "fast termination"
		}
		fmt.Fprintf(&b, "    %-6s %06X size %-4s %-10s %-11s %s\n", name, uint32(bar&0xfff8)<<8, sizes[bar&7], rw, bytes, ws)
	}
	return b.String(), nil
}

// uploadDriver maps the driver RAM, writes drv there and reads it back.
func (a *ArduBDM) uploadDriver(e *ECU, drv []byte) error {
	if e.DrvAddr == 0 {
		return fmt.Errorf("%s: no RAM defined for the flash driver", e.Name)
	}
	for _, w := range e.DrvPrep {
		if err := a.writeMem(w.addr, w.val, w.size); err != nil {
			return err
		}
	}
	if err := a.load(e.DrvAddr, drv, func(uint32) {}); err != nil {
		return err
	}
	got, err := a.block(e.DrvAddr, uint32(len(drv)))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, drv) {
		i := 0
		for i < len(got) && got[i] == drv[i] {
			i++
		}
		return fmt.Errorf("flash driver upload corrupted: block write to %08X reads back wrong from byte %d: want % X got % X",
			e.DrvAddr, i, drv[i:min(i+8, len(drv))], got[i:min(i+8, len(got))])
	}
	return nil
}

func erased(b []byte) bool {
	for _, x := range b {
		if x != 0xFF {
			return false
		}
	}
	return true
}

// ---- 28F010 (original T5) ----
//
// No program/erase engine in these chips: the CPU32 driver in
// ardubdm/driver/am28prog.s does every pulse and verify from the 68332's
// internal RAM. Erase is two passes as Intel requires: program the array to
// 0x0000 (mode 1 with zero data, so it shows progress), then erase pulses over
// the whole chip (mode 2).

//go:embed am28prog.bin
var am28Driver []byte

var am28TimingCheck = true // tests run against a fake port that answers instantly

// resetAM28 puts both chips in read mode: 0xFF twice.
func (a *ArduBDM) resetAM28(e *ECU) error {
	_, err := a.batch([]req{wordW(e.FlashAddr, 0xFFFF), wordW(e.FlashAddr, 0xFFFF)})
	return err
}

// am28Run uploads one parameter block plus data and runs the driver; returns
// the header it left behind: address at +0, result at +12, stat at +14.
func (a *ArduBDM) am28Run(e *ECU, mode uint16, addr, end uint32, data []byte, limit time.Duration) ([]byte, error) {
	blk := make([]byte, am28DrvHeader+len(data))
	binary.BigEndian.PutUint32(blk[0:], addr)
	binary.BigEndian.PutUint32(blk[4:], end)
	binary.BigEndian.PutUint16(blk[8:], mode)
	binary.BigEndian.PutUint16(blk[10:], uint16(len(data)/2))
	if a.d10ms == 0 {
		a.d10us, a.d6us, a.d10ms = am28Delay10us, am28Delay6us, am28Delay10ms
	}
	binary.BigEndian.PutUint16(blk[16:], a.d10us)
	binary.BigEndian.PutUint16(blk[18:], a.d6us)
	binary.BigEndian.PutUint16(blk[20:], a.d10ms)
	copy(blk[am28DrvHeader:], data)
	if err := a.load(e.DrvAddr+am28DrvParams, blk, func(uint32) {}); err != nil {
		return nil, err
	}
	if _, err := a.RunWaitFor(e.DrvAddr, limit); err != nil {
		return nil, fmt.Errorf("flash driver did not return (%s): %w", modeName(mode), err)
	}
	res, err := a.block(e.DrvAddr+am28DrvParams, am28DrvHeader)
	if err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(res[12:]) != 0 {
		return res, fmt.Errorf("flash %s failed at %08X", modeName(mode), binary.BigEndian.Uint32(res[0:]))
	}
	return res, nil
}

func modeName(mode uint16) string {
	switch mode {
	case am28ModeProg:
		return "program"
	case am28ModeErase:
		return "erase"
	}
	return "timing"
}

// am28Setup maps the driver RAM, uploads and verifies the driver, and
// calibrates its delay loop: 100 x "10 ms" with the nominal counts is timed
// against the wall clock and the counts scaled so a pulse really is 10 ms.
func (a *ArduBDM) am28Setup(e *ECU) error {
	if err := a.uploadDriver(e, am28Driver); err != nil {
		return err
	}
	if err := a.writeSysReg(sysregSR, 0x2700); err != nil {
		return err
	}
	a.d10us, a.d6us, a.d10ms = am28Delay10us, am28Delay6us, am28Delay10ms
	t := time.Now()
	if _, err := a.am28Run(e, am28ModeTime, 0, 0, nil, 10*time.Second); err != nil {
		return err
	}
	d := time.Since(t)
	if am28TimingCheck && (d < 300*time.Millisecond || d > 5*time.Second) {
		return fmt.Errorf("flash driver timing off (%v for 100 x 10 ms): wrong CPU clock?", d.Round(time.Millisecond))
	}
	if am28TimingCheck {
		scale := float64(time.Second) / float64(d)
		a.d10us = uint16(float64(am28Delay10us) * scale)
		a.d6us = uint16(float64(am28Delay6us) * scale)
		a.d10ms = uint16(float64(am28Delay10ms) * scale)
	}
	debugLog("28F driver delay check: 100 x 10 ms took %v, counts scaled to %d/%d/%d",
		d.Round(time.Millisecond), a.d10us, a.d6us, a.d10ms)
	return nil
}

// programAM28 programs the image block by block through the driver. Blocks
// that are all 0xFF are skipped: on erased flash they are no-ops.
func (a *ArduBDM) programAM28(e *ECU, bin []byte, prog progressFn) error {
	if err := a.resetAM28(e); err != nil {
		return err
	}
	if err := a.am28Setup(e); err != nil {
		return err
	}
	for done := uint32(0); done < e.FlashSize; {
		n := min(uint32(am28DrvData), e.FlashSize-done)
		data := bin[done : done+n]
		if !erased(data) {
			if _, err := a.am28Run(e, am28ModeProg, e.FlashAddr+done, 0, data, 5*time.Second); err != nil {
				a.resetAM28(e)
				return err
			}
		}
		done += n
		prog(done)
	}
	return a.resetAM28(e)
}

// eraseAM28: program everything to 0x0000, then erase pulses over the chip.
func (a *ArduBDM) eraseAM28(e *ECU, prog progressFn) error {
	if err := a.resetAM28(e); err != nil {
		return err
	}
	if err := a.am28Setup(e); err != nil {
		return err
	}
	zeros := make([]byte, am28DrvData)
	for done := uint32(0); done < e.FlashSize; {
		n := min(uint32(am28DrvData), e.FlashSize-done)
		if _, err := a.am28Run(e, am28ModeProg, e.FlashAddr+done, 0, zeros[:n], 5*time.Second); err != nil {
			a.resetAM28(e)
			return fmt.Errorf("pre-erase zero fill: %w", err)
		}
		done += n
		prog(done / 2)
	}
	// Pulse phase in 16 address chunks for the progress bar: a pulse only
	// happens when a word fails verify, so the chunking adds almost none.
	// 1000 pulses x 10 ms plus verify reads: allow a generous minute each.
	chunk := e.FlashSize / 16
	for done := uint32(0); done < e.FlashSize; done += chunk {
		res, err := a.am28Run(e, am28ModeErase, e.FlashAddr+done, e.FlashAddr+done+chunk, nil, 60*time.Second)
		if err != nil {
			a.resetAM28(e)
			return err
		}
		if done == 0 {
			debugLog("28F erase: first chunk took %d pulses", 1000-binary.BigEndian.Uint16(res[14:]))
		}
		prog(e.FlashSize/2 + (done+chunk)/2)
	}
	return a.resetAM28(e)
}
