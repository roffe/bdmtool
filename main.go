// BDM Tool -- reads and writes SAAB Trionic (and Volvo CEM) ECU flash over
// BDM using a CombiAdapter, USB BDM, USB BDM MkII or ardubdm. A Go/Fyne replacement
// for Janis Silins' BDM Tool.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"embed"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/software"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	sq "github.com/sqweek/dialog"
)

// Button artwork lifted out of the original bdmtool.exe -- the 22x22 PNGs sit
// verbatim in its .NET resource stream.
//
//go:embed icons
var iconFS embed.FS

func icon(name string) fyne.Resource {
	b, err := iconFS.ReadFile("icons/" + name + ".png")
	if err != nil {
		panic(err) // embedded at build time; missing means a broken build
	}
	return fyne.NewStaticResource(name, b)
}

// Adapter is the BDM transport the UI drives. The three adapters the stock
// BDM Tool supported speak three different protocols; see combi.go and
// usbbdm.go.
type Adapter interface {
	Close()
	Version() (major, minor byte, err error)
	Stop() error
	Reset() error
	ReadFlash(e *ECU, w io.Writer, prog progressFn) error
	EraseFlash(e *ECU, prog progressFn) error
	WriteFlash(e *ECU, bin []byte, erase bool, prog progressFn) error
	ReadSRAM(e *ECU, w io.Writer, prog progressFn) error
	WriteSRAM(e *ECU, snap []byte, prog progressFn) error
}

type adapterDef struct {
	name string
	open func() (Adapter, error)
}

var adapters = []adapterDef{
	{"CombiAdapter", opener(Open)},
	{"USB BDM", opener(OpenBDM)},
	{"USB BDM MkII", opener(OpenBDM2)},
}

// One ardubdm entry per USB serial port present at startup.
func init() {
	for _, p := range arduPorts() {
		adapters = append(adapters, adapterDef{"ardubdm on " + p,
			func() (Adapter, error) { return OpenArdu(p) }})
	}
}

func opener[T Adapter](open func() (T, error)) func() (Adapter, error) {
	return func() (Adapter, error) {
		a, err := open()
		if err != nil {
			return nil, err
		}
		return a, nil
	}
}

type UI struct {
	win  fyne.Window
	c    Adapter
	busy atomic.Bool

	adapter    *canvas.Text
	adapterSel *widget.Select
	status     *canvas.Text
	prog       *widget.ProgressBar
	ecuSel     *widget.Select
	erase      *widget.Check
	verify     *widget.Check
	connectBtn *widget.Button
	ops        []*widget.Button

	logText   strings.Builder
	logLabel  *widget.Label
	logScroll *container.Scroll
}

func main() {
	shot := flag.String("screenshot", "", "write a PNG of the main window to this path and exit")
	flag.Parse()
	a := fyneapp.New()
	meta := a.Metadata()
	title := fmt.Sprintf("BDM tool %s build %d", meta.Version, meta.Build)
	w := a.NewWindow(title)
	ui := &UI{win: w}
	w.SetContent(ui.build())
	w.SetMainMenu(ui.menu())
	w.SetIcon(icon("app"))
	w.Resize(fyne.NewSize(470, 500))
	w.SetOnClosed(ui.disconnect)
	if *shot != "" {
		if err := screenshot(ui, *shot); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	w.ShowAndRun()
}

// screenshot renders the main window offscreen to a PNG (README material):
// `bdmtool -screenshot screenshot.png`. The GL front-buffer readback in
// Canvas().Capture() comes back blank on Wayland/Xvfb, so use the software
// painter; it skips the menu bar but needs no display at all.
func screenshot(ui *UI, path string) error {
	c := software.NewCanvas()
	c.SetContent(ui.build())
	c.Resize(fyne.NewSize(470, 500))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, c.Capture())
}

func (u *UI) menu() *fyne.MainMenu {
	item := func(label string, action func()) *fyne.MenuItem {
		return fyne.NewMenuItem(label, action)
	}
	flash := fyne.NewMenuItem("Flash memory", nil)
	flash.ChildMenu = fyne.NewMenu("",
		item("Read...", u.readFlash), item("Erase", u.eraseFlash), item("Write...", u.writeFlash))
	sram := fyne.NewMenuItem("SRAM", nil)
	sram.ChildMenu = fyne.NewMenu("", item("Read...", u.readSRAM), item("Write...", u.writeSRAM))

	return fyne.NewMainMenu(
		fyne.NewMenu("File",
			item("Connect", u.toggleConnect),
			fyne.NewMenuItemSeparator(),
			flash, sram,
			fyne.NewMenuItemSeparator(),
			item("Reset", u.resetMCU),
			item("Stop", u.stopMCU),
		),
		fyne.NewMenu("Help", item("About BDM Tool...", u.about)),
	)
}

