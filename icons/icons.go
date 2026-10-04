// Package icons holds the button artwork lifted out of the original
// bdmtool.exe -- the 22x22 PNGs sit verbatim in its .NET resource stream.
package icons

import (
	"embed"

	"fyne.io/fyne/v2"
)

//go:embed *.png
var fs embed.FS

// Get returns the named icon, e.g. "app" or "readflash".
func Get(name string) fyne.Resource {
	b, err := fs.ReadFile(name + ".png")
	if err != nil {
		panic(err) // embedded at build time; missing means a broken build
	}
	return fyne.NewStaticResource(name, b)
}
