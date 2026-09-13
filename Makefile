.PHONY: bdmtool appimage clean

VERSION=$(shell sed -n 's/^Version = "\(.*\)"/\1/p' FyneApp.toml)
APPIMAGETOOL=.tmp/appimagetool

default: bdmtool

bdmtool:
	go build -ldflags '-s -w' -o bdmtool .

clean:
	rm -rf bdmtool BDMTool-x86_64.AppImage .tmp/AppDir

$(APPIMAGETOOL):
	@mkdir -p .tmp
	curl -fsSL -o $@ https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage
	chmod +x $@

# ponytail: no bundled .so files; libusb-1.0 and gtk3 ship with every desktop distro
appimage: bdmtool $(APPIMAGETOOL)
	rm -rf .tmp/AppDir
	mkdir -p .tmp/AppDir/usr/bin .tmp/AppDir/usr/share/icons
	cp bdmtool .tmp/AppDir/usr/bin/
	cp icons/app.png .tmp/AppDir/bdmtool.png
	cp icons/app.png .tmp/AppDir/usr/share/icons/bdmtool.png
	cp icons/app.png .tmp/AppDir/.DirIcon
	printf '#!/bin/sh\nexec "$$(dirname "$$(readlink -f "$$0")")/usr/bin/bdmtool" "$$@"\n' > .tmp/AppDir/AppRun
	chmod +x .tmp/AppDir/AppRun
	printf '[Desktop Entry]\nType=Application\nName=BDMTool\nExec=bdmtool\nIcon=bdmtool\nCategories=Utility;\n' > .tmp/AppDir/bdmtool.desktop
	ARCH=x86_64 VERSION=$(VERSION) $(APPIMAGETOOL) --appimage-extract-and-run --no-appstream .tmp/AppDir BDMTool-x86_64.AppImage
