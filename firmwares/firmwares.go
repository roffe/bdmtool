package firmwares

import _ "embed"

//go:embed ardubdm.hex
var ArduBDMHex []byte

//go:embed combiadapter.bin
var CombiAdapterBin []byte

//go:embed combiadapter-1.1.bin
var CombiAdapter111Bin []byte
