// Package main: CombiAdapter BDM transport.
//
// Wire protocol (same framing as the CAN side): cmd, size (u16 BE), payload,
// terminator (0x00 ack / 0xFF nak), in both directions. Command payloads are
// big-endian, reply payloads little-endian -- that asymmetry is the firmware's,
// not a bug here.
//
// BDM mode has no CAN traffic, so this is plain synchronous request/response:
// no read loop, no queues. Long operations (readflash) stream unsolicited
// reply packets carrying the same command code, which recv() just keeps
// reading.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gotmc/libusb/v2"
)

const (
	combiVID = 0xFFFF
	combiPID = 0x0005
	bdm2PID  = 0x0006 // USB BDM MkII -- same firmware protocol, own PID

	// Untyped: gotmc's transfer methods take an unexported endpoint type.
	inEP   = 0x82
	outEP5 = 0x05
	outEP2 = 0x02 // MkII, and STM32 CombiAdapter clones

	termAck = 0x00
	termNak = 0xFF

	cmdFWVersion = 0x20

	cmdBDMStop        = 0x40
	cmdBDMReset       = 0x41
	cmdBDMRun         = 0x42
	cmdBDMReadMem     = 0x45
	cmdBDMWriteMem    = 0x46
	cmdBDMWriteSysReg = 0x48
	cmdBDMReadFlash   = 0x4B
	cmdBDMEraseFlash  = 0x4C
	cmdBDMWriteFlash  = 0x4D

	cmdCANOpen = 0x80

	combiBlock = 256 // CombiAdapter firmware transfer block
	bdm2Block  = 32  // MkII firmware transfer block

	sysregSR  = 0x0B
	sysregSFC = 0x0E
	sysregDFC = 0x0F

	fcSuperData = 5 // 68K supervisor data space

	defaultTimeout = 10000 // ms
	eraseTimeout   = 90000 // ms
)

type Combi struct {
	ctx    *libusb.Context
	dev    *libusb.Device
	h      *libusb.DeviceHandle
	rd     *bufio.Reader
	iface  int
	useEP2 bool
	blk    uint32 // flash transfer block
	tmo    int    // current bulk timeout, ms
}

// Open connects to a CombiAdapter.
func Open() (*Combi, error) { return openUSB("CombiAdapter", combiPID, combiBlock, true) }

// OpenBDM2 connects to a USB BDM MkII. Same packet protocol as the
// CombiAdapter (transcribed from caUSBBDM2, which shares caLibUsbAdapter with
// caCombiAdapter) minus the CAN side, with a smaller transfer block.
func OpenBDM2() (*Combi, error) { return openUSB("USB BDM MkII", bdm2PID, bdm2Block, false) }

func openUSB(name string, pid uint16, blk uint32, can bool) (*Combi, error) {
	ctx, err := libusb.NewContext()
	if err != nil {
		return nil, fmt.Errorf("libusb init: %w", err)
	}
	dev, h, err := ctx.OpenDeviceWithVendorProduct(combiVID, pid)
	if err != nil {
		ctx.Close()
		return nil, fmt.Errorf("%s not found: %w", name, err)
	}
	c := &Combi{ctx: ctx, dev: dev, h: h, blk: blk, tmo: defaultTimeout}
	c.iface, c.useEP2 = c.endpoints()
	c.rd = bufio.NewReaderSize(c, 4096)

	_ = h.SetAutoDetachKernelDriver(true)
	if err := h.ClaimInterface(c.iface); err != nil {
		c.Close()
		return nil, fmt.Errorf("claim interface %d: %w", c.iface, err)
	}

	c.drain()
	if can {
		// Known state: CAN channel closed (BDM and CAN sessions are exclusive).
		if _, err := c.cmd(cmdCANOpen, []byte{0}, 0); err != nil {
			c.Close()
			return nil, fmt.Errorf("adapter did not respond: %w", err)
		}
		return c, nil
	}
	if _, _, err := c.Version(); err != nil {
		c.Close()
		return nil, fmt.Errorf("adapter did not respond: %w", err)
	}
	return c, nil
}

func (c *Combi) Close() {
	if c.h != nil {
		_ = c.h.ReleaseInterface(c.iface)
		_ = c.h.Close()
	}
	if c.dev != nil {
		c.dev.Close()
	}
	if c.ctx != nil {
		_ = c.ctx.Close()
	}
}