func (u *UI) build() fyne.CanvasObject {
	u.adapter = canvas.NewText("Disconnected", theme.Color(theme.ColorNamePrimary))
	u.adapter.TextStyle = fyne.TextStyle{Bold: true}
	u.status = canvas.NewText("Idle", theme.Color(theme.ColorNamePrimary))
	u.prog = widget.NewProgressBar()
	// Blank instead of "0%" while idle. The bar always keeps its slot in the
	// status strip: hiding it changed the content's minimum height, which
	// forced a window resize and cost us the menu bar.
	u.prog.TextFormatter = func() string {
		if u.prog.Value == 0 {
			return ""
		}
		return fmt.Sprintf("%.0f%%", u.prog.Value*100)
	}
	u.connectBtn = widget.NewButton("Connect", u.toggleConnect)

	adapterNames := make([]string, len(adapters))
	for i, a := range adapters {
		adapterNames[i] = a.name
	}
	prefs := fyne.CurrentApp().Preferences()
	u.adapterSel = widget.NewSelect(adapterNames, func(name string) {
		prefs.SetString("adapter", name)
	})
	// Restore last used adapter; SetSelected ignores names not in the list
	// (e.g. an ardubdm port that is gone), so fall back to the first.
	u.adapterSel.SetSelected(prefs.String("adapter"))
	if u.adapterSel.SelectedIndex() < 0 {
		u.adapterSel.SetSelectedIndex(0)
	}

	names := make([]string, len(ECUs))
	for i, e := range ECUs {
		names[i] = e.Name
	}
	u.ecuSel = widget.NewSelect(names, func(name string) {
		prefs.SetString("ecu", name)
	})
	u.ecuSel.PlaceHolder = " "
	u.ecuSel.SetSelected(prefs.String("ecu"))

	u.erase = widget.NewCheck("Erase before writing", nil)
	u.erase.SetChecked(true)
	u.verify = widget.NewCheck("Verify after writing", nil)

	u.logLabel = widget.NewLabel("")
	u.logLabel.Selectable = true
	u.logLabel.TextStyle = fyne.TextStyle{Monospace: true}
	// Wrap, and scroll both ways: a vertical-only scroll adopts its content's
	// width as a minimum, so one long log line would widen the whole window.
	u.logLabel.Wrapping = fyne.TextWrapWord
	u.logScroll = container.NewScroll(u.logLabel)
	u.logScroll.SetMinSize(fyne.NewSize(0, 70))

	flash := group("Flash memory", container.NewGridWithColumns(3,
		u.opButton("Read", "readflash", u.readFlash),
		u.opButton("Erase", "eraseflash", u.eraseFlash),
		u.opButton("Write", "writeflash", u.writeFlash),
	))
	sram := group("SRAM", container.NewGridWithColumns(2,
		u.opButton("Read", "readsram", u.readSRAM),
		u.opButton("Write", "writesram", u.writeSRAM),
	))
	mcu := group("MCU control", container.NewGridWithColumns(2,
		u.opButton("Reset", "reset", u.resetMCU),
		u.opButton("Stop", "stop", u.stopMCU),
	))

	ctrl := group("", container.NewGridWithRows(1,
		widget.NewButton("Exit", func() { u.win.Close() }),
	))

	u.setOps(false)

	head := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Adapter:"), u.connectBtn, u.adapterSel),
		container.NewPadded(u.adapter),
		widget.NewSeparator(),
		container.NewBorder(nil, nil, widget.NewLabel("ECU type:"), nil, u.ecuSel),
		container.NewGridWithColumns(2, u.erase, u.verify),
		container.NewGridWithColumns(2, flash, sram),
		// The right cell mirrors the group box's own padding + title row so the
		// Exit button lines up with the MCU buttons beside it.
		container.NewGridWithColumns(2, mcu, ctrl),
	)
	foot := container.NewVBox(widget.NewSeparator(),
		container.NewBorder(nil, u.status, nil, nil, u.prog))
	// Padded so the group boxes keep the same margin as the rows above them;
	// widget.Card draws edge to edge otherwise.
	return container.NewBorder(head, foot, nil, nil, u.logScroll)
}

// group draws a titled panel, the closest Fyne gets to a Win32 group box.
func group(title string, content fyne.CanvasObject) fyne.CanvasObject {
	return widget.NewCard("", "", container.NewVBox(
		widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		content))
}

