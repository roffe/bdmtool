// BDM Tool -- reads and writes SAAB Trionic (and Volvo CEM) ECU flash over
// BDM using a CombiAdapter, USB BDM, USB BDM MkII, ardubdm or bdmtoy. A Go/Fyne replacement
// for Janis Silins' BDM Tool. The tool itself is package bdm; this is the
// standalone window around it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/png"
	"os"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/software"

	"github.com/roffe/bdmtool/bdm"
	"github.com/roffe/bdmtool/icons"
	"github.com/roffe/browse"
)

func main() {
	shot := flag.String("screenshot", "", "write a PNG of the main window to this path and exit")
	flag.Parse()
	a := fyneapp.New()
	meta := a.Metadata()
	title := fmt.Sprintf("BDM tool %s build %d", meta.Version, meta.Build)
	w := a.NewWindow(title)
	ui := bdm.New(&bdm.Config{
		Window: w,
		OpenFile: func(ext string, fn func(string)) {
			pick(w, browse.OpenFile, browse.Options{Title: "Open " + ext[1:] + " file", Filters: filter(ext)}, fn)
		},
		SaveFile: func(name, ext string, fn func(string)) {
			pick(w, browse.SaveFile, browse.Options{Title: "Save " + ext[1:] + " file", Name: name, Filters: filter(ext)}, fn)
		},
		OnExit: w.Close,
	})
	if *shot != "" {
		if err := screenshot(ui, *shot); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	w.SetContent(ui)
	w.SetMainMenu(ui.Menu())
	w.SetIcon(icons.Get("app"))
	w.Resize(fyne.NewSize(470, 500))
	w.SetOnClosed(ui.Disconnect)
	w.ShowAndRun()
}

func filter(ext string) []browse.Filter {
	return []browse.Filter{{Name: ext[1:] + " files", Extensions: []string{ext}}}
}

// pick runs a native file dialog off the UI thread and hands the path to fn.
// The window stays live meanwhile; ops are guarded by UI.busy.
func pick(w fyne.Window, run func(browse.Options) (string, error), o browse.Options, fn func(string)) {
	go func() {
		path, err := run(o)
		fyne.Do(func() {
			switch {
			case err == nil:
				fn(path)
			case !errors.Is(err, browse.ErrCancelled):
				dialog.ShowError(err, w)
			}
		})
	}()
}

// screenshot renders the main window offscreen to a PNG (README material):
// `bdmtool -screenshot screenshot.png`. The GL front-buffer readback in
// Canvas().Capture() comes back blank on Wayland/Xvfb, so use the software
// painter; it skips the menu bar but needs no display at all.
func screenshot(ui fyne.CanvasObject, path string) error {
	c := software.NewCanvas()
	c.SetContent(ui)
	c.Resize(fyne.NewSize(470, 500))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, c.Capture())
}
