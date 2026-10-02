package main

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"github.com/roffe/avrflash"
	"github.com/roffe/bdmtool/firmwares"
)

func (u *UI) combiMenu() *fyne.MenuItem {
	m := fyne.NewMenuItem("CombiAdapter", nil)
	m.ChildMenu = fyne.NewMenu("",
		fyne.NewMenuItem("1.1 (legacy)", func() { u.uploadCombi("1.1 (legacy)", firmwares.CombiAdapter111Bin) }),
		fyne.NewMenuItem("2.0 (latest)", func() { u.uploadCombi("2.0 (latest)", firmwares.CombiAdapterBin) }),
	)
	return m
}

// uploadCombi flashes fw to the CombiAdapter over its USB bootloader
// (combiflash.go).
func (u *UI) uploadCombi(name string, fw []byte) {
	dialog.ShowConfirm("CombiAdapter firmware",
		"Flash the "+name+" firmware to the CombiAdapter?",
		func(ok bool) {
			if !ok || u.busy.Load() {
				return
			}
			u.disconnect() // the bootloader needs the USB interface we hold
			u.work("Upload CombiAdapter firmware", uint32(len(fw)), func(p progressFn) error {
				return flashCombi(fw, u.logf, p)
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
