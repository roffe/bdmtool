package main

// ECU descriptors and the per-ECU register setup ("prepare") sequences,
// transcribed from the CombiAdapter .NET driver (caAdapterBase::ECUDescriptors
// and caAdapterBase::prepare_ecu). The writes configure the MCU's chip
// selects, bus timing and watchdog so external flash is addressable while the
// core sits in background mode.

type memWrite struct {
	addr uint32
	val  uint32
	size int // bytes: 1, 2 or 4
}

type ECU struct {
	Name          string
	FlashType     string // exactly 6 chars; sent to the adapter's flash driver
	FlashAddr     uint32
	FlashSize     uint32
	SRAMAddr      uint32
	SRAMSize      uint32
	EraseFeedback bool // 28Fxxx chips report per-word erase progress
	Prepare       []memWrite
	// Where the CPU32 flash driver runs and what maps that RAM. The MCU's
	// internal RAM is used, not the ECU's SRAM: it needs no chip selects, so
	// it works after a reset into BDM when the ECU's own setup never ran.
	DrvAddr uint32
	DrvPrep []memWrite
}

// 68332 standby RAM (2 KB) mapped at 0x100000 via RAMBAR, as Just4Trionic does.
var ram332 = []memWrite{w16(0xfffb04, 0x1000)}

// 68377 DPTRAM (6 KB) mapped at 0x100000, per Just4Trionic's T8 prep.
var ram377 = []memWrite{w16(0xfff684, 0x1000)}

// words expands a run of consecutive 16-bit writes starting at addr.
func words(addr uint32, vals ...uint32) []memWrite {
	out := make([]memWrite, len(vals))
	for i, v := range vals {
		out[i] = memWrite{addr + uint32(2*i), v, 2}
	}
	return out
}

func w16(addr, val uint32) memWrite { return memWrite{addr, val, 2} }
func w8(addr, val uint32) memWrite  { return memWrite{addr, val, 1} }

// Trionic 5.2, 5.5 (28F010 chips) and Volvo CEM share this setup.
var prepT5 = []memWrite{
	w16(0xfffa04, 0x7f00), w16(0xfffa21, 0x0000), w16(0xfffa44, 0x3fff),
	w16(0xfffa48, 0x0405), w16(0xfffa4a, 0x6b70), w16(0xfffa50, 0x0405),
	w16(0xfffa52, 0x3370), w16(0xfffa54, 0x0405), w16(0xfffa56, 0x5370),
	w16(0xfffc14, 0x0040), w16(0xfffc17, 0x0040),
}

var prepT55New = []memWrite{
	w16(0xfffa20, 0x000c), w16(0xfffa00, 0x40cf), w16(0xfffa04, 0x7f00),
	w16(0xfffa21, 0x0000), w16(0xfffa44, 0x3fff), w16(0xfffa4a, 0x6b70),
	w16(0xfffa50, 0x0005), w16(0xfffa52, 0x3370), w16(0xfffa54, 0x0005),
	w16(0xfffa56, 0x5370),
}

var prepT7 = concat(
	[]memWrite{
		w16(0xfffa00, 0x61c2), w16(0xfffa04, 0x7f08), w16(0xfffa11, 0x0000),
		w16(0xfffa13, 0x0000), w16(0xfffa15, 0x00fe), w16(0xfffa17, 0x0011),
		w16(0xfffa19, 0x007b), w16(0xfffa1b, 0x007b), w16(0xfffa1d, 0x0085),
		w16(0xfffa1f, 0x0008), w16(0xfffa22, 0x0250), w16(0xfffa24, 0x0129),
		w16(0xfffa27, 0x0000), w16(0xfffa41, 0x000f), w16(0xfffa44, 0x2fff),
		w16(0xfffa46, 0x0001), w16(0xfffa17, 0x0011), w16(0xfffa48, 0x0006),
	},
	words(0xfffa4c,
		0xf003, 0x6830, 0x0006, 0x1030, 0x0000, 0x0000, 0xf003, 0x5030,
		0xf003, 0x3030, 0xff00, 0x7bf0, 0x0000, 0x0000, 0x0000, 0x0000,
		0x0000, 0x0000, 0xfff8, 0x2bc7, 0x0000, 0x0000, 0x0000, 0x0000,
		0x0000, 0x0000),
	[]memWrite{w16(0xfffa4a, 0x6bb0)},
	words(0xfffa4e,
		0x6830, 0x0006, 0x1030, 0x0000, 0x0000, 0xf003, 0x5030, 0xf003,
		0x3030, 0xff00, 0x7bf0, 0x0000, 0x0000, 0x0000, 0x0000, 0x0000,
		0x0000, 0xfff8, 0x2bc7),
	[]memWrite{
		w16(0xfffa04, 0x7f00), w8(0xfffa21, 0x00), w16(0xfffa4a, 0x6b70),
		w16(0xfffa50, 0x0007), w16(0xfffa52, 0x3370),
	},
)

var ECUs = []ECU{
	{"Trionic 5.2", "28f010", 0x60000, 0x20000, 0x0, 0x8000, true, prepT5, 0x100000, ram332},
	{"Trionic 5.5 (28F010 chips)", "28f010", 0x40000, 0x40000, 0x0, 0x8000, true, prepT5, 0x100000, ram332},
	{"Trionic 5.5 (AM29F010 chips)", "29f010", 0x40000, 0x40000, 0x0, 0x8000, false, prepT55New, 0x100000, ram332},
	{"Trionic 7", "29f400", 0x0, 0x80000, 0xf00000, 0xffff, false, prepT7, 0x100000, ram332},
	{"Trionic 8", "29f400", 0x0, 0x100000, 0xf00000, 0xffff, false, []memWrite{w16(0xfffa50, 0x0000)}, 0x100000, ram377},
	{"Volvo CEM", "28f400", 0x0, 0x80000, 0xf00000, 0xffff, false, prepT5, 0, nil},
}

func concat[T any](s ...[]T) []T {
	var out []T
	for _, v := range s {
		out = append(out, v...)
	}
	return out
}
