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
// blocks each terminated by \r and acked with \r, then "EXIT\r".
package bdm

import (
	"bytes"
	"errors"
	"fmt"
	"time"
)

const bootTimeout = 3000 // ms

// flashCombi writes fw to a CombiAdapter.
func flashCombi(fw []byte, logf func(string, ...any), prog progressFn) error {
	c, block, err := bootOpen()
	if c == nil {
		return err
	}
	if err != nil {
		// Not the bootloader, so the app is running: ask it to reboot into
		// the bootloader, then find it again as a new USB device.
		logf("CombiAdapter is running its firmware, rebooting it into the bootloader")
		err = c.bootReboot()
		c.Close()
		if err == nil {
			// The bootloader only waits ~4 s for UPDT, so give up after that.
			if c, block, err = bootWait(4 * time.Second); err != nil {
				return fmt.Errorf("adapter didn't come back after rebooting into the bootloader: %w", err)
			}
		} else {
			// 1.x can't reboot itself; a replug starts the bootloader, so
			// catch it then instead of making the user race its 4 s window.
			logf("The adapter's firmware can't reboot into the bootloader (%v; needs 2.0 or later).\nUnplug the CombiAdapter and plug it back in now (waiting 30 s)", err)
			if c, block, err = bootWait(30 * time.Second); err != nil {
				return fmt.Errorf("no bootloader within 30 s, was the adapter replugged? %w", err)
			}
		}
	}
	defer c.Close()
	logf("Bootloader ready, block size %d", block)

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
	logf("Firmware updated, CombiAdapter restarting")
	return nil
}

// bootOpen claims the adapter and signs in to its bootloader, returning the
// block size. A running app doesn't answer the sign-in: the adapter then
// comes back open along with the error.
func bootOpen() (*Combi, int, error) {
	c, err := claimUSB("CombiAdapter", combiPID)
	if err != nil {
		return nil, 0, err
	}
	// The STM32 clones share the USB id but not the MCU or the bootloader.
	if c.useEP2 {
		c.Close()
		return nil, 0, errors.New("not an LPC1768 CombiAdapter (STM32 clone?): this firmware is not for it")
	}
	c.tmo = bootTimeout

	c.drain() // discard any stale bytes
	if err := c.write([]byte("UPDT\r")); err != nil {
		return c, 0, fmt.Errorf("UPDT: %w", err)
	}
	reply, _ := c.bootRead(500)
	switch {
	case bytes.HasPrefix(reply, []byte("RDY2\r")):
		return c, 200, nil
	case bytes.HasPrefix(reply, []byte("RDY\r")):
		return c, 4, nil
	}
	return c, 0, fmt.Errorf("no bootloader sign-in reply (got %q)", reply)
}

// bootWait polls for the bootloader until limit. Tries can still hit the
// app (on its way down, or not yet replugged), an unplugged adapter, or udev
// setting the new device up.
func bootWait(limit time.Duration) (*Combi, int, error) {
	for deadline := time.Now().Add(limit); ; {
		time.Sleep(50 * time.Millisecond)
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
