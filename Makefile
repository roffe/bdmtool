.PHONY: bdmtool appimage clean run firmwares

VERSION=$(shell sed -n 's/^Version = "\(.*\)"/\1/p' FyneApp.toml)
APPIMAGETOOL=.tmp/appimagetool

default: bdmtool

firmwares/ardubdm.hex: $(HOME)/Documents/PlatformIO/Projects/ardubdm/.pio/build/ATmega328PB/firmware.hex
	cp $< $@

firmwares/combiadapter.bin: $(HOME)/devel/CombiAdapter2/firmware/combi-firmware-2.2.bin
	cp $< $@

firmwares/combi-bootloader-installer.bin: $(HOME)/devel/CombiAdapter2/bootloader/combi-bootloader-2.0-installer.bin
	cp $< $@

firmwares/combi-bootloader-1.0-installer.bin: $(HOME)/devel/CombiAdapter2/bootloader/combi-bootloader-1.0-installer.bin
	cp $< $@

firmwares/bdmtoy.bin: $(HOME)/OneDrive/devel/bdmtoy/firmware/bin/firmware.bin
	cp $< $@

firmwares: firmwares/ardubdm.hex firmwares/combiadapter.bin firmwares/combi-bootloader-installer.bin firmwares/combi-bootloader-1.0-installer.bin firmwares/bdmtoy.bin

run: firmwares
	go run -tags=wayland .

bdmtool.exe:
	CGO_CFLAGS="-I/home/roffe/go/src/github.com/roffe/txlogger/vcpkg/packages/libusb_x64-windows/include/libusb-1.0" \
      CGO_LDFLAGS="-L/home/roffe/go/src/github.com/roffe/txlogger/vcpkg/packages/libusb_x64-windows/lib" \
      CGO_ENABLED=1 \
      CC=x86_64-w64-mingw32-gcc \
      GOARCH=amd64 \
      GOOS=windows \
      fyne package --os windows --release

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