// =====================
// transport
// =====================

// Read implements io.Reader over the bulk IN endpoint.
func (c *Combi) Read(p []byte) (int, error) {
	n, err := c.h.BulkTransfer(inEP, p, len(p), c.tmo)
	if n > 0 {
		// ponytail: swallow a timeout that still delivered bytes; the next
		// read reports it. Keeps the packet stream in sync.
		return n, nil
	}
	return n, err
}

func (c *Combi) write(p []byte) error {
	var err error
	if c.useEP2 {
		_, err = c.h.BulkTransferOut(outEP2, p, c.tmo)
	} else {
		_, err = c.h.BulkTransferOut(outEP5, p, c.tmo)
	}
	return err
}

// endpoints finds the interface carrying the bulk IN endpoint and whether its
// bulk OUT is 0x02 rather than 0x05. The CombiAdapter uses interface 1 / EP
// 0x05, its STM32 clones and the MkII use EP 0x02; falling out of the scan
// keeps those defaults.
func (c *Combi) endpoints() (iface int, useEP2 bool) {
	cfg, err := c.dev.ActiveConfigDescriptor()
	if err != nil {
		return 1, false
	}
	for _, si := range cfg.SupportedInterfaces {
		for _, id := range si.InterfaceDescriptors {
			var hasIn, hasEP5 bool
			for _, ep := range id.EndpointDescriptors {
				switch byte(ep.EndpointAddress) {
				case inEP:
					hasIn = true
				case outEP5:
					hasEP5 = true
				}
			}
			if hasIn {
				return int(id.InterfaceNumber), !hasEP5
			}
		}
	}
	return 1, false
}

func (c *Combi) drain() {
	old := c.tmo
	c.tmo = 50
	buf := make([]byte, 512)
	for i := 0; i < 20; i++ {
		if n, err := c.h.BulkTransfer(inEP, buf, len(buf), c.tmo); err != nil || n == 0 {
			break
		}
	}
	c.rd.Reset(c)
	c.tmo = old
}

func packet(code byte, data []byte) []byte {
	p := make([]byte, 4+len(data))
	p[0] = code
	binary.BigEndian.PutUint16(p[1:3], uint16(len(data)))
	copy(p[3:], data)
	p[3+len(data)] = termAck
	return p
}

func (c *Combi) send(code byte, data []byte) error {
	return c.write(packet(code, data))
}

// sendBreak asks the adapter to abort a long-running operation.
func (c *Combi) sendBreak(code byte) {
	_ = c.write([]byte{code, 0, 0, termNak})
}

func (c *Combi) recv(code byte, wantLen int) ([]byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:3]))
	buf := make([]byte, n+1)
	if _, err := io.ReadFull(c.rd, buf); err != nil {
		return nil, err
	}
	switch {
	case hdr[0] != code:
		return nil, fmt.Errorf("unexpected response %02X, want %02X", hdr[0], code)
	case buf[n] != termAck:
		return nil, fmt.Errorf("command %02X failed (NAK)", code)
	case n != wantLen:
		return nil, fmt.Errorf("command %02X: reply len %d, want %d", code, n, wantLen)
	}
	return buf[:n], nil
}

func (c *Combi) cmd(code byte, data []byte, replyLen int) ([]byte, error) {
	if err := c.send(code, data); err != nil {
		return nil, err
	}
	return c.recv(code, replyLen)
}

// =====================
// board
// =====================

// Version returns the adapter firmware version.
func (c *Combi) Version() (major, minor byte, err error) {
	d, err := c.cmd(cmdFWVersion, nil, 2)
	if err != nil {
		return 0, 0, err
	}
	return d[1], d[0], nil
}

// =====================
// BDM primitives
// =====================

func (c *Combi) Stop() error  { _, err := c.cmd(cmdBDMStop, nil, 0); return err }
func (c *Combi) Reset() error { _, err := c.cmd(cmdBDMReset, nil, 0); return err }

func (c *Combi) Run(addr uint32) error {
	_, err := c.cmd(cmdBDMRun, be32(addr), 0)
	return err
}

