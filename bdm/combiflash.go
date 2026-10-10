// Package bdm: CombiAdapter (LPC1768) firmware update over its USB bootloader.
//
// The bootloader runs for about 4 s after every reset or power-up. If the
// adapter is already running its app, fw 2.0 and later reboot into the
// bootloader on request (command 0x26); with older firmware the upload waits
// for the user to replug the adapter and catches the bootloader then.
//
// The bootloader speaks an ASCII protocol on the same USB id/endpoints as the
// app (ffff:0005, OUT 0x05 / IN 0x82): send "UPDT\r", get "RDY\r" (4-byte
// blocks) or "RDY2\r" (200-byte blocks), stream the .bin as uppercase hex
// blocks each terminated by \r and acked with \r, then "EXIT\r". Where
// libusb can't open the adapter because Windows gave it the serial driver,
// the same lines go through its COM port.
package bdm

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const bootTimeout = 3000 // ms

// flashCombi writes fw to a CombiAdapter.
func flashCombi(fw []byte, logf func(string, ...any), prog progressFn) error {
	c, block, err := bootEnter(logf)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := bootSend(c, block, fw, prog); err != nil {
		return err
	}
	logf("Firmware updated, CombiAdapter restarting")
	return nil
}

// installCombiBoot replaces the adapter's bootloader with bootloader ver,
// then writes fw through it. inst is that bootloader's installer: an app that
// checks the bootloader it carries, writes it to sectors 0-2 (~0.4 s), drops
// off USB, erases itself and resets. Run on that bootloader already, it only
// does the last part. block is the line size ver signs in with: 200 for 2.0,
// 4 for 1.0.
func installCombiBoot(ver string, block int, inst, fw []byte, logf func(string, ...any), prog progressFn) error {
	c, got, err := bootEnter(logf)
	if err != nil {
		return err
	}
	err = bootSend(c, got, inst, prog)
	c.Close()
	if err != nil {
		return fmt.Errorf("installer: %w", err)
	}
	logf("Installing bootloader %s, keep the adapter plugged in", ver)
	// The old bootloader starts the installer on EXIT; the pause keeps our
	// UPDT off it until it has. The new bootloader then finds no app and
	// waits for one, so there's no window to hit; 60 s covers Windows
	// installing a driver for the new device first.
	time.Sleep(time.Second)
	if c, got, err = bootWait(60 * time.Second); err != nil {
		return fmt.Errorf("bootloader %s didn't come up (%w)\nIf the error LED blinks, the installer refused its copy and left the old bootloader in place.\n"+
			"On Windows, if the adapter shows up without a driver, the bootloader is in: give it WinUSB with Zadig, then flash the firmware from this menu to finish", ver, err)
	}
	defer c.Close()
	if got != block {
		return fmt.Errorf("the old bootloader answered, not bootloader %s: the installer didn't run", ver)
	}
	logf("Bootloader %s installed%s", ver, via(c))
	n := uint32(len(inst))
	if err := bootSend(c, block, fw, func(d uint32) { prog(n + d) }); err != nil {
		return err
	}
	logf("Firmware updated, CombiAdapter restarting")
	return nil
}

// bootEnter returns the adapter signed in to its bootloader, rebooting a
// running app into it.
func bootEnter(logf func(string, ...any)) (bootLink, int, error) {
	c, block, err := bootOpen()
	if c == nil {
		return nil, 0, err
	}
	if err != nil {
		// Not the bootloader, so the app is running: ask it to reboot into
		// the bootloader, then find it again as a new USB device. An app on
		// a COM port is 1.x: no WinUSB descriptors, no reboot command.
		logf("CombiAdapter is running its firmware, rebooting it into the bootloader")
		err = errors.New("firmware on a COM port is 1.x")
		if u, ok := c.(*Combi); ok {
			err = u.bootReboot()
		}
		c.Close()
		if err == nil {
			// The bootloader only waits ~4 s for UPDT, so give up after that.
			if c, block, err = bootWait(4 * time.Second); err != nil {
				return nil, 0, fmt.Errorf("adapter didn't come back after rebooting into the bootloader: %w", err)
			}
		} else {
			// 1.x can't reboot itself; a replug starts the bootloader, so
			// catch it then instead of making the user race its 4 s window.
			logf("The adapter's firmware can't reboot into the bootloader (%v; needs 2.0 or later).\nUnplug the CombiAdapter and plug it back in now (waiting 30 s)", err)
			if c, block, err = bootWait(30 * time.Second); err != nil {
				return nil, 0, fmt.Errorf("no bootloader within 30 s, was the adapter replugged? %w", err)
			}
		}
	}
	logf("Bootloader ready, block size %d%s", block, via(c))
	return c, block, nil
}

// bootSend streams fw to a signed-in bootloader, then EXIT starts it.
func bootSend(c bootLink, block int, fw []byte, prog progressFn) error {
	for off := 0; off < len(fw); off += block {
		if err := c.write(bootBlock(fw, off, block)); err != nil {
			return fmt.Errorf("write at %#x: %w", off, err)
		}
		ack, err := c.bootRead(bootTimeout)
		if err != nil {
			return fmt.Errorf("ack at %#x: %w", off, err)
		}
		if ack[len(ack)-1] != '\r' {
			return fmt.Errorf("block at %#x not acked: %q", off, ack)
		}
		prog(uint32(min(off+block, len(fw))))
	}
	if err := c.write([]byte("EXIT\r")); err != nil {
		return fmt.Errorf("EXIT: %w", err)
	}
	return nil
}

