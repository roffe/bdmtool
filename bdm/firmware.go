package bdm

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"github.com/roffe/avrflash"
	"github.com/roffe/bdmtool/v3/firmwares"
)

func (u *UI) ardubdmMenu() *fyne.MenuItem {
	m := fyne.NewMenuItem("ArduBDM", nil)
	m.ChildMenu = fyne.NewMenu("",
		fyne.NewMenuItem("1.3 (latest)", u.uploadArdubdm),
	)
	return m
}

func (u *UI) combiMenu() *fyne.MenuItem {
	m := fyne.NewMenuItem("CombiAdapter", nil)
	m.ChildMenu = fyne.NewMenu("",
		fyne.NewMenuItem("2.5 (latest)", func() { u.uploadCombi("2.5 (latest)", firmwares.CombiAdapterBin) }),
		fyne.NewMenuItem("Bootloader 2.1 + 2.5 (latest)", func() {
			u.uploadCombiBoot("2.1", 200, firmwares.CombiBootInstallerBin, "2.5 (latest)", firmwares.CombiAdapterBin)
		}),
		fyne.NewMenuItem("Bootloader 1.0 (original) + 2.5", func() {
			u.uploadCombiBoot("1.0", 4, firmwares.CombiBoot10InstallerBin, "2.5 (latest)", firmwares.CombiAdapterBin)
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("1.1 (legacy)", func() { u.uploadCombi("1.1 (legacy)", firmwares.CombiAdapter111Bin) }),
		fyne.NewMenuItem("Bootloader 1.0 (original) + 1.1 (legacy)", func() {
			u.uploadCombiBoot("1.0", 4, firmwares.CombiBoot10InstallerBin, "1.1 (legacy)", firmwares.CombiAdapter111Bin)
		}),
	)
	return m
}

func (u *UI) bdmtoyMenu() *fyne.MenuItem {
	m := fyne.NewMenuItem("bdmtoy", nil)
	m.ChildMenu = fyne.NewMenu("",
		fyne.NewMenuItem("2.2 (latest)", func() { u.uploadToy("2.2 (latest)", firmwares.BdmtoyBin) }),
	)
	return m
}

// uploadToy flashes fw to a bdmtoy over its USB DFU bootloader (toyflash.go).
func (u *UI) uploadToy(name string, fw []byte) {
	dialog.ShowConfirm("bdmtoy firmware",
		"Flash the "+name+" firmware to the bdmtoy?\nIt needs firmware 2.0 or later on it already (see the README).",
		func(ok bool) {
			if !ok || u.busy.Load() {
				return
			}
			u.Disconnect() // the update needs the USB interface we hold
			u.work("Upload bdmtoy firmware", uint32(len(fw)), func(p progressFn) error {
				return flashToy(fw, u.logf, p)
			})
		}, u.win)
}

// uploadCombi flashes fw to the CombiAdapter over its USB bootloader
// (combiflash.go).
func (u *UI) uploadCombi(name string, fw []byte) {
	dialog.ShowConfirm("CombiAdapter firmware",
		"Flash the "+name+" firmware to the CombiAdapter?\nWith firmware older than 2.0, unplug and replug the adapter when the log asks.",
		func(ok bool) {
			if !ok || u.busy.Load() {
				return
			}
			u.Disconnect() // the bootloader needs the USB interface we hold
			u.work("Upload CombiAdapter firmware", uint32(len(fw)), func(p progressFn) error {
				return flashCombi(name, fw, u.logf, p)
			})
		}, u.win)
}

// uploadCombiBoot installs bootloader ver on the CombiAdapter with its
// installer inst, then fw (installCombiBoot).
func (u *UI) uploadCombiBoot(ver string, block int, inst []byte, name string, fw []byte) {
	dialog.ShowConfirm("CombiAdapter bootloader",
		"Install bootloader "+ver+" on the CombiAdapter, then the "+name+" firmware?\n"+
			"With firmware older than 2.0, unplug and replug the adapter when the log asks.\n\n"+
			"Keep the adapter plugged in until this is done: if it loses power while\n"+
			"the bootloader is written (about half a second), it can only be\n"+
			"recovered over SWD or the LPC17xx ROM ISP.",
		func(ok bool) {
			if !ok || u.busy.Load() {
				return
			}
			u.Disconnect() // the bootloader needs the USB interface we hold
			u.work("Install CombiAdapter bootloader", uint32(len(inst)+len(fw)), func(p progressFn) error {
				status := func(s string) { fyne.Do(func() { setText(u.status, s+"...") }) }
				return installCombiBoot(ver, block, inst, name, fw, u.logf, status, p)
			})
		}, u.win)
}

// CombiDumpWindow (Ctrl+D in bdmtool) saves the CombiAdapter's own flash, e.g. a V1's
// bootloader, with firmware 1.4 or 2.4+ (Combi.readSelf). Progress and the
// log show in the main window.
func (u *UI) CombiDumpWindow() {
	w := fyne.CurrentApp().NewWindow("CombiAdapter flash dump")
	start, length := widget.NewEntry(), widget.NewEntry()
	start.SetText("0x0")
	length.SetText("0x4000")
	dump := widget.NewButton("Dump...", func() {
		addr, err1 := strconv.ParseUint(start.Text, 0, 32)
		n, err2 := strconv.ParseUint(length.Text, 0, 32)
		if err1 != nil || err2 != nil || n == 0 {
			dialog.ShowError(fmt.Errorf("start and length must be numbers, e.g. 0x0 and 0x4000"), w)
			return
		}
		u.save("combi-flash.bin", ".bin", func(wc io.WriteCloser) {
			if u.busy.Load() {
				wc.Close()
				return
			}
			u.Disconnect() // the dump opens the adapter itself
			u.work("Dump CombiAdapter flash", uint32(n), func(p progressFn) error {
				defer wc.Close()
				c, err := Open()
				if err != nil {
					return err
				}
				defer c.Close()
				bw := bufio.NewWriter(wc)
				if err := c.readSelf(uint32(addr), uint32(n), bw, p); err != nil {
					return err
				}
				return bw.Flush()
			})
		})
	})
	w.SetContent(container.NewVBox(
		widget.NewForm(widget.NewFormItem("Start", start), widget.NewFormItem("Length", length)),
		widget.NewLabel("Bootloader: 0x0, 0x4000\nWhole flash: 0x0, 0x20000 on a V1, 0x40000 on a V2"),
		dump,
	))
	w.Show()
}

func (u *UI) uploadArdubdm() {
	// get last word from the selected adapter name
	parts := strings.Fields(u.adapterSel.Selected)
	lastWord := parts[len(parts)-1]
	// busy makes logf marshal onto the UI thread from the worker goroutine.
	if !u.busy.CompareAndSwap(false, true) {
		return
	}
	u.connectBtn.Disable()
	go func() {
		err := avrflash.Update(lastWord, 115200, firmwares.ArduBDMHex, u.logf)
		fyne.Do(func() {
			u.busy.Store(false)
			u.connectBtn.Enable()
			if err != nil {
				u.logf("%v", err)
			}
		})
	}()
}