func (c *Combi) writeSysReg(reg byte, v uint32) error {
	_, err := c.cmd(cmdBDMWriteSysReg, append([]byte{reg}, be32(v)...), 0)
	return err
}

func (c *Combi) setFunctionCode(fc uint32) error {
	if err := c.writeSysReg(sysregSFC, fc); err != nil {
		return err
	}
	return c.writeSysReg(sysregDFC, fc)
}

// memPayloads builds the cmdBDMWriteMem payloads for a size-byte (1, 2 or 4)
// write at addr. Unaligned multi-byte writes are split into byte writes,
// low byte first -- the prepare tables target odd addresses like 0xfffa21, so
// this path is exercised on every T7/T5 session.
func memPayloads(addr, val uint32, size int) ([][]byte, error) {
	if size > 1 && addr%2 != 0 {
		var out [][]byte
		for i := range size {
			out = append(out, append(be32(addr+uint32(i)), byte(val>>(8*i))))
		}
		return out, nil
	}
	switch size {
	case 1:
		return [][]byte{append(be32(addr), byte(val))}, nil
	case 2:
		return [][]byte{append(be32(addr), byte(val>>8), byte(val))}, nil
	case 4:
		return [][]byte{append(be32(addr), be32(val)...)}, nil
	}
	return nil, fmt.Errorf("bad write size %d", size)
}

func (c *Combi) writeMem(addr, val uint32, size int) error {
	payloads, err := memPayloads(addr, val, size)
	if err != nil {
		return err
	}
	for _, p := range payloads {
		if _, err := c.cmd(cmdBDMWriteMem, p, 0); err != nil {
			return err
		}
	}
	return nil
}