// bootLink carries the bootloader's lines: the adapter's bulk endpoints
// (*Combi), or its COM port (comBoot).
type bootLink interface {
	write([]byte) error
	bootRead(ms int) ([]byte, error)
	drain()
	Close()
}

// bootOpen claims the adapter, or failing that its COM port, and signs in
// to its bootloader, returning the block size. A running app doesn't answer
// the sign-in: the adapter then comes back open along with the error.
func bootOpen() (bootLink, int, error) {
	var p bootLink
	c, err := claimUSB("CombiAdapter", combiPID)
	switch {
	case err != nil:
		s, serr := openComBoot()
		if serr != nil {
			return nil, 0, err // libusb's error says more
		}
		p = s
	case c.useEP2:
		// The STM32 clones share the USB id but not the MCU or the bootloader.
		c.Close()
		return nil, 0, errors.New("not an LPC1768 CombiAdapter (STM32 clone?): this firmware is not for it")
	default:
		c.tmo = bootTimeout
		p = c
	}
	block, err := bootSignIn(p)
	return p, block, err
}

// bootSignIn sends UPDT and returns the block size the bootloader asks for.
func bootSignIn(p bootLink) (int, error) {
	p.drain() // discard any stale bytes
	if err := p.write([]byte("UPDT\r")); err != nil {
		return 0, fmt.Errorf("UPDT: %w", err)
	}
	reply, _ := p.bootRead(500)
	switch {
	case bytes.HasPrefix(reply, []byte("RDY2\r")):
		return 200, nil
	case bytes.HasPrefix(reply, []byte("RDY\r")):
		return 4, nil
	}
	return 0, fmt.Errorf("no bootloader sign-in reply (got %q)", reply)
}

// bootWait polls for the bootloader until limit. Tries can still hit the
// app (on its way down, or not yet replugged), an unplugged adapter, or udev
// setting the new device up.
func bootWait(limit time.Duration) (bootLink, int, error) {
	for deadline := time.Now().Add(limit); ; {
		time.Sleep(200 * time.Millisecond)
		c, block, err := bootOpen()
		if err == nil {
			return c, block, nil
		}
		if c != nil {
			c.Close()
		}
		if time.Now().After(deadline) {
			return nil, 0, err
		}
	}
}

// bootReboot asks a running app to reset into the bootloader:
// [26][00 04]"boot"[00], acked with [26 00 00 00] by fw 2.0+. Older firmware
// NAKs it.
func (c *Combi) bootReboot() error {
	// The app took the "UPDT\r" from the sign-in for the start of a packet; it
	// drops a half packet after 100 ms of silence (fw 2.0; 1.x sooner).
	time.Sleep(150 * time.Millisecond)
	c.drain()
	_, err := c.cmd(cmdBootMode, []byte("boot"), 0)
	return err
}

// bootRead returns the bytes of one bulk IN transfer.
func (c *Combi) bootRead(ms int) ([]byte, error) {
	buf := make([]byte, 64)
	n, err := c.h.BulkTransfer(inEP, buf, len(buf), ms)
	if err == nil && n == 0 {
		err = errors.New("empty reply")
	}
	return buf[:n], err
}

// bootBlock is the block of fw at off as the bootloader takes it: uppercase
// hex and a \r, the last block zero-padded.
func bootBlock(fw []byte, off, block int) []byte {
	b := make([]byte, block)
	copy(b, fw[off:])
	return fmt.Appendf(nil, "%X\r", b)
}

// comBoot is the bootloader through the adapter's COM port. Windows binds its
// serial driver to the 1.x bootloader and firmware (no WinUSB descriptors)
// unless Zadig replaced it, and then libusb can't open them.
type comBoot struct {
	p    serial.Port
	name string
}

// openComBoot opens the adapter's COM port, found by its USB id.
func openComBoot() (*comBoot, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	for _, d := range ports {
		if d.IsUSB && strings.EqualFold(d.VID, fmt.Sprintf("%04x", combiVID)) && strings.EqualFold(d.PID, fmt.Sprintf("%04x", combiPID)) {
			// CDC: the baud rate is only for show
			p, err := serial.Open(d.Name, &serial.Mode{BaudRate: 115200})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.Name, err)
			}
			return &comBoot{p: p, name: d.Name}, nil
		}
	}
	return nil, errors.New("no CombiAdapter COM port")
}

func (s *comBoot) write(b []byte) error {
	_, err := s.p.Write(b)
	return err
}

// bootRead returns one reply: up to its \r, or what came before a short
// silence (a 1.x app's NAK has no \r). A serial read can split a USB packet.
func (s *comBoot) bootRead(ms int) ([]byte, error) {
	var reply []byte
	buf := make([]byte, 64)
	_ = s.p.SetReadTimeout(time.Duration(ms) * time.Millisecond)
	for len(reply) == 0 || reply[len(reply)-1] != '\r' {
		n, err := s.p.Read(buf)
		if err != nil {
			return reply, err
		}
		if n == 0 {
			break
		}
		reply = append(reply, buf[:n]...)
		_ = s.p.SetReadTimeout(20 * time.Millisecond)
	}
	if len(reply) == 0 {
		return nil, errors.New("empty reply")
	}
	return reply, nil
}

func (s *comBoot) drain() { _ = s.p.ResetInputBuffer() }
func (s *comBoot) Close() { _ = s.p.Close() }

// via says where the bootloader was found, for the log: nothing for USB.
func via(p bootLink) string {
	if s, ok := p.(*comBoot); ok {
		return " on " + s.name
	}
	return ""
}