func (u *UI) about() {
	dialog.ShowCustom("About BDM Tool", "Close", container.NewVBox(
		widget.NewLabelWithStyle("BDM Tool", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Go/Fyne port by Joakim \"Roffe\" Karlsson."),
		widget.NewLabel("Original BDM Tool v2.13 by Janis Silins, 2009-2016."),
		widget.NewLabel("Portions of code from BDM v0.90, Scott Howard, 1992."),
		widget.NewLabel("For non-commercial use only."),
	), u.win)
}

func (u *UI) resetMCU() {
	u.run("Reset MCU", 1, func(progressFn) error { return u.c.Reset() })
}

func (u *UI) stopMCU() {
	u.run("Stop MCU", 1, func(progressFn) error { return u.c.Stop() })
}

func (u *UI) opButton(label, iconName string, tapped func()) *widget.Button {
	b := widget.NewButtonWithIcon(label, icon(iconName), tapped)
	u.ops = append(u.ops, b)
	return b
}

func (u *UI) setOps(enabled bool) {
	for _, b := range u.ops {
		if enabled {
			b.Enable()
		} else {
			b.Disable()
		}
	}
}

// =====================
// connection
// =====================

func (u *UI) toggleConnect() {
	if u.c != nil {
		u.disconnect()
		return
	}
	if !u.busy.CompareAndSwap(false, true) {
		return
	}
	sel := max(u.adapterSel.SelectedIndex(), 0)
	u.connectBtn.Disable()
	setText(u.adapter, "Connecting...")
	// Off the UI thread: opening probes the hardware and can take seconds.
	go func() {
		c, err := adapters[sel].open()
		var major, minor byte
		var verr error
		if err == nil {
			major, minor, verr = c.Version()
		}
		fyne.Do(func() {
			u.busy.Store(false)
			u.connectBtn.Enable()
			if err != nil {
				setText(u.adapter, "Disconnected")
				dialog.ShowError(err, u.win)
				u.logf("%v", err)
				return
			}
			u.c = c
			debugLog = u.logf
			if verr != nil {
				u.logf("firmware version unavailable: %v", verr)
				setText(u.adapter, "Connected")
			} else {
				setText(u.adapter, fmt.Sprintf("Connected (firmware v%d.%d)", major, minor))
			}
			u.connectBtn.SetText("Disconnect")
			u.adapterSel.Disable()
			u.setOps(true)
			u.logf("%s connected", adapters[sel].name)
		})
	}()
}

func (u *UI) disconnect() {
	if u.c == nil {
		return
	}
	u.c.Close()
	u.c = nil
	setText(u.adapter, "Disconnected")
	u.connectBtn.SetText("Connect")
	u.adapterSel.Enable()
	u.setOps(false)
	u.logf("Disconnected")
}

// =====================
// operations
// =====================

func (u *UI) ecu() *ECU {
	i := u.ecuSel.SelectedIndex()
	if i < 0 {
		dialog.ShowInformation("BDM Tool", "ECU type not selected", u.win)
		return nil
	}
	return &ECUs[i]
}

// run executes fn on a worker goroutine, feeding the progress bar and
// re-enabling the buttons when it finishes.
func (u *UI) run(name string, total uint32, fn func(progressFn) error) {
	if u.c == nil || !u.busy.CompareAndSwap(false, true) {
		return
	}
	//u.setOps(false)
	setText(u.status, name+"...")
	u.prog.SetValue(0)

	step := max(total/100, 1)
	start := time.Now()
	go func() {
		last := uint32(0)
		err := fn(func(done uint32) {
			if done-last < step && done < total {
				return
			}
			last = done
			v := float64(done) / float64(total)
			fyne.Do(func() { u.prog.SetValue(v) })
		})
		fyne.Do(func() {
			u.busy.Store(false)
			u.setOps(u.c != nil)
			u.prog.SetValue(0)
			if err != nil {
				setText(u.status, "Failed")
				u.logf("%s failed: %v", name, err)
				dialog.ShowError(err, u.win)
				return
			}
			setText(u.status, "Idle")
			u.logf("%s completed in %s", name, time.Since(start).Round(time.Second))
		})
	}()
}

func (u *UI) readFlash() {
	e := u.ecu()
	if e == nil {
		return
	}
	u.save("flash.bin", ".bin", func(wc io.WriteCloser) {
		u.run("Read flash", e.FlashSize, func(p progressFn) error {
			defer wc.Close()
			bw := bufio.NewWriter(wc)
			if err := u.c.ReadFlash(e, bw, p); err != nil {
				return err
			}
			return bw.Flush()
		})
	})
}