// readMem32 reads a long word. setAddr=false continues from the last address.
func (c *Combi) readMem32(addr uint32, setAddr bool) (uint32, error) {
	req := []byte{4, 0}
	if setAddr {
		req[1] = 1
		req = append(req, be32(addr)...)
	}
	d, err := c.cmd(cmdBDMReadMem, req, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(d), nil
}

// enterBDM halts the MCU, selects supervisor data space and applies the ECU's
// chip-select / watchdog setup so flash is reachable.
func (c *Combi) enterBDM(e *ECU) error {
	if err := c.Stop(); err != nil {
		return err
	}
	if err := c.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	for _, w := range e.Prepare {
		if err := c.writeMem(w.addr, w.val, w.size); err != nil {
			return err
		}
	}
	return nil
}

// =====================
// flash / SRAM operations
// =====================

type progressFn func(done uint32)

// ReadFlash dumps the ECU flash to w.
func (c *Combi) ReadFlash(e *ECU, w io.Writer, prog progressFn) error {
	if err := c.enterBDM(e); err != nil {
		return err
	}
	hdr := append(be32(e.FlashAddr), be32(e.FlashSize)...)
	for done := uint32(0); done < e.FlashSize; done += c.blk {
		var (
			data []byte
			err  error
		)
		if done == 0 {
			data, err = c.cmd(cmdBDMReadFlash, hdr, int(c.blk))
		} else {
			data, err = c.recv(cmdBDMReadFlash, int(c.blk))
		}
		if err != nil {
			c.sendBreak(cmdBDMReadFlash)
			c.drain()
			return err
		}
		if _, err := w.Write(data); err != nil {
			c.sendBreak(cmdBDMReadFlash)
			c.drain()
			return err
		}
		prog(done + c.blk)
	}
	return nil
}

// EraseFlash erases the whole ECU flash. The MCU must already be in BDM mode
// (EraseFlash does that itself when called standalone).
func (c *Combi) EraseFlash(e *ECU, prog progressFn) error {
	if err := c.enterBDM(e); err != nil {
		return err
	}
	return c.eraseFlash(e, prog)
}

func (c *Combi) eraseFlash(e *ECU, prog progressFn) error {
	req := append([]byte(e.FlashType), append(be32(e.FlashAddr), be32(e.FlashSize)...)...)
	if err := c.send(cmdBDMEraseFlash, req); err != nil {
		return err
	}
	defer func() { c.tmo = defaultTimeout }()

	// 28Fxxx chips report progress: one empty reply per word erased.
	if e.EraseFeedback {
		c.tmo = 5000
		for done := uint32(0); done < e.FlashSize; done += 2 {
			if _, err := c.recv(cmdBDMEraseFlash, 0); err != nil {
				c.sendBreak(cmdBDMEraseFlash)
				c.drain()
				return err
			}
			prog(done + 2)
		}
	}

	c.tmo = eraseTimeout
	if _, err := c.recv(cmdBDMEraseFlash, 0); err != nil {
		c.sendBreak(cmdBDMEraseFlash)
		c.drain()
		return err
	}
	prog(e.FlashSize)
	return nil
}

// WriteFlash programs bin into the ECU flash. bin must be exactly FlashSize.
func (c *Combi) WriteFlash(e *ECU, bin []byte, erase bool, prog progressFn) error {
	if uint32(len(bin)) != e.FlashSize {
		return fmt.Errorf("file is %d bytes, %s flash is %d", len(bin), e.Name, e.FlashSize)
	}
	if err := c.enterBDM(e); err != nil {
		return err
	}
	if erase {
		if err := c.eraseFlash(e, prog); err != nil {
			return err
		}
	}
	req := append([]byte(e.FlashType), append(be32(e.FlashAddr), be32(e.FlashSize)...)...)
	if _, err := c.cmd(cmdBDMWriteFlash, req, 0); err != nil {
		return err
	}
	for done := uint32(0); done < e.FlashSize; done += c.blk {
		if _, err := c.cmd(cmdBDMWriteFlash, bin[done:done+c.blk], 0); err != nil {
			c.sendBreak(cmdBDMWriteFlash)
			c.drain()
			return err
		}
		prog(done + c.blk)
	}
	return nil
}

// enterSRAM halts the MCU in supervisor data space. Unlike flash, SRAM is
// internal to the MCU, so no chip-select setup is needed. The run/stop pair
// puts the core in a defined state first, as the reference driver does.
func (c *Combi) enterSRAM(e *ECU) error {
	if e.SRAMSize == 0 {
		return fmt.Errorf("%s has no SRAM defined", e.Name)
	}
	if err := c.Stop(); err != nil {
		return err
	}
	if err := c.setFunctionCode(fcSuperData); err != nil {
		return err
	}
	if err := c.Run(0); err != nil {
		return err
	}
	return c.Stop()
}

// sramBytes is the SRAM size rounded up to a long word -- T7/T8 declare
// 0xffff, and reads/writes always move whole long words.
func sramBytes(e *ECU) uint32 { return (e.SRAMSize + 3) &^ 3 }

// ReadSRAM dumps the ECU SRAM to w.
func (c *Combi) ReadSRAM(e *ECU, w io.Writer, prog progressFn) error {
	if err := c.enterSRAM(e); err != nil {
		return err
	}
	var buf [4]byte
	for done := uint32(0); done < sramBytes(e); done += 4 {
		v, err := c.readMem32(e.SRAMAddr+done, done == 0)
		if err != nil {
			return err
		}
		// Firmware hands back the long word little-endian; the file wants 68K
		// byte order.
		binary.BigEndian.PutUint32(buf[:], v)
		if _, err := w.Write(buf[:]); err != nil {
			return err
		}
		prog(done + 4)
	}
	return nil
}

// WriteSRAM restores an SRAM snapshot. The adapter firmware has no bulk SRAM
// command (the original BDM Tool never implemented this button at all), so it
// is one write-memory round trip per long word.
//
// ponytail: ~16k USB round trips for a 64 KB snapshot, roughly 20 s. Fine for
// a one-shot restore; needs a firmware-side bulk command to go faster.
func (c *Combi) WriteSRAM(e *ECU, snap []byte, prog progressFn) error {
	if uint32(len(snap)) != sramBytes(e) {
		return fmt.Errorf("snapshot is %d bytes, %s SRAM is %d", len(snap), e.Name, sramBytes(e))
	}
	if err := c.enterSRAM(e); err != nil {
		return err
	}
	for done := uint32(0); done < sramBytes(e); done += 4 {
		// The file holds 68K byte order; writeMem sends the value big-endian
		// too, so this round-trips what ReadSRAM produced.
		v := binary.BigEndian.Uint32(snap[done : done+4])
		if err := c.writeMem(e.SRAMAddr+done, v, 4); err != nil {
			return err
		}
		prog(done + 4)
	}
	return nil
}

func be32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
