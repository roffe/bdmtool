package firmwares

import _ "embed"

//go:embed ardubdm.hex
var ArduBDMHex []byte

//go:embed combiadapter.bin
var CombiAdapterBin []byte

//go:embed combiadapter-1.1.bin
var CombiAdapter111Bin []byte

// CombiAdapter bootloader 2.0's installer: an app that replaces the
// bootloader with the copy it carries
//
//go:embed combi-bootloader-installer.bin
var CombiBootInstallerBin []byte

// The same installer carrying the original 1.0 bootloader, to go back
//
//go:embed combi-bootloader-1.0-installer.bin
var CombiBoot10InstallerBin []byte

// The bdmtoy app (2.0+), for its USB DFU bootloader
//
//go:embed bdmtoy.bin
var BdmtoyBin []byte