func (u *UI) readSRAM() {
	e := u.ecu()
	if e == nil {
		return
	}
	u.save("sram.ram", ".ram", func(wc io.WriteCloser) {
		u.run("Read SRAM", e.SRAMSize, func(p progressFn) error {
			defer wc.Close()
			bw := bufio.NewWriter(wc)
			if err := u.c.ReadSRAM(e, bw, p); err != nil {
				return err
			}
			return bw.Flush()
		})
	})
}

func (u *UI) writeSRAM() {
	e := u.ecu()
	if e == nil {
		return
	}
	u.open(".ram", func(snap []byte) {
		u.run("Write SRAM", e.SRAMSize, func(p progressFn) error {
			return u.c.WriteSRAM(e, snap, p)
		})
	})
}

func (u *UI) eraseFlash() {
	e := u.ecu()
	if e == nil {
		return
	}
	dialog.ShowConfirm("Erase flash",
		"Current contents of flash memory will be lost!\nDo you want to continue?",
		func(ok bool) {
			if ok {
				u.run("Erase flash", e.FlashSize, func(p progressFn) error {
					return u.c.EraseFlash(e, p)
				})
			}
		}, u.win)
}

func (u *UI) writeFlash() {
	e := u.ecu()
	if e == nil {
		return
	}
	u.open(".bin", func(bin []byte) {
		if uint32(len(bin)) != e.FlashSize {
			dialog.ShowError(fmt.Errorf("file is %d bytes, %s flash is %d",
				len(bin), e.Name, e.FlashSize), u.win)
			return
		}
		dialog.ShowConfirm("Write flash",
			"Current contents of flash memory will be lost!\nDo you want to continue?",
			func(ok bool) {
				if !ok {
					return
				}
				erase, verify := u.erase.Checked, u.verify.Checked
				u.run("Write flash", e.FlashSize, func(p progressFn) error {
					if err := u.c.WriteFlash(e, bin, erase, p); err != nil {
						return err
					}
					if !verify {
						return nil
					}
					u.logf("Verifying...")
					var buf bytes.Buffer
					if err := u.c.ReadFlash(e, &buf, p); err != nil {
						return err
					}
					if !bytes.Equal(buf.Bytes(), bin) {
						return errors.New("verify failed: flash contents differ from file")
					}
					return nil
				})
			}, u.win)
	})
}

// pick runs a native file dialog. sqweek's GTK backend initialises GTK on the
// main thread at startup and segfaults if called from anywhere else, so the
// call is marshalled back via DoAndWait -- the UI freezes while it is up.
func pick(ext string, run func(*sq.FileBuilder) (string, error)) (string, error) {
	var path string
	var err error
	fyne.DoAndWait(func() {
		path, err = run(sq.File().Filter(ext[1:]+" files", ext[1:]))
	})
	return path, err
}

// open asks for a source file and hands its contents to fn.
func (u *UI) open(ext string, fn func([]byte)) {
	go func() {
		name, err := pick(ext, (*sq.FileBuilder).Load)
		if err != nil {
			if err != sq.ErrCancelled {
				fyne.Do(func() { dialog.ShowError(err, u.win) })
			}
			return
		}
		data, err := os.ReadFile(name)
		if err != nil {
			fyne.Do(func() { dialog.ShowError(err, u.win) })
			return
		}
		fyne.Do(func() { fn(data) })
	}()
}

// save asks for a target file and hands the writer to fn.
func (u *UI) save(name, ext string, fn func(io.WriteCloser)) {
	go func() {
		path, err := pick(ext, func(b *sq.FileBuilder) (string, error) {
			return b.SetStartFile(name).Save()
		})
		if err != nil {
			if err != sq.ErrCancelled {
				fyne.Do(func() { dialog.ShowError(err, u.win) })
			}
			return
		}
		if !strings.HasSuffix(strings.ToLower(path), ext) {
			path += ext
		}
		f, err := os.Create(path)
		if err != nil {
			fyne.Do(func() { dialog.ShowError(err, u.win) })
			return
		}
		fyne.Do(func() { fn(f) })
	}()
}

func (u *UI) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	write := func() {
		if u.logText.Len() > 0 {
			u.logText.WriteString("\n")
		}
		u.logText.WriteString(line)
		u.logLabel.SetText(u.logText.String())
		u.logScroll.ScrollToBottom()
	}
	if fyne.CurrentApp() != nil && u.busy.Load() {
		fyne.Do(write)
		return
	}
	write()
}

func setText(t *canvas.Text, s string) {
	t.Text = s
	t.Refresh()
}
