// Package bdm: bdmtoy firmware update over USB DFU.
//
// Firmware 2.0 and later sit behind a DFU 1.1 bootloader (ffff:0108). The
// app reboots into it on TAP_DO_BOOTLOADER; the BOOT1 jumper, or an app that
// is missing or broken, keeps the dongle there too. The image is the app's
// .bin, linked for 0x08004000, sent in 1 KB blocks; a USB reset after the
// download starts it. dfu-util does the same: dfu-util -d ffff:0108 -D
// firmware.bin -R. Firmware before 2.0 has no bootloader: flash
// bdmtoy-full.hex over SWD once.
package bdm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/gotmc/libusb/v2"
)

const (
	toyBootPID    = 0x0108
	toyBootloader = 0x0004 // TAP_DO_BOOTLOADER
	toyBootFW     = 0x0200 // first firmware with the bootloader

	dfuBlock     = 1024 // the bootloader's wTransferSize: one flash page
	dfuDetach    = 0
	dfuDnload    = 1
	dfuGetStatus = 3
	dfuClrStatus = 4
	dfuAbort     = 6

	dfuStateIdle       = 2
	dfuStateDnloadIdle = 5
	dfuStateManifest   = 7
	dfuStateError      = 10
)

// flashToy writes fw, a bdmtoy app image, to the dongle.
func flashToy(fw []byte, logf func(string, ...any), prog progressFn) error {
	if len(fw) < 8 || len(fw) > 0xC000 {
		return fmt.Errorf("bdmtoy image is %d bytes, not an app image", len(fw))
	}
	ctx, err := libusb.NewContext()
	if err != nil {
		return fmt.Errorf("libusb init: %w", err)
	}
	defer ctx.Close()

	dev, h, err := ctx.OpenDeviceWithVendorProduct(toyVID, toyBootPID)
	if err != nil {
		if err := toyEnterBootloader(logf); err != nil {
			return err
		}
		if dev, h, err = toyWaitOpen(ctx, toyBootPID, 5*time.Second); err != nil {
			return fmt.Errorf("bdmtoy bootloader did not show up (on Linux it needs its own udev rule, see the README): %w", err)
		}
	}
	defer dev.Close()
	defer h.Close()
	_ = h.SetAutoDetachKernelDriver(true)
	if err := h.ClaimInterface(0); err != nil {
		return fmt.Errorf("claim DFU interface: %w", err)
	}

	d := dfu{h}
	if err := d.idle(); err != nil {
		return err
	}
	logf("bdmtoy bootloader ready, writing %d bytes", len(fw))
	for off := 0; off < len(fw); off += dfuBlock {
		blk := fw[off:min(off+dfuBlock, len(fw))]
		if err := d.download(uint16(off/dfuBlock), blk); err != nil {
			return fmt.Errorf("block at %#x: %w", off, err)
		}
		prog(uint32(off + len(blk)))
	}
	// The zero-length block ends the download; the bootloader writes the
	// vector page then (manifest) and is back in dfuIDLE when it is done.
	if err := d.download(uint16((len(fw)+dfuBlock-1)/dfuBlock), nil); err != nil {
		return fmt.Errorf("finishing the download: %w", err)
	}
	// DETACH or a USB reset starts the new app; the device leaves, so the
	// results are moot. Both, since WinUSB cannot reset the port.
	_, _ = h.ControlTransfer(0x21, dfuDetach, 1000, 0, nil, 0, 1000)
	_ = h.ReleaseInterface(0)
	_ = h.ResetDevice()

	t, err := toyWaitApp(8 * time.Second)
	if err != nil {
		return fmt.Errorf("firmware written, but the app did not come back: %w", err)
	}
	major, minor, _ := t.Version()
	t.Close()
	logf("bdmtoy firmware %d.%d running", major, minor)
	return nil
}

// toyEnterBootloader reboots a running app into the bootloader.
func toyEnterBootloader(logf func(string, ...any)) error {
	t, err := openToy(0)
	if err != nil {
		return err
	}
	defer t.Close()
	if t.fw < toyBootFW {
		return fmt.Errorf("bdmtoy firmware %d.%d has no USB bootloader: flash bdmtoy-full.hex over SWD once (see the README)", t.fw>>8, t.fw&0xff)
	}
	logf("bdmtoy running firmware %d.%d, rebooting it into the bootloader", t.fw>>8, t.fw&0xff)
	_, err = t.cmd(toyBootloader)
	return err
}

func toyWaitOpen(ctx *libusb.Context, pid uint16, limit time.Duration) (*libusb.Device, *libusb.DeviceHandle, error) {
	for deadline := time.Now().Add(limit); ; {
		time.Sleep(200 * time.Millisecond)
		dev, h, err := ctx.OpenDeviceWithVendorProduct(toyVID, pid)
		if err == nil {
			return dev, h, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, err
		}
	}
}

func toyWaitApp(limit time.Duration) (*Toy, error) {
	for deadline := time.Now().Add(limit); ; {
		time.Sleep(300 * time.Millisecond)
		t, err := OpenToy()
		if err == nil {
			return t, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
	}
}

// dfu is a DFU 1.1 device in DFU mode, interface 0.
type dfu struct{ h *libusb.DeviceHandle }

// status sends DFU_GETSTATUS: status code, poll timeout, state.
func (d dfu) status() (byte, time.Duration, byte, error) {
	var b [6]byte
	n, err := d.h.ControlTransfer(0xA1, dfuGetStatus, 0, 0, b[:], len(b), 1000)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("DFU status: %w", err)
	}
	if n < 6 {
		return 0, 0, 0, errors.New("DFU status: short reply")
	}
	poll := time.Duration(binary.LittleEndian.Uint32(b[1:5])&0xFFFFFF) * time.Millisecond
	return b[0], poll, b[4], nil
}

// idle gets the device to dfuIDLE: clears an error a broken-off update left,
// aborts a download it was in the middle of.
func (d dfu) idle() error {
	for range 3 {
		_, _, state, err := d.status()
		if err != nil {
			return err
		}
		switch state {
		case dfuStateIdle:
			return nil
		case dfuStateError:
			_, err = d.h.ControlTransfer(0x21, dfuClrStatus, 0, 0, nil, 0, 1000)
		default:
			_, err = d.h.ControlTransfer(0x21, dfuAbort, 0, 0, nil, 0, 1000)
		}
		if err != nil {
			return fmt.Errorf("DFU reset: %w", err)
		}
	}
	return errors.New("DFU device will not go idle")
}

// download sends one block (none: the end of the download) and polls until
// the device has dealt with it.
func (d dfu) download(block uint16, data []byte) error {
	if _, err := d.h.ControlTransfer(0x21, dfuDnload, block, 0, data, len(data), 2000); err != nil {
		return fmt.Errorf("DFU download: %w", err)
	}
	want := byte(dfuStateDnloadIdle)
	if data == nil {
		want = dfuStateIdle
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		st, poll, state, err := d.status()
		if err != nil {
			return err
		}
		if st != 0 || state == dfuStateError {
			return fmt.Errorf("bootloader reports DFU status %d", st)
		}
		if state == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("DFU device stuck in state %d", state)
		}
		time.Sleep(max(poll, 5*time.Millisecond))
	}
}
