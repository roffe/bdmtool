package bdm

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"github.com/roffe/avrflash"
	"github.com/roffe/bdmtool/firmwares"
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
		fyne.NewMenuItem("2.2 (latest)", func() { u.uploadCombi("2.2 (latest)", firmwares.CombiAdapterBin) }),
		fyne.NewMenuItem("Bootloader 2.0 + 2.2 (latest)", func() {
			u.uploadCombiBoot("2.0", 200, firmwares.CombiBootInstallerBin, "2.2 (latest)", firmwares.CombiAdapterBin)
		}),
		fyne.NewMenuItem("Bootloader 1.0 (original) + 2.2", func() {
			u.uploadCombiBoot("1.0", 4, firmwares.CombiBoot10InstallerBin, "2.2 (latest)", firmwares.CombiAdapterBin)
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
				return flashCombi(fw, u.logf, p)
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
				return installCombiBoot(ver, block, inst, fw, u.logf, p)
			})
		}, u.win)
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
