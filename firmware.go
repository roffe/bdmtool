package main

import (
	"log"
	"strings"

	"fyne.io/fyne/v2"
	"github.com/roffe/avrflash"
	"github.com/roffe/bdmtool/firmwares"
)

func (u *UI) uploadArdubdm() {
	// get last word from the selected adapter name
	parts := strings.Fields(u.adapterSel.Selected)
	lastWord := parts[len(parts)-1]
	log.Println(lastWord)
	u.connectBtn.Disable()
	go func() {
		defer fyne.Do(u.connectBtn.Enable)
		err := avrflash.Update(lastWord, 115200, firmwares.ArduBDMHex, u.logf)
		if err != nil {
			u.logf("%v", err)
		}
	}()
}
